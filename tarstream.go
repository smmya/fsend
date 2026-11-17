package main

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ---------- 发送侧：流式 tar 打包 ----------

// writeTarArchive 把队列写成 tar 流（不落盘，直接写入 w）
func writeTarArchive(w io.Writer, entries []*queueEntry) error {
	tw := tar.NewWriter(w)
	for _, e := range entries {
		base := e.Name
		if err := addPathToTar(tw, e.Path, base); err != nil {
			tw.Close()
			return err
		}
	}
	return tw.Close()
}

func addPathToTar(tw *tar.Writer, fsPath, name string) error {
	fi, err := os.Lstat(fsPath)
	if err != nil {
		return err
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return fmt.Errorf("不支持软链接或特殊文件: %s", fsPath)
	}
	var link string
	if fi.Mode()&os.ModeSymlink != 0 {
		link, err = os.Readlink(fsPath)
		if err != nil {
			return err
		}
	}
	hdr, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}
	hdr.Name = filepath.ToSlash(name)
	hdr.Uname, hdr.Gname = "", ""
	if fi.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
		hdr.Name += "/"
	}
	// 不显式指定 Format：标准库对可表示的头用 USTAR，超长/特殊名自动 PAX
	hdr.Format = tar.FormatUnknown
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("写入 tar 头失败 (%s): %w", hdr.Name, err)
	}
	switch {
	case fi.IsDir():
		des, err := os.ReadDir(fsPath)
		if err != nil {
			return err
		}
		for _, de := range des {
			if err := addPathToTar(tw, filepath.Join(fsPath, de.Name()), name+"/"+de.Name()); err != nil {
				return err
			}
		}
	case fi.Mode().IsRegular():
		f, err := os.Open(fsPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
	}
	return nil
}

// sendTarStream 发送 tar 流，返回整条 tar 流（加密前明文）的 sha256 hex
func sendTarStream(sess *SecureSession, entries []*queueEntry, bar *progressBar) (string, error) {
	pr, pw := io.Pipe()
	h := sha256.New()

	go func() {
		err := writeTarArchive(io.MultiWriter(pw, h), entries)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()

	buf := make([]byte, maxPlainLen)
	for {
		n, rerr := pr.Read(buf)
		if n > 0 {
			if err := sess.WriteFrame(frameData, buf[:n]); err != nil {
				pr.CloseWithError(err)
				return "", err
			}
			bar.add(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			pr.CloseWithError(rerr)
			return "", rerr
		}
	}
	if err := sess.WriteFrame(frameStreamEnd, nil); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------- 接收侧：流式解包 ----------

// receiveTarStream 接收 tar 流并解包，返回本地累计 sha256 hex 与对端声明的一致性
func receiveTarStream(sess *SecureSession, storeDir string, bar *progressBar) (bool, error) {
	store, err := openReceiveStore(storeDir)
	if err != nil {
		return false, err
	}
	defer store.close()
	ds := newDataStream(sess, bar)
	tr := tar.NewReader(ds)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Errorf("解析 tar 流失败: %w", err)
		}
		if err := extractTarEntry(tr, hdr, store); err != nil {
			return false, err
		}
	}
	// 消费 tar 尾部填充与 stream-end 帧
	if _, err := io.Copy(io.Discard, ds); err != nil {
		return false, err
	}

	raw, err := sess.ReadCtrl()
	if err != nil {
		return false, err
	}
	var dm doneMsg
	if err := json.Unmarshal(raw, &dm); err != nil {
		return false, fmt.Errorf("done 消息解析失败: %w", err)
	}
	if dm.T != "done" {
		return false, fmt.Errorf("协议错误: 期望 done, 收到 %q", dm.T)
	}
	local := ds.sum()
	ok := strings.EqualFold(dm.SHA256, local)
	ack := doneAckMsg{T: "doneack", OK: ok, SHA256: local}
	if !ok {
		ack.Err = "SHA-256 不一致"
	}
	if err := sess.WriteJSON(ack); err != nil {
		return false, err
	}
	store.committed = ok
	return ok, nil
}

// extractTarEntry 解包单个 tar 条目（先校验路径再写入）
func extractTarEntry(tr *tar.Reader, hdr *tar.Header, store *receiveStore) error {
	if err := store.consume(hdr.Size); err != nil {
		return err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return store.mkdir(hdr.Name)
	case tar.TypeReg, tar.TypeRegA:
		f, _, err := store.create(hdr.Name)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(f, tr, hdr.Size)
		// chmod uses the open handle; never follow an attacker-replaced path.
		if copyErr == nil {
			copyErr = f.Chmod(os.FileMode(hdr.Mode) & 0o777)
		}
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	default:
		return fmt.Errorf("拒绝 tar 软链接、硬链接或特殊条目: %q", hdr.Name)
	}
}

// ensureDir 创建目录（0755）
func ensureDir(dir string) (bool, error) {
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return false, errors.New("目标路径已存在且不是目录")
		}
		return false, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	return true, nil
}
