// Package examplekeys 仅供可执行示例加载显式指定的测试密钥，不是SDK托管功能。
package examplekeys

import (
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/eightyun/x402-tron-go/exact"
)

var ErrKeyFile = errors.New("示例密钥文件无效")

// Load 只接受当前用户的0400/0600普通文件，拒绝最终路径符号链接。
func Load(path string) (*exact.EOASigner, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrKeyFile
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrKeyFile
	}
	defer f.Close()
	s, err := f.Stat()
	if err != nil || !s.Mode().IsRegular() || (s.Mode().Perm() != 0400 && s.Mode().Perm() != 0600) || (s.Size() != 64 && s.Size() != 65) {
		return nil, ErrKeyFile
	}
	stat, ok := s.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrKeyFile
	}
	b, err := io.ReadAll(io.LimitReader(f, 66))
	defer clear(b)
	if err != nil {
		return nil, ErrKeyFile
	}
	if len(b) == 65 && b[64] == '\n' {
		b = b[:64]
	}
	var key [32]byte
	defer clear(key[:])
	if len(b) != 64 {
		return nil, ErrKeyFile
	}
	if _, err := hex.Decode(key[:], b); err != nil {
		return nil, ErrKeyFile
	}
	signer, err := exact.NewEOASigner(key[:])
	if err != nil {
		return nil, ErrKeyFile
	}
	return signer, nil
}
