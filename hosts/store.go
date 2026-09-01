package hosts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
)

type fileMetadata struct {
	mode         fs.FileMode
	uid          int
	gid          int
	setOwnership bool
}

type Store struct {
	ops fileOps
}

func newStore(ops fileOps) *Store {
	return &Store{ops: ops}
}

// replaceFile replaces target through an adjacent temporary file. The returned
// boolean reports whether the rename occurred, including when a later
// durability or validation step fails.
func (s *Store) replaceFile(ctx context.Context, target string, data []byte, metadata fileMetadata) (bool, error) {
	if err := checkContext(ctx, "create temporary file"); err != nil {
		return false, err
	}

	parent := filepath.Dir(target)
	temp, err := s.ops.CreateTemp(parent, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("create temporary file for %q: %w", target, err)
	}
	tempPath := temp.Name()
	closeAttempted := false
	cleanup := func(primary error) error {
		joined := primary
		if !closeAttempted {
			if closeErr := temp.Close(); closeErr != nil {
				joined = errors.Join(joined, fmt.Errorf("close temporary file %q: %w", tempPath, closeErr))
			}
			closeAttempted = true
		}
		if removeErr := s.ops.Remove(tempPath); removeErr != nil {
			joined = errors.Join(joined, fmt.Errorf("remove temporary file %q: %w", tempPath, removeErr))
		}
		return joined
	}

	if err := checkContext(ctx, "set temporary file mode"); err != nil {
		return false, cleanup(err)
	}
	if err := temp.Chmod(metadata.mode); err != nil {
		return false, cleanup(fmt.Errorf("set temporary file mode: %w", err))
	}

	if metadata.setOwnership {
		if err := checkContext(ctx, "set temporary file ownership"); err != nil {
			return false, cleanup(err)
		}
		if err := temp.Chown(metadata.uid, metadata.gid); err != nil {
			return false, cleanup(fmt.Errorf("set temporary file ownership: %w", err))
		}
	}

	if err := checkContext(ctx, "write temporary file"); err != nil {
		return false, cleanup(err)
	}
	written, err := temp.Write(data)
	if err != nil {
		return false, cleanup(fmt.Errorf("write temporary file: %w", err))
	}
	if written != len(data) {
		return false, cleanup(fmt.Errorf("write temporary file: %w", io.ErrShortWrite))
	}

	if err := checkContext(ctx, "sync temporary file"); err != nil {
		return false, cleanup(err)
	}
	if err := temp.Sync(); err != nil {
		return false, cleanup(fmt.Errorf("sync temporary file: %w", err))
	}

	closeAttempted = true
	if err := temp.Close(); err != nil {
		return false, cleanup(fmt.Errorf("close temporary file: %w", err))
	}

	if err := checkContext(ctx, "rename temporary file"); err != nil {
		return false, cleanup(err)
	}
	if err := s.ops.Rename(tempPath, target); err != nil {
		return false, cleanup(fmt.Errorf("rename temporary file over %q: %w", target, err))
	}

	if err := checkContext(ctx, "open parent directory"); err != nil {
		return true, err
	}
	dir, err := s.ops.OpenDir(parent)
	if err != nil {
		return true, fmt.Errorf("open parent directory %q: %w", parent, err)
	}

	if err := checkContext(ctx, "sync parent directory"); err != nil {
		return true, joinDirClose(err, dir, parent)
	}
	if err := dir.Sync(); err != nil {
		return true, joinDirClose(fmt.Errorf("sync parent directory %q: %w", parent, err), dir, parent)
	}
	if err := dir.Close(); err != nil {
		return true, fmt.Errorf("close parent directory %q: %w", parent, err)
	}

	if err := checkContext(ctx, "read destination back"); err != nil {
		return true, err
	}
	readback, err := s.ops.ReadFile(target)
	if err != nil {
		return true, fmt.Errorf("read destination %q back: %w", target, err)
	}
	if !bytes.Equal(readback, data) {
		return true, fmt.Errorf("read destination %q back: content mismatch", target)
	}

	return true, nil
}

// restoreTarget restores only target from caller-retained validated bytes. It
// deliberately has no backup behavior and does not attempt recursive rollback.
func (s *Store) restoreTarget(ctx context.Context, target string, retainedValidatedBytes []byte, metadata fileMetadata) error {
	_, err := s.replaceFile(ctx, target, retainedValidatedBytes, metadata)
	if err != nil {
		return fmt.Errorf("restore target %q: %w", target, err)
	}
	return nil
}

func checkContext(ctx context.Context, phase string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("before %s: %w", phase, err)
	}
	return nil
}

func joinDirClose(primary error, dir syncDir, path string) error {
	if err := dir.Close(); err != nil {
		return errors.Join(primary, fmt.Errorf("close parent directory %q: %w", path, err))
	}
	return primary
}
