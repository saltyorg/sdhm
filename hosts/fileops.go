package hosts

import (
	"io"
	"io/fs"
	"os"
)

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
	Lstat(string) (fs.FileInfo, error)
	ReadFile(string) ([]byte, error)
	CreateTemp(string, string) (syncFile, error)
	Rename(string, string) error
	Remove(string) error
	OpenDir(string) (syncDir, error)
}

type osFileOps struct{}

func (osFileOps) Lstat(path string) (fs.FileInfo, error) {
	return os.Lstat(path)
}

func (osFileOps) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
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
