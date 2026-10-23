package command

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// HostsLockSuffix identifies the persistent lock adjacent to the hosts file.
const HostsLockSuffix = ".sdhm.lock"

func validateDistinctFilePaths(hostsPath, backupPath string) error {
	hostsDestination, err := canonicalDestination(hostsPath)
	if err != nil {
		return fmt.Errorf("resolve hosts file destination: %w", err)
	}
	backupDestination, err := canonicalDestination(backupPath)
	if err != nil {
		return fmt.Errorf("resolve backup file destination: %w", err)
	}
	if hostsDestination == backupDestination {
		return fmt.Errorf("hosts file and backup file must be different paths: both resolve to %q", hostsDestination)
	}
	if backupDestination == hostsDestination+HostsLockSuffix {
		return fmt.Errorf("backup file must not be the hosts writer lock %q", backupDestination)
	}
	return nil
}

// Resolve parent aliases without following the final component, matching the
// store's no-follow reads and atomic replacement of the destination pathname.
func canonicalDestination(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := canonicalDirectory(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func canonicalDirectory(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// Missing parents remain valid configuration inputs. An existing dangling
	// symlink cannot be resolved safely and must retain its original error.
	if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolvedParent, err := canonicalDirectory(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}
