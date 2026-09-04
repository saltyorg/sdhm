package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const hostsLockSuffix = ".sdhm.lock"

var errHostsLocked = errors.New("hosts file is locked by another SDHM process")

func acquireHostsLock(hostsPath string) (io.Closer, error) {
	absoluteHostsPath, err := filepath.Abs(hostsPath)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute hosts path %q: %w", hostsPath, err)
	}
	lockPath := absoluteHostsPath + hostsLockSuffix
	flags := os.O_CREATE | os.O_RDWR | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	file, err := os.OpenFile(lockPath, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open hosts lock %q without following symlinks: %w", lockPath, err)
	}
	fail := func(primary error) (io.Closer, error) {
		if closeErr := file.Close(); closeErr != nil {
			primary = errors.Join(primary, fmt.Errorf("close hosts lock %q: %w", lockPath, closeErr))
		}
		return nil, primary
	}

	info, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("stat hosts lock %q: %w", lockPath, err))
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("hosts lock %q is not a regular file", lockPath))
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("set hosts lock %q permissions: %w", lockPath, err))
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fail(fmt.Errorf("%w: %q", errHostsLocked, lockPath))
		}
		return fail(fmt.Errorf("acquire hosts lock %q: %w", lockPath, err))
	}
	return file, nil
}
