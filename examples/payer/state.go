package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// 锁文件inode不删除，进程中断后由内核释放锁，不能通过删文件绕过互斥。
func lockState(directory string) (*os.File, error) {
	s, err := os.Lstat(directory)
	if err != nil || !s.IsDir() || s.Mode().Perm() != 0700 {
		return nil, errState
	}
	stat, ok := s.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errState
	}
	f, err := os.OpenFile(filepath.Join(directory, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, errState
	}
	s, err = f.Stat()
	if err != nil || !privateFile(s) {
		_ = f.Close()
		return nil, errState
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = f.Close()
		return nil, errState
	}
	return f, nil
}

func privateFile(s os.FileInfo) bool {
	stat, ok := s.Sys().(*syscall.Stat_t)
	return ok && s.Mode().IsRegular() && s.Mode().Perm() == 0600 && stat.Uid == uint32(os.Geteuid())
}

func writeState(directory, name string, b []byte) error {
	f, err := os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errState
	}
	n, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	if n != len(b) || writeErr != nil || syncErr != nil || closeErr != nil {
		return errState
	}
	d, err := os.Open(directory)
	if err != nil {
		return errState
	}
	defer d.Close()
	if d.Sync() != nil {
		return errState
	}
	return nil
}

func readState(directory, name string, maximum int64) ([]byte, error) {
	f, err := os.OpenFile(filepath.Join(directory, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errState
	}
	defer f.Close()
	s, err := f.Stat()
	if err != nil || !privateFile(s) || s.Size() > maximum {
		return nil, errState
	}
	b, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil || int64(len(b)) > maximum {
		return nil, errState
	}
	return b, nil
}

func decodeIntent(directory string) (policy, error) {
	b, err := readState(directory, "intent.json", 4096)
	var p policy
	if err != nil || json.Unmarshal(b, &p) != nil {
		return policy{}, errState
	}
	return p, nil
}
