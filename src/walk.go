package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// queueEntry 队列中的一个入口（文件或目录）
type queueEntry struct {
	Path  string // 用户输入的原始路径
	Name  string // tar 内根名 / 串行名前缀
	IsDir bool
	Bytes int64 // 常规文件 size 之和（目录/软链不计）
	Files int   // 常规文件 + 软链数量
}

// fileItem 串行模式下的扁平文件项
type fileItem struct {
	Name   string // 传输名，如 book/ch1.pdf
	Path   string // 源路径
	IsDir  bool
	IsLink bool
	Link   string
	Mode   int64
	Mtime  int64
	Size   int64
}

// scanEntry 预扫描一个入口：Lstat 递归，不跟随软链
func scanEntry(p string) (*queueEntry, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("不支持软链接或特殊文件: %s", p)
	}
	clean, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	e := &queueEntry{Path: clean, Name: filepath.Base(clean), IsDir: fi.IsDir()}
	if _, err := safeRelPath(e.Name); err != nil {
		return nil, err
	}
	switch {
	case fi.IsDir():
		if err := walkSum(clean, e); err != nil {
			return nil, err
		}
	case fi.Mode()&os.ModeSymlink != 0:
		e.Files = 1
	case fi.Mode().IsRegular():
		e.Bytes = fi.Size()
		e.Files = 1
	}
	return e, nil
}

func walkSum(dir string, e *queueEntry) error {
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, de := range des {
		full := filepath.Join(dir, de.Name())
		rel, err := filepath.Rel(filepath.Dir(e.Path), full)
		if err != nil {
			return err
		}
		if _, err := safeRelPath(filepath.ToSlash(rel)); err != nil {
			return fmt.Errorf("非法文件名 %q: %w", full, err)
		}
		fi, err := os.Lstat(full)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			if err := walkSum(full, e); err != nil {
				return err
			}
		case !fi.Mode().IsRegular():
			return fmt.Errorf("不支持软链接或特殊文件: %s", full)
		case fi.Mode().IsRegular():
			e.Bytes += fi.Size()
			e.Files++
		}
	}
	return nil
}

// makeFileItem 构造文件项（软链以目标字符串为内容，mode 0777）
func makeFileItem(p, name string) (fileItem, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return fileItem{}, err
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return fileItem{}, fmt.Errorf("不支持软链接或特殊文件: %s", p)
	}
	if _, err := safeRelPath(name); err != nil {
		return fileItem{}, err
	}
	it := fileItem{
		IsDir: fi.IsDir(),
		Name:  name,
		Path:  p,
		Mode:  int64(fi.Mode().Perm()),
		Mtime: fi.ModTime().Unix(),
	}
	if fi.IsDir() {
		it.Size = 0
		return it, nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(p)
		if err != nil {
			return fileItem{}, err
		}
		it.IsLink = true
		it.Link = link
		it.Size = int64(len(link))
		it.Mode = 0o777
	} else {
		it.Size = fi.Size()
	}
	return it, nil
}

// flattenEntries 把队列展开为串行传输的扁平文件列表
func flattenEntries(entries []*queueEntry) ([]fileItem, error) {
	var out []fileItem
	for _, e := range entries {
		fi, err := os.Lstat(e.Path)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			it, err := makeFileItem(e.Path, e.Name)
			if err != nil {
				return nil, err
			}
			out = append(out, it)
			continue
		}
		if err := walkFiles(filepath.Clean(e.Path), e.Name, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func walkFiles(dir, prefix string, out *[]fileItem) error {
	it, err := makeFileItem(dir, prefix)
	if err != nil {
		return err
	}
	*out = append(*out, it)
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, de := range des {
		full := filepath.Join(dir, de.Name())
		fi, err := os.Lstat(full)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			if err := walkFiles(full, prefix+"/"+de.Name(), out); err != nil {
				return err
			}
			continue
		}
		it, err := makeFileItem(full, prefix+"/"+de.Name())
		if err != nil {
			return err
		}
		*out = append(*out, it)
	}
	return nil
}

// safeRelPath 校验并归一化对端给出的相对路径（路径穿越防护）
//
// 拒绝：空名、控制字符、绝对路径、含 .. 段、Windows 盘符。
func safeRelPath(name string) (string, error) {
	if name == "" || len(name) > 4096 {
		return "", errors.New("名称为空或过长")
	}
	n := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(n, "/") {
		return "", fmt.Errorf("拒绝绝对路径: %q", name)
	}
	parts := strings.Split(n, "/")
	if len(parts) > 128 {
		return "", errors.New("路径层级过深")
	}
	for _, part := range parts {
		if part == ".." {
			return "", fmt.Errorf("拒绝 .. 路径段: %q", name)
		}
		if strings.ContainsAny(part, ":<>\"|?*") {
			return "", fmt.Errorf("拒绝特殊文件名: %q", name)
		}
		for _, r := range part {
			if r < 32 || r == 127 {
				return "", errors.New("文件名含控制字符")
			}
		}
		if isWindowsDeviceName(part) {
			return "", errors.New("拒绝 Windows 设备名称")
		}
	}
	clean := path.Clean(n)
	if clean == "." || !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", errors.New("非法相对路径")
	}
	return filepath.FromSlash(clean), nil
}
