package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type selectedPath struct {
	path string
	dir  bool
}

type pathSelection struct{ items []selectedPath }

func canonicalSelectionPath(name string) (selectedPath, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return selectedPath{}, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return selectedPath{}, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return selectedPath{}, fmt.Errorf("不支持软链接或特殊文件: %q", name)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return selectedPath{}, err
	}
	if _, err := safeRelPath(filepath.Base(resolved)); err != nil {
		return selectedPath{}, err
	}
	return selectedPath{path: resolved, dir: info.IsDir()}, nil
}

func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func sameSelectionPath(a, b string) bool {
	return a == b || (runtime.GOOS == "windows" && strings.EqualFold(a, b))
}

// A directory covers its descendants. Adding the parent replaces selected
// children; selecting a covered child never schedules the same bytes twice.
func (s *pathSelection) add(name string) (bool, error) {
	item, err := canonicalSelectionPath(name)
	if err != nil {
		return false, err
	}
	for _, other := range s.items {
		if sameSelectionPath(other.path, item.path) || (other.dir && pathWithin(other.path, item.path)) {
			return false, nil
		}
	}
	kept := s.items[:0]
	for _, other := range s.items {
		if !item.dir || !pathWithin(item.path, other.path) {
			kept = append(kept, other)
		}
	}
	s.items = append(kept, item)
	return true, nil
}

func (s *pathSelection) paths() []string {
	paths := make([]string, 0, len(s.items))
	for _, item := range s.items {
		paths = append(paths, item.path)
	}
	return paths
}

func buildSelectedQueue(ctx context.Context, paths []string) ([]*queueEntry, error) {
	var selection pathSelection
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := selection.add(p); err != nil {
			return nil, err
		}
	}
	queue := make([]*queueEntry, 0, len(selection.items))
	for _, item := range selection.items {
		e, err := scanEntryContext(ctx, item.path)
		if err != nil {
			return nil, fmt.Errorf("检查 %q 失败: %w", item.path, err)
		}
		queue = append(queue, e)
	}
	return queue, nil
}
