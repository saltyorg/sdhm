package hosts

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/saltyorg/sdhm/daemon"
)

type faultOps struct {
	base            fileOps
	failAt          map[string]int
	calls           map[string]int
	failErr         map[string]error
	tempDirs        []string
	tempPaths       []string
	openDirs        []string
	readbackPending bool
	readbackBytes   []byte
}

func newFaultOps() *faultOps {
	return &faultOps{
		base:   osFileOps{},
		failAt: make(map[string]int),
		calls:  make(map[string]int),
		failErr: map[string]error{
			"lstat":       errors.New("injected lstat failure"),
			"read":        errors.New("injected read failure"),
			"create_temp": errors.New("injected create-temp failure"),
			"write":       errors.New("injected write failure"),
			"chmod":       errors.New("injected chmod failure"),
			"chown":       errors.New("injected chown failure"),
			"file_sync":   errors.New("injected file-sync failure"),
			"file_close":  errors.New("injected file-close failure"),
			"rename":      errors.New("injected rename failure"),
			"open_dir":    errors.New("injected open-dir failure"),
			"dir_sync":    errors.New("injected dir-sync failure"),
			"dir_close":   errors.New("injected dir-close failure"),
			"readback":    errors.New("injected readback failure"),
			"remove":      errors.New("injected remove failure"),
		},
	}
}

func (o *faultOps) fault(operation string) error {
	o.calls[operation]++
	if o.failAt[operation] == o.calls[operation] {
		return o.failErr[operation]
	}
	return nil
}

func (o *faultOps) Lstat(path string) (fs.FileInfo, error) {
	if err := o.fault("lstat"); err != nil {
		return nil, err
	}
	return o.base.Lstat(path)
}

func (o *faultOps) ReadFile(path string) ([]byte, error) {
	operation := "read"
	if o.readbackPending {
		operation = "readback"
		o.readbackPending = false
	}
	if err := o.fault(operation); err != nil {
		return nil, err
	}
	data, err := o.base.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if operation == "readback" && o.readbackBytes != nil {
		return bytes.Clone(o.readbackBytes), nil
	}
	return data, nil
}

func (o *faultOps) CreateTemp(dir, pattern string) (syncFile, error) {
	o.tempDirs = append(o.tempDirs, dir)
	if err := o.fault("create_temp"); err != nil {
		return nil, err
	}
	file, err := o.base.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	o.tempPaths = append(o.tempPaths, file.Name())
	return &faultFile{syncFile: file, ops: o}, nil
}

func (o *faultOps) Rename(oldPath, newPath string) error {
	if err := o.fault("rename"); err != nil {
		return err
	}
	if err := o.base.Rename(oldPath, newPath); err != nil {
		return err
	}
	o.readbackPending = true
	return nil
}

func (o *faultOps) Remove(path string) error {
	if err := o.fault("remove"); err != nil {
		return err
	}
	return o.base.Remove(path)
}

func (o *faultOps) OpenDir(path string) (syncDir, error) {
	o.openDirs = append(o.openDirs, path)
	if err := o.fault("open_dir"); err != nil {
		return nil, err
	}
	dir, err := o.base.OpenDir(path)
	if err != nil {
		return nil, err
	}
	return &faultDir{syncDir: dir, ops: o}, nil
}

type faultFile struct {
	syncFile
	ops *faultOps
}

func (f *faultFile) Write(data []byte) (int, error) {
	if err := f.ops.fault("write"); err != nil {
		return 0, err
	}
	return f.syncFile.Write(data)
}

func (f *faultFile) Chmod(mode fs.FileMode) error {
	if err := f.ops.fault("chmod"); err != nil {
		return err
	}
	return f.syncFile.Chmod(mode)
}

func (f *faultFile) Chown(uid, gid int) error {
	if err := f.ops.fault("chown"); err != nil {
		return err
	}
	return f.syncFile.Chown(uid, gid)
}

func (f *faultFile) Sync() error {
	if err := f.ops.fault("file_sync"); err != nil {
		return err
	}
	return f.syncFile.Sync()
}

func (f *faultFile) Close() error {
	faultErr := f.ops.fault("file_close")
	return errors.Join(faultErr, f.syncFile.Close())
}

type faultDir struct {
	syncDir
	ops *faultOps
}

func (d *faultDir) Sync() error {
	if err := d.ops.fault("dir_sync"); err != nil {
		return err
	}
	return d.syncDir.Sync()
}

func (d *faultDir) Close() error {
	faultErr := d.ops.fault("dir_close")
	return errors.Join(faultErr, d.syncDir.Close())
}

func TestFileOpsFaultsLstatOnConfiguredCall(t *testing.T) {
	target, _ := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	ops.failAt["lstat"] = 2

	info, err := ops.Lstat(target)
	if err != nil {
		t.Fatalf("first Lstat() error = %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("first Lstat() mode = %v, want regular file", info.Mode())
	}
	if _, err := ops.Lstat(target); !errors.Is(err, ops.failErr["lstat"]) {
		t.Fatalf("second Lstat() error = %v, want %v", err, ops.failErr["lstat"])
	}
	info, err = ops.Lstat(target)
	if err != nil {
		t.Fatalf("third Lstat() error = %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("third Lstat() mode = %v, want regular file", info.Mode())
	}
}

func TestFileOpsFaultsReadOnConfiguredCall(t *testing.T) {
	target, _ := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	ops.failAt["read"] = 2

	data, err := ops.ReadFile(target)
	if err != nil {
		t.Fatalf("first ReadFile() error = %v", err)
	}
	if !bytes.Equal(data, []byte("old\n")) {
		t.Fatalf("first ReadFile() = %q, want %q", data, "old\n")
	}
	if _, err := ops.ReadFile(target); !errors.Is(err, ops.failErr["read"]) {
		t.Fatalf("second ReadFile() error = %v, want %v", err, ops.failErr["read"])
	}
	data, err = ops.ReadFile(target)
	if err != nil {
		t.Fatalf("third ReadFile() error = %v", err)
	}
	if !bytes.Equal(data, []byte("old\n")) {
		t.Fatalf("third ReadFile() = %q, want %q", data, "old\n")
	}
}

func TestReplaceFileSuccess(t *testing.T) {
	target, metadata := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	store := newStore(ops)

	renamed, err := store.replaceFile(t.Context(), target, []byte("new\n"), metadata)
	if err != nil {
		t.Fatalf("replaceFile() error = %v", err)
	}
	if !renamed {
		t.Fatal("replaceFile() renamed = false, want true")
	}
	assertFileContent(t, target, []byte("new\n"))

	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("lstat target: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("target mode = %04o, want 0640", got)
	}
	if len(ops.tempDirs) != 1 || ops.tempDirs[0] != filepath.Dir(target) {
		t.Fatalf("CreateTemp directories = %q, want [%q]", ops.tempDirs, filepath.Dir(target))
	}
	if len(ops.openDirs) != 1 || ops.openDirs[0] != filepath.Dir(target) {
		t.Fatalf("OpenDir paths = %q, want [%q]", ops.openDirs, filepath.Dir(target))
	}
	assertNoTempFiles(t, target, ops.tempPaths)

	wantCalls := map[string]int{
		"create_temp": 1,
		"chmod":       1,
		"chown":       1,
		"write":       1,
		"file_sync":   1,
		"file_close":  1,
		"rename":      1,
		"open_dir":    1,
		"dir_sync":    1,
		"dir_close":   1,
		"readback":    1,
	}
	for operation, want := range wantCalls {
		if got := ops.calls[operation]; got != want {
			t.Errorf("%s calls = %d, want %d", operation, got, want)
		}
	}
	if got := ops.calls["remove"]; got != 0 {
		t.Errorf("remove calls = %d, want 0 after rename", got)
	}
}

func TestReplaceFilePreRenameFailuresLeaveTargetUnchanged(t *testing.T) {
	tests := []struct {
		name      string
		operation string
	}{
		{name: "create temp", operation: "create_temp"},
		{name: "chmod", operation: "chmod"},
		{name: "chown", operation: "chown"},
		{name: "write", operation: "write"},
		{name: "file sync", operation: "file_sync"},
		{name: "file close", operation: "file_close"},
		{name: "rename", operation: "rename"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, metadata := createTarget(t, "old\n", 0o640)
			ops := newFaultOps()
			ops.failAt[tt.operation] = 1
			store := newStore(ops)

			renamed, err := store.replaceFile(t.Context(), target, []byte("new\n"), metadata)
			if !errors.Is(err, ops.failErr[tt.operation]) {
				t.Fatalf("replaceFile() error = %v, want %v", err, ops.failErr[tt.operation])
			}
			if renamed {
				t.Fatal("replaceFile() renamed = true, want false")
			}
			assertFileContent(t, target, []byte("old\n"))
			assertNoTempFiles(t, target, ops.tempPaths)
		})
	}
}

func TestReplaceFileCanceledContextDoesNotMutateTarget(t *testing.T) {
	target, metadata := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	store := newStore(ops)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	renamed, err := store.replaceFile(ctx, target, []byte("new\n"), metadata)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("replaceFile() error = %v, want context.Canceled", err)
	}
	if renamed {
		t.Fatal("replaceFile() renamed = true, want false")
	}
	assertFileContent(t, target, []byte("old\n"))
	if len(ops.tempDirs) != 0 {
		t.Fatalf("CreateTemp directories = %q, want none", ops.tempDirs)
	}
}

func TestReplaceFileJoinsPreRenameCleanupFailures(t *testing.T) {
	target, metadata := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	ops.failAt["write"] = 1
	ops.failAt["file_close"] = 1
	ops.failAt["remove"] = 1
	store := newStore(ops)

	renamed, err := store.replaceFile(t.Context(), target, []byte("new\n"), metadata)
	if renamed {
		t.Fatal("replaceFile() renamed = true, want false")
	}
	for _, operation := range []string{"write", "file_close", "remove"} {
		if !errors.Is(err, ops.failErr[operation]) {
			t.Errorf("replaceFile() error = %v, want joined %s error", err, operation)
		}
	}
	assertFileContent(t, target, []byte("old\n"))
	if len(ops.tempPaths) != 1 {
		t.Fatalf("temporary paths = %q, want one", ops.tempPaths)
	}
	if _, statErr := os.Lstat(ops.tempPaths[0]); statErr != nil {
		t.Fatalf("temporary path after injected remove failure: %v", statErr)
	}
	if removeErr := os.Remove(ops.tempPaths[0]); removeErr != nil {
		t.Fatalf("clean injected leftover temp: %v", removeErr)
	}
}

func TestReplaceFileJoinsIsolatedRemoveCleanupFailure(t *testing.T) {
	target, metadata := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	ops.failAt["write"] = 1
	ops.failAt["remove"] = 1
	store := newStore(ops)

	renamed, err := store.replaceFile(t.Context(), target, []byte("new\n"), metadata)
	if renamed {
		t.Fatal("replaceFile() renamed = true, want false")
	}
	for _, operation := range []string{"write", "remove"} {
		if !errors.Is(err, ops.failErr[operation]) {
			t.Errorf("replaceFile() error = %v, want joined %s error", err, operation)
		}
	}
	if errors.Is(err, ops.failErr["file_close"]) {
		t.Fatalf("replaceFile() error = %v, do not want file-close failure", err)
	}
	if got := ops.calls["file_close"]; got != 1 {
		t.Fatalf("file-close calls = %d, want 1 successful cleanup close", got)
	}
	assertFileContent(t, target, []byte("old\n"))
	if len(ops.tempPaths) != 1 {
		t.Fatalf("temporary paths = %q, want one", ops.tempPaths)
	}
	if removeErr := os.Remove(ops.tempPaths[0]); removeErr != nil {
		t.Fatalf("clean injected leftover temp: %v", removeErr)
	}
}

func TestReplaceFilePostRenameFailuresReportRenamed(t *testing.T) {
	tests := []struct {
		name      string
		operation string
	}{
		{name: "open parent directory", operation: "open_dir"},
		{name: "sync parent directory", operation: "dir_sync"},
		{name: "close parent directory", operation: "dir_close"},
		{name: "read destination back", operation: "readback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, metadata := createTarget(t, "old\n", 0o640)
			ops := newFaultOps()
			ops.failAt[tt.operation] = 1
			store := newStore(ops)

			renamed, err := store.replaceFile(t.Context(), target, []byte("new\n"), metadata)
			if !errors.Is(err, ops.failErr[tt.operation]) {
				t.Fatalf("replaceFile() error = %v, want %v", err, ops.failErr[tt.operation])
			}
			if !renamed {
				t.Fatal("replaceFile() renamed = false, want true")
			}
			assertFileContent(t, target, []byte("new\n"))
			assertNoTempFiles(t, target, ops.tempPaths)
			if got := ops.calls["remove"]; got != 0 {
				t.Fatalf("remove calls = %d, want 0 after rename", got)
			}
		})
	}
}

func TestReplaceFileJoinsDirectorySyncAndCloseFailures(t *testing.T) {
	target, metadata := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	ops.failAt["dir_sync"] = 1
	ops.failAt["dir_close"] = 1
	store := newStore(ops)

	renamed, err := store.replaceFile(t.Context(), target, []byte("new\n"), metadata)
	if !renamed {
		t.Fatal("replaceFile() renamed = false, want true")
	}
	for _, operation := range []string{"dir_sync", "dir_close"} {
		if !errors.Is(err, ops.failErr[operation]) {
			t.Errorf("replaceFile() error = %v, want joined %s error", err, operation)
		}
	}
	assertFileContent(t, target, []byte("new\n"))
}

func TestReplaceFileRejectsReadbackMismatch(t *testing.T) {
	target, metadata := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	ops.readbackBytes = []byte("not-new\n")
	store := newStore(ops)

	renamed, err := store.replaceFile(t.Context(), target, []byte("new\n"), metadata)
	if err == nil {
		t.Fatal("replaceFile() error = nil, want readback mismatch")
	}
	if !renamed {
		t.Fatal("replaceFile() renamed = false, want true")
	}
	assertFileContent(t, target, []byte("new\n"))
}

func TestRestoreTargetRestoresOnlyTargetAfterLateFailure(t *testing.T) {
	target, metadata := createTarget(t, "old\n", 0o640)
	backup := filepath.Join(filepath.Dir(target), "hosts.backup")
	if err := os.WriteFile(backup, []byte("sentinel backup\n"), 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	ops := newFaultOps()
	ops.failAt["dir_sync"] = 1
	store := newStore(ops)

	renamed, primaryErr := store.replaceFile(t.Context(), target, []byte("new\n"), metadata)
	if !renamed || !errors.Is(primaryErr, ops.failErr["dir_sync"]) {
		t.Fatalf("replaceFile() = (%v, %v), want renamed with dir-sync error", renamed, primaryErr)
	}
	ops.failAt["dir_sync"] = 0
	if err := store.restoreTarget(t.Context(), target, []byte("old\n"), metadata); err != nil {
		t.Fatalf("restoreTarget() error = %v", err)
	}

	assertFileContent(t, target, []byte("old\n"))
	assertFileContent(t, backup, []byte("sentinel backup\n"))
	if got := ops.calls["rename"]; got != 2 {
		t.Fatalf("rename calls = %d, want 2", got)
	}
	for _, dir := range ops.tempDirs {
		if dir != filepath.Dir(target) {
			t.Errorf("CreateTemp directory = %q, want target directory %q", dir, filepath.Dir(target))
		}
	}
}

func TestRestoreTargetCallerCanJoinPrimaryAndRollbackErrors(t *testing.T) {
	target, metadata := createTarget(t, "old\n", 0o640)
	backup := filepath.Join(filepath.Dir(target), "hosts.backup")
	if err := os.WriteFile(backup, []byte("sentinel backup\n"), 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	ops := newFaultOps()
	primarySentinel := errors.New("primary durability failure")
	rollbackSentinel := errors.New("rollback rename failure")
	ops.failErr["dir_sync"] = primarySentinel
	ops.failErr["rename"] = rollbackSentinel
	ops.failAt["dir_sync"] = 1
	ops.failAt["rename"] = 2
	store := newStore(ops)

	renamed, primaryErr := store.replaceFile(t.Context(), target, []byte("new\n"), metadata)
	if !renamed {
		t.Fatal("replaceFile() renamed = false, want true")
	}
	rollbackErr := store.restoreTarget(t.Context(), target, []byte("old\n"), metadata)
	joined := errors.Join(primaryErr, rollbackErr)
	if !errors.Is(joined, primarySentinel) {
		t.Errorf("joined error = %v, want primary sentinel", joined)
	}
	if !errors.Is(joined, rollbackSentinel) {
		t.Errorf("joined error = %v, want rollback sentinel", joined)
	}
	assertFileContent(t, target, []byte("new\n"))
	assertFileContent(t, backup, []byte("sentinel backup\n"))
	assertNoTempFiles(t, target, ops.tempPaths)
}

func TestStorePrepareStates(t *testing.T) {
	const (
		section = "DOCKER CONTAINERS"
		validA  = "127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n172.18.0.2 radarr radarr.saltbox\n# END DOCKER CONTAINERS\n# retained suffix\n"
		validB  = "127.0.0.1 backup-host\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n"
		corrupt = "127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n"
	)

	t.Run("valid target and valid backup stay byte-for-byte unchanged", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(validA), []byte(validB))
		store := NewStore(target, backup, section, "saltbox")

		if err := store.Prepare(t.Context()); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		assertFileContent(t, target, []byte(validA))
		assertFileContent(t, backup, []byte(validB))
	})

	t.Run("valid target seeds a missing backup", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(validA), nil)
		store := NewStore(target, backup, section, "saltbox")

		if err := store.Prepare(t.Context()); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		assertFileContent(t, target, []byte(validA))
		assertFileContent(t, backup, []byte(validA))
	})

	t.Run("valid target repairs a corrupt backup", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(validA), []byte(corrupt))
		if err := os.Chmod(backup, 0o600); err != nil {
			t.Fatalf("chmod backup: %v", err)
		}
		store := NewStore(target, backup, section, "saltbox")

		if err := store.Prepare(t.Context()); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		assertFileContent(t, target, []byte(validA))
		assertFileContent(t, backup, []byte(validA))
		assertFileMode(t, backup, 0o600)
	})

	t.Run("no markers appends an empty managed section and backs up the valid result", func(t *testing.T) {
		original := []byte("127.0.0.1 localhost\n# user suffix without newline")
		want := []byte("127.0.0.1 localhost\n# user suffix without newline\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n")
		target, backup := createStoreFiles(t, original, []byte(corrupt))
		if err := os.Chmod(target, 0o640); err != nil {
			t.Fatalf("chmod target: %v", err)
		}
		store := NewStore(target, backup, section, "saltbox")

		if err := store.Prepare(t.Context()); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		assertFileContent(t, target, want)
		assertFileContent(t, backup, want)
		assertFileMode(t, target, 0o640)
	})

	t.Run("corrupt target restores a valid backup without changing the backup", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(corrupt), []byte(validB))
		if err := os.Chmod(target, 0o600); err != nil {
			t.Fatalf("chmod target: %v", err)
		}
		if err := os.Chmod(backup, 0o640); err != nil {
			t.Fatalf("chmod backup: %v", err)
		}
		store := NewStore(target, backup, section, "saltbox")

		if err := store.Prepare(t.Context()); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		assertFileContent(t, target, []byte(validB))
		assertFileMode(t, target, 0o600)
		assertFileContent(t, backup, []byte(validB))
		assertFileMode(t, backup, 0o640)
	})

	t.Run("corrupt target and invalid backup fail without mutation", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(corrupt), []byte("127.0.0.1 backup-without-markers\n"))
		store := NewStore(target, backup, section, "saltbox")

		if err := store.Prepare(t.Context()); err == nil {
			t.Fatal("Prepare() error = nil, want invalid recovery error")
		}
		assertFileContent(t, target, []byte(corrupt))
		assertFileContent(t, backup, []byte("127.0.0.1 backup-without-markers\n"))
	})
}

func TestStorePrepareRejectsMissingAndNonRegularTargets(t *testing.T) {
	const section = "DOCKER CONTAINERS"

	t.Run("missing target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "hosts")
		backup := filepath.Join(dir, "hosts.backup")
		store := NewStore(target, backup, section, "saltbox")

		if err := store.Prepare(t.Context()); err == nil {
			t.Fatal("Prepare() error = nil, want missing-target error")
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("target was created or cannot be checked: %v", err)
		}
	})

	t.Run("symlink target", func(t *testing.T) {
		dir := t.TempDir()
		referenced := filepath.Join(dir, "referenced")
		original := []byte("127.0.0.1 referenced\n")
		if err := os.WriteFile(referenced, original, 0o644); err != nil {
			t.Fatalf("write referenced file: %v", err)
		}
		target := filepath.Join(dir, "hosts")
		if err := os.Symlink(referenced, target); err != nil {
			t.Fatalf("create target symlink: %v", err)
		}
		store := NewStore(target, filepath.Join(dir, "hosts.backup"), section, "saltbox")

		if err := store.Prepare(t.Context()); err == nil {
			t.Fatal("Prepare() error = nil, want symlink-target error")
		}
		assertFileContent(t, referenced, original)
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatalf("lstat target: %v", err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("target mode = %v, want symlink", info.Mode())
		}
	})

	t.Run("directory target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "hosts")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatalf("create target directory: %v", err)
		}
		sentinel := filepath.Join(target, "sentinel")
		if err := os.WriteFile(sentinel, []byte("unchanged\n"), 0o644); err != nil {
			t.Fatalf("write sentinel: %v", err)
		}
		store := NewStore(target, filepath.Join(dir, "hosts.backup"), section, "saltbox")

		if err := store.Prepare(t.Context()); err == nil {
			t.Fatal("Prepare() error = nil, want directory-target error")
		}
		assertFileContent(t, sentinel, []byte("unchanged\n"))
	})
}

func TestStoreApplyNoChangeLeavesBackupUntouched(t *testing.T) {
	current := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n172.18.0.2 radarr radarr.saltbox\n# END DOCKER CONTAINERS\n# suffix\n")
	sentinelBackup := []byte("sentinel backup without markers\n")
	target, backup := createStoreFiles(t, current, sentinelBackup)
	store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")
	endpoints := []daemon.Endpoint{{
		Network: "saltbox",
		IP:      netip.MustParseAddr("172.18.0.2"),
		Aliases: []string{"radarr"},
	}}

	if err := store.Apply(t.Context(), endpoints); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assertFileContent(t, target, current)
	assertFileContent(t, backup, sentinelBackup)
}

func TestStoreApplyUsesConfiguredDefaultAcrossNetworks(t *testing.T) {
	const old = "127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n10.0.0.1 old old.saltbox\n# END DOCKER CONTAINERS\n# unmanaged suffix\n"
	endpoints := []daemon.Endpoint{
		{
			Network: "backend",
			IP:      netip.MustParseAddr("172.20.0.2"),
			Aliases: []string{"radarr"},
		},
		{
			Network: "saltbox",
			IP:      netip.MustParseAddr("172.18.0.2"),
			Aliases: []string{"radarr"},
		},
	}

	t.Run("saltbox default gets the bare alias", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(old), []byte("old backup sentinel\n"))
		store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")

		if err := store.Apply(t.Context(), endpoints); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}
		want := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n172.18.0.2 radarr radarr.saltbox\n172.20.0.2 radarr.backend\n# END DOCKER CONTAINERS\n# unmanaged suffix\n")
		assertFileContent(t, target, want)
		assertFileContent(t, backup, []byte(old))
	})

	t.Run("secondary default moves the bare alias", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(old), nil)
		store := NewStore(target, backup, "DOCKER CONTAINERS", "backend")

		if err := store.Apply(t.Context(), endpoints); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}
		want := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n172.20.0.2 radarr radarr.backend\n172.18.0.2 radarr.saltbox\n# END DOCKER CONTAINERS\n# unmanaged suffix\n")
		assertFileContent(t, target, want)
		assertFileContent(t, backup, []byte(old))
	})
}

func TestStoreApplyValidationAndBackupFailuresLeaveTargetUnchanged(t *testing.T) {
	const old = "127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n"

	t.Run("renderer rejects an empty default network before mutation", func(t *testing.T) {
		sentinelBackup := []byte("sentinel backup\n")
		target, backup := createStoreFiles(t, []byte(old), sentinelBackup)
		store := NewStore(target, backup, "DOCKER CONTAINERS", "")
		endpoints := []daemon.Endpoint{{
			Network: "saltbox",
			IP:      netip.MustParseAddr("172.18.0.2"),
			Aliases: []string{"radarr"},
		}}

		if err := store.Apply(t.Context(), endpoints); err == nil {
			t.Fatal("Apply() error = nil, want default-network validation error")
		}
		assertFileContent(t, target, []byte(old))
		assertFileContent(t, backup, sentinelBackup)
	})

	t.Run("renderer rejects endpoint data before mutation", func(t *testing.T) {
		sentinelBackup := []byte("sentinel backup\n")
		target, backup := createStoreFiles(t, []byte(old), sentinelBackup)
		store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")
		endpoints := []daemon.Endpoint{{
			Network: "saltbox",
			IP:      netip.MustParseAddr("172.18.0.2"),
			Aliases: []string{"bad alias"},
		}}

		if err := store.Apply(t.Context(), endpoints); err == nil {
			t.Fatal("Apply() error = nil, want endpoint validation error")
		}
		assertFileContent(t, target, []byte(old))
		assertFileContent(t, backup, sentinelBackup)
	})

	t.Run("backup write failure", func(t *testing.T) {
		sentinelBackup := []byte("sentinel backup\n")
		target, backup := createStoreFiles(t, []byte(old), sentinelBackup)
		ops := newFaultOps()
		ops.failAt["write"] = 1
		store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")
		store.ops = ops
		endpoints := []daemon.Endpoint{{
			Network: "saltbox",
			IP:      netip.MustParseAddr("172.18.0.2"),
			Aliases: []string{"radarr"},
		}}

		err := store.Apply(t.Context(), endpoints)
		if !errors.Is(err, ops.failErr["write"]) {
			t.Fatalf("Apply() error = %v, want backup write sentinel", err)
		}
		assertFileContent(t, target, []byte(old))
		assertFileContent(t, backup, sentinelBackup)
	})

	t.Run("corrupt current target never overwrites a valid backup", func(t *testing.T) {
		corrupt := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n")
		validBackup := []byte(old)
		target, backup := createStoreFiles(t, corrupt, validBackup)
		store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")

		if err := store.Apply(t.Context(), nil); err == nil {
			t.Fatal("Apply() error = nil, want corrupt-target error")
		}
		assertFileContent(t, target, corrupt)
		assertFileContent(t, backup, validBackup)
	})
}

func TestStoreApplyLateTargetFailureRollsBackOnlyTarget(t *testing.T) {
	old := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n10.0.0.1 old old.saltbox\n# END DOCKER CONTAINERS\n")
	target, backup := createStoreFiles(t, old, []byte("backup sentinel\n"))
	ops := newFaultOps()
	ops.failAt["dir_sync"] = 2
	store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")
	store.ops = ops
	endpoints := []daemon.Endpoint{{
		Network: "saltbox",
		IP:      netip.MustParseAddr("172.18.0.2"),
		Aliases: []string{"radarr"},
	}}

	err := store.Apply(t.Context(), endpoints)
	if !errors.Is(err, ops.failErr["dir_sync"]) {
		t.Fatalf("Apply() error = %v, want target dir-sync sentinel", err)
	}
	assertFileContent(t, target, old)
	assertFileContent(t, backup, old)
	if got := ops.calls["rename"]; got != 3 {
		t.Fatalf("rename calls = %d, want backup, target, and target-only rollback", got)
	}
}

func TestStoreApplyJoinsPrimaryAndRollbackFailures(t *testing.T) {
	old := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n10.0.0.1 old old.saltbox\n# END DOCKER CONTAINERS\n")
	target, backup := createStoreFiles(t, old, []byte("backup sentinel\n"))
	ops := newFaultOps()
	primarySentinel := errors.New("target durability sentinel")
	rollbackSentinel := errors.New("rollback rename sentinel")
	ops.failErr["dir_sync"] = primarySentinel
	ops.failErr["rename"] = rollbackSentinel
	ops.failAt["dir_sync"] = 2
	ops.failAt["rename"] = 3
	store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")
	store.ops = ops
	endpoints := []daemon.Endpoint{{
		Network: "saltbox",
		IP:      netip.MustParseAddr("172.18.0.2"),
		Aliases: []string{"radarr"},
	}}

	err := store.Apply(t.Context(), endpoints)
	if !errors.Is(err, primarySentinel) {
		t.Errorf("Apply() error = %v, want primary sentinel", err)
	}
	if !errors.Is(err, rollbackSentinel) {
		t.Errorf("Apply() error = %v, want rollback sentinel", err)
	}
	assertFileContent(t, backup, old)
	wantTarget := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n172.18.0.2 radarr radarr.saltbox\n# END DOCKER CONTAINERS\n")
	assertFileContent(t, target, wantTarget)
}

func TestStoreRegenerateCreatesMissingTargetAndBackup(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "hosts")
	backup := filepath.Join(dir, "hosts.backup")
	store := NewStore(target, backup, "DOCKER CONTAINERS", "")
	store.hostname = func() (string, error) { return "saltbox-host", nil }

	if err := store.Regenerate(t.Context()); err != nil {
		t.Fatalf("Regenerate() error = %v", err)
	}
	want := freshHostsFixture()
	assertFileContent(t, target, want)
	assertFileContent(t, backup, want)
	assertFileMode(t, target, 0o644)
	assertFileMode(t, backup, 0o644)
}

func TestStoreRegenerateBacksUpValidPriorTarget(t *testing.T) {
	prior := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n172.18.0.2 old old.saltbox\n# END DOCKER CONTAINERS\n# retained suffix\n")
	oldBackup := []byte("127.0.0.1 older\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n")
	target, backup := createStoreFiles(t, prior, oldBackup)
	store := NewStore(target, backup, "DOCKER CONTAINERS", "")
	store.hostname = func() (string, error) { return "saltbox-host", nil }

	if err := store.Regenerate(t.Context()); err != nil {
		t.Fatalf("Regenerate() error = %v", err)
	}
	assertFileContent(t, target, freshHostsFixture())
	assertFileContent(t, backup, prior)
}

func TestStoreRegeneratePreservesValidBackupWhenPriorTargetIsCorrupt(t *testing.T) {
	corrupt := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n")
	validBackup := []byte("127.0.0.1 recovery\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n")
	target, backup := createStoreFiles(t, corrupt, validBackup)
	if err := os.Chmod(backup, 0o600); err != nil {
		t.Fatalf("chmod backup: %v", err)
	}
	store := NewStore(target, backup, "DOCKER CONTAINERS", "")
	store.hostname = func() (string, error) { return "saltbox-host", nil }

	if err := store.Regenerate(t.Context()); err != nil {
		t.Fatalf("Regenerate() error = %v", err)
	}
	assertFileContent(t, target, freshHostsFixture())
	assertFileContent(t, backup, validBackup)
	assertFileMode(t, backup, 0o600)
}

func TestStoreRegenerateRepairsInvalidBackupAfterCorruptTarget(t *testing.T) {
	corruptTarget := []byte("127.0.0.1 localhost\n# END DOCKER CONTAINERS\n")
	invalidBackup := []byte("127.0.0.1 backup without markers\n")
	target, backup := createStoreFiles(t, corruptTarget, invalidBackup)
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatalf("chmod target: %v", err)
	}
	if err := os.Chmod(backup, 0o600); err != nil {
		t.Fatalf("chmod backup: %v", err)
	}
	store := NewStore(target, backup, "DOCKER CONTAINERS", "")
	store.hostname = func() (string, error) { return "saltbox-host", nil }

	if err := store.Regenerate(t.Context()); err != nil {
		t.Fatalf("Regenerate() error = %v", err)
	}
	want := freshHostsFixture()
	assertFileContent(t, target, want)
	assertFileContent(t, backup, want)
	assertFileMode(t, target, 0o640)
	assertFileMode(t, backup, 0o600)
}

func TestStoreRegenerateHostnameFailureDoesNotMutateFiles(t *testing.T) {
	prior := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n")
	oldBackup := []byte("backup sentinel\n")
	target, backup := createStoreFiles(t, prior, oldBackup)
	hostnameSentinel := errors.New("hostname sentinel")
	store := NewStore(target, backup, "DOCKER CONTAINERS", "")
	store.hostname = func() (string, error) { return "", hostnameSentinel }

	err := store.Regenerate(t.Context())
	if !errors.Is(err, hostnameSentinel) {
		t.Fatalf("Regenerate() error = %v, want hostname sentinel", err)
	}
	assertFileContent(t, target, prior)
	assertFileContent(t, backup, oldBackup)
}

func freshHostsFixture() []byte {
	return []byte("127.0.0.1\tlocalhost\n" +
		"127.0.1.1\tsaltbox-host\n" +
		"\n" +
		"# The following lines are desirable for IPv6 capable hosts\n" +
		"::1\tip6-localhost ip6-loopback\n" +
		"fe00::0\tip6-localnet\n" +
		"ff00::0\tip6-mcastprefix\n" +
		"ff02::1\tip6-allnodes\n" +
		"ff02::2\tip6-allrouters\n" +
		"ff02::3\tip6-allhosts\n" +
		"\n" +
		"# BEGIN DOCKER CONTAINERS\n" +
		"# END DOCKER CONTAINERS\n")
}

func createStoreFiles(t *testing.T, targetData, backupData []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "hosts")
	backup := filepath.Join(dir, "hosts.backup")
	if targetData != nil {
		if err := os.WriteFile(target, targetData, 0o644); err != nil {
			t.Fatalf("write target: %v", err)
		}
	}
	if backupData != nil {
		if err := os.WriteFile(backup, backupData, 0o644); err != nil {
			t.Fatalf("write backup: %v", err)
		}
	}
	return target, backup
}

func assertFileMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode of %s = %04o, want %04o", path, got, want)
	}
}

func createTarget(t *testing.T, content string, mode fs.FileMode) (string, fileMetadata) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "hosts")
	if err := os.WriteFile(target, []byte(content), mode); err != nil {
		t.Fatalf("write target: %v", err)
	}
	return target, fileMetadata{
		mode:         mode,
		uid:          os.Getuid(),
		gid:          os.Getgid(),
		setOwnership: true,
	}
}

func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content of %s = %q, want %q", path, got, want)
	}
}

func assertNoTempFiles(t *testing.T, target string, tempPaths []string) {
	t.Helper()
	for _, path := range tempPaths {
		if path == target {
			continue
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temporary path %s still exists or cannot be checked: %v", path, err)
		}
	}
}
