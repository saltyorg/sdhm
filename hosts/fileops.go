package hosts

import (
	"io"
	"io/fs"
	"os"
	"syscall"
)

type readHandle interface {
	io.Reader
	Stat() (fs.FileInfo, error)
	Close() error
}

type syncFile interface {
	io.Writer
	Name() string
	Chmod(fs.FileMode) error
	Chown(int, int) error
	Sync() error
	Close() error
}

type syncDir interface {
	Sync() error
	Close() error
}

type fileOps interface {
	OpenReadNoFollow(string) (readHandle, error)
	CreateTemp(string, string) (syncFile, error)
	Rename(string, string) error
	Remove(string) error
	OpenDir(string) (syncDir, error)
}

type osFileOps struct{}

func (osFileOps) OpenReadNoFollow(path string) (readHandle, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}

func (osFileOps) CreateTemp(dir, pattern string) (syncFile, error) {
	return os.CreateTemp(dir, pattern)
}

func (osFileOps) Rename(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

func (osFileOps) Remove(path string) error {
	return os.Remove(path)
}

func (osFileOps) OpenDir(path string) (syncDir, error) {
	return os.Open(path)
}
