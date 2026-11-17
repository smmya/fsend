package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// All peer-controlled filesystem operations stay under a pinned directory handle.
type receiveStore struct {
	root        *os.Root
	files       []string
	committed   bool
	bytesLeft   int64
	entriesLeft int
}

func openReceiveStore(dir string) (*receiveStore, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &receiveStore{root: root, bytesLeft: receiveMaxBytes, entriesLeft: receiveMaxEntries}, nil
}

func (s *receiveStore) close() {
	if !s.committed {
		for i := len(s.files) - 1; i >= 0; i-- {
			s.remove(s.files[i])
		}
	}
	_ = s.root.Close()
}

func (s *receiveStore) remove(name string) {
	// Windows will not remove read-only files. Change only a handle confined to
	// the receive root before retrying cleanup.
	if err := s.root.Remove(name); err != nil {
		if f, openErr := s.root.Open(name); openErr == nil {
			_ = f.Chmod(0o600)
			_ = f.Close()
		}
		if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			errf("清理未完成文件失败 %q: %v", name, err)
		}
	}
}

// O_EXCL atomically reserves a fresh name, including against dangling symlinks.
func (s *receiveStore) create(name string) (*os.File, string, error) {
	rel, err := safeRelPath(name)
	if err != nil {
		return nil, "", err
	}
	if err := s.root.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
		return nil, "", err
	}
	ext := filepath.Ext(rel)
	stem := strings.TrimSuffix(rel, ext)
	for i := 0; i < 100000; i++ {
		candidate := rel
		if i > 0 {
			candidate = fmt.Sprintf("%s.%d%s", stem, i, ext)
		}
		f, err := s.root.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		s.files = append(s.files, candidate)
		return f, candidate, nil
	}
	return nil, "", errors.New("同名文件数量过多")
}

func (s *receiveStore) mkdir(name string) error {
	rel, err := safeRelPath(name)
	if err != nil {
		return err
	}
	return s.root.MkdirAll(rel, 0o700)
}

var receiveMaxBytes int64 = 100 << 30
var receiveMaxEntries = 100000

func (s *receiveStore) consume(size int64) error {
	if size < 0 || size == int64(^uint64(0)>>1) {
		return errors.New("文件大小无效")
	}
	if s.entriesLeft <= 0 {
		return errors.New("接收条目数超过限额")
	}
	s.entriesLeft--
	if receiveMaxBytes > 0 {
		if size > s.bytesLeft {
			return errors.New("接收字节数超过限额")
		}
		s.bytesLeft -= size
	}
	return nil
}
