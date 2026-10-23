package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/saltyorg/sdhm/command"
)

func TestOverlappingBackupConfigurationPreservesHeldWriterLock(t *testing.T) {
	for _, regenerate := range []bool{false, true} {
		mode := "daemon"
		if regenerate {
			mode = "regenerate"
		}
		t.Run(mode, func(t *testing.T) {
			hostsPath := filepath.Join(t.TempDir(), "hosts")
			const original = "127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n"
			if err := os.WriteFile(hostsPath, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			first, err := acquireHostsLock(hostsPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = first.Close() }()
			lockPath := hostsPath + hostsLockSuffix
			before, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			called := false
			root := command.NewRoot("test",
				func(context.Context, command.Config) error { called = true; return nil },
				func(context.Context, command.RegenerateConfig) error { called = true; return nil },
			)
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			args := []string{"--hosts-file", hostsPath, "--backup-file", lockPath}
			if regenerate {
				args = append([]string{"regenerate"}, args...)
			}
			root.SetArgs(args)
			if err := root.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "writer lock") {
				t.Errorf("ExecuteContext() error = %v, want writer-lock collision rejection", err)
			}
			if called {
				t.Error("runtime called for backup overlapping the held lock")
			}
			after, err := os.Lstat(lockPath)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("writer lock inode changed: error %v", err)
			}
			second, err := acquireHostsLock(hostsPath)
			if err == nil {
				_ = second.Close()
			}
			if !errors.Is(err, errHostsLocked) {
				t.Errorf("second writer lock error = %v, want %v", err, errHostsLocked)
			}
			data, err := os.ReadFile(hostsPath)
			if err != nil || string(data) != original {
				t.Errorf("hosts contents = %q, error %v, want unchanged target", data, err)
			}
		})
	}
}

func TestAcquireHostsLockExcludesSamePathUntilClose(t *testing.T) {
	hostsPath := filepath.Join(t.TempDir(), "hosts")

	first, err := acquireHostsLock(hostsPath)
	if err != nil {
		t.Fatalf("acquireHostsLock() first error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	if _, err := acquireHostsLock(hostsPath); !errors.Is(err, errHostsLocked) {
		t.Fatalf("acquireHostsLock() concurrent error = %v, want %v", err, errHostsLocked)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first lock: %v", err)
	}
	second, err := acquireHostsLock(hostsPath)
	if err != nil {
		t.Fatalf("acquireHostsLock() after close error = %v", err)
	}
	defer func() { _ = second.Close() }()

	absoluteHostsPath, err := filepath.Abs(hostsPath)
	if err != nil {
		t.Fatalf("resolve absolute hosts path: %v", err)
	}
	lockPath := absoluteHostsPath + hostsLockSuffix
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatalf("lstat persistent lock file: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("lock mode = %v, want regular file", info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock permissions = %04o, want 0600", got)
	}
}

func TestAcquireHostsLockAllowsDifferentPaths(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireHostsLock(filepath.Join(dir, "hosts-a"))
	if err != nil {
		t.Fatalf("acquire first path: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := acquireHostsLock(filepath.Join(dir, "hosts-b"))
	if err != nil {
		t.Fatalf("acquire second path: %v", err)
	}
	defer func() { _ = second.Close() }()
}

func TestAcquireHostsLockRejectsSpecialFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
					t.Fatalf("create symlink: %v", err)
				}
			},
		},
		{
			name: "fifo",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatalf("create FIFO: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hostsPath := filepath.Join(t.TempDir(), "hosts")
			lockPath := hostsPath + hostsLockSuffix
			tt.setup(t, lockPath)

			lock, err := acquireHostsLock(hostsPath)
			if err == nil {
				_ = lock.Close()
				t.Fatal("acquireHostsLock() error = nil, want special-file rejection")
			}
		})
	}
}
