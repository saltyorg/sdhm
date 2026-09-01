package hosts

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
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
