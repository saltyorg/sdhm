package hosts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/saltyorg/sdhm/daemon"
)

type faultOps struct {
	base            fileOps
	failAt          map[string]int
	calls           map[string]int
	callOrder       []string
	failErr         map[string]error
	tempDirs        []string
	tempPaths       []string
	openDirs        []string
	readbackPending bool
	readbackBytes   []byte
	beforeOpenRead  func(string)
	afterOpenRead   func(string)
	afterChown      func(string)
	chmodModes      []fs.FileMode
	afterRename     func(string, string)
}

func newFaultOps() *faultOps {
	return &faultOps{
		base:   osFileOps{},
		failAt: make(map[string]int),
		calls:  make(map[string]int),
		failErr: map[string]error{
			"open_read":      errors.New("injected open-read failure"),
			"read_stat":      errors.New("injected read-stat failure"),
			"read":           errors.New("injected read failure"),
			"read_close":     errors.New("injected read-close failure"),
			"create_temp":    errors.New("injected create-temp failure"),
			"write":          errors.New("injected write failure"),
			"chmod":          errors.New("injected chmod failure"),
			"chown":          errors.New("injected chown failure"),
			"file_sync":      errors.New("injected file-sync failure"),
			"file_close":     errors.New("injected file-close failure"),
			"rename":         errors.New("injected rename failure"),
			"open_dir":       errors.New("injected open-dir failure"),
			"dir_sync":       errors.New("injected dir-sync failure"),
			"dir_close":      errors.New("injected dir-close failure"),
			"readback_open":  errors.New("injected readback-open failure"),
			"readback_stat":  errors.New("injected readback-stat failure"),
			"readback":       errors.New("injected readback failure"),
			"readback_close": errors.New("injected readback-close failure"),
			"remove":         errors.New("injected remove failure"),
		},
	}
}

func (o *faultOps) fault(operation string) error {
	o.callOrder = append(o.callOrder, operation)
	o.calls[operation]++
	if o.failAt[operation] == o.calls[operation] {
		return o.failErr[operation]
	}
	return nil
}

func (o *faultOps) OpenReadNoFollow(path string) (readHandle, error) {
	readback := o.readbackPending
	if readback {
		o.readbackPending = false
	}
	openOperation := "open_read"
	statOperation := "read_stat"
	readOperation := "read"
	closeOperation := "read_close"
	if readback {
		openOperation = "readback_open"
		statOperation = "readback_stat"
		readOperation = "readback"
		closeOperation = "readback_close"
	}
	if err := o.fault(openOperation); err != nil {
		return nil, err
	}
	if !readback && o.beforeOpenRead != nil {
		o.beforeOpenRead(path)
	}
	file, err := o.base.OpenReadNoFollow(path)
	if err != nil {
		return nil, err
	}
	if !readback && o.afterOpenRead != nil {
		o.afterOpenRead(path)
	}
	reader := io.Reader(file)
	if readback && o.readbackBytes != nil {
		reader = bytes.NewReader(bytes.Clone(o.readbackBytes))
	}
	return &faultReadHandle{
		readHandle:     file,
		reader:         reader,
		ops:            o,
		statOperation:  statOperation,
		readOperation:  readOperation,
		closeOperation: closeOperation,
	}, nil
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
	if o.afterRename != nil {
		o.afterRename(oldPath, newPath)
	}
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

type faultReadHandle struct {
	readHandle
	reader         io.Reader
	ops            *faultOps
	statOperation  string
	readOperation  string
	closeOperation string
	readStarted    bool
}

func (f *faultReadHandle) Stat() (fs.FileInfo, error) {
	if err := f.ops.fault(f.statOperation); err != nil {
		return nil, err
	}
	return f.readHandle.Stat()
}

func (f *faultReadHandle) Read(data []byte) (int, error) {
	if !f.readStarted {
		f.readStarted = true
		if err := f.ops.fault(f.readOperation); err != nil {
			return 0, err
		}
	}
	return f.reader.Read(data)
}

func (f *faultReadHandle) Close() error {
	faultErr := f.ops.fault(f.closeOperation)
	return errors.Join(faultErr, f.readHandle.Close())
}

func (f *faultFile) Write(data []byte) (int, error) {
	if err := f.ops.fault("write"); err != nil {
		return 0, err
	}
	return f.syncFile.Write(data)
}

func (f *faultFile) Chmod(mode fs.FileMode) error {
	f.ops.chmodModes = append(f.ops.chmodModes, mode)
	if err := f.ops.fault("chmod"); err != nil {
		return err
	}
	return f.syncFile.Chmod(mode)
}

func (f *faultFile) Chown(uid, gid int) error {
	if err := f.ops.fault("chown"); err != nil {
		return err
	}
	if err := f.syncFile.Chown(uid, gid); err != nil {
		return err
	}
	if f.ops.afterChown != nil {
		f.ops.afterChown(f.Name())
	}
	return nil
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

func TestFileOpsFaultsOpenReadOnConfiguredCall(t *testing.T) {
	target, _ := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	ops.failAt["open_read"] = 2

	file, err := ops.OpenReadNoFollow(target)
	if err != nil {
		t.Fatalf("first OpenReadNoFollow() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close first handle: %v", err)
	}
	if _, err := ops.OpenReadNoFollow(target); !errors.Is(err, ops.failErr["open_read"]) {
		t.Fatalf("second OpenReadNoFollow() error = %v, want %v", err, ops.failErr["open_read"])
	}
	file, err = ops.OpenReadNoFollow(target)
	if err != nil {
		t.Fatalf("third OpenReadNoFollow() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close third handle: %v", err)
	}
}

func TestFileOpsFaultsReadOnConfiguredCall(t *testing.T) {
	target, _ := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	ops.failAt["read"] = 2

	file, err := ops.OpenReadNoFollow(target)
	if err != nil {
		t.Fatalf("open first handle: %v", err)
	}
	data, err := io.ReadAll(file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("first read/close errors = (%v, %v)", err, closeErr)
	}
	if !bytes.Equal(data, []byte("old\n")) {
		t.Fatalf("first read = %q, want %q", data, "old\n")
	}
	file, err = ops.OpenReadNoFollow(target)
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	if _, err := io.ReadAll(file); !errors.Is(err, ops.failErr["read"]) {
		t.Fatalf("second read error = %v, want %v", err, ops.failErr["read"])
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close second handle: %v", err)
	}
	file, err = ops.OpenReadNoFollow(target)
	if err != nil {
		t.Fatalf("open third handle: %v", err)
	}
	data, err = io.ReadAll(file)
	closeErr = file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("third read/close errors = (%v, %v)", err, closeErr)
	}
	if !bytes.Equal(data, []byte("old\n")) {
		t.Fatalf("third read = %q, want %q", data, "old\n")
	}
}

func TestReadOptionalRegularFileSurfacesDescriptorPhaseFailures(t *testing.T) {
	tests := []struct {
		name      string
		operation string
	}{
		{name: "open", operation: "open_read"},
		{name: "stat", operation: "read_stat"},
		{name: "read", operation: "read"},
		{name: "close", operation: "read_close"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, _ := createTarget(t, "old\n", 0o640)
			ops := newFaultOps()
			ops.failAt[tt.operation] = 1
			store := newStore(ops)

			_, _, _, err := store.readOptionalRegularFile(target)
			if !errors.Is(err, ops.failErr[tt.operation]) {
				t.Fatalf("readOptionalRegularFile() error = %v, want %v", err, ops.failErr[tt.operation])
			}
		})
	}
}

func TestReadOptionalRegularFileJoinsReadAndCloseFailures(t *testing.T) {
	target, _ := createTarget(t, "old\n", 0o640)
	ops := newFaultOps()
	ops.failAt["read"] = 1
	ops.failAt["read_close"] = 1
	store := newStore(ops)

	_, _, _, err := store.readOptionalRegularFile(target)
	for _, operation := range []string{"read", "read_close"} {
		if !errors.Is(err, ops.failErr[operation]) {
			t.Errorf("readOptionalRegularFile() error = %v, want joined %s error", err, operation)
		}
	}
}

func TestReadOptionalRegularFileRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "hosts.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("create FIFO: %v", err)
	}
	store := newStore(osFileOps{})
	result := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, _, _, err := store.readOptionalRegularFile(fifoPath)
		result <- err
	}()
	<-started

	const promptLimit = time.Second
	timer := time.NewTimer(promptLimit)
	defer timer.Stop()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
			t.Fatalf("readOptionalRegularFile() error = %v, want non-regular-file error", err)
		}
	case <-timer.C:
		release, err := os.OpenFile(fifoPath, os.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("open test-only FIFO release handle: %v", err)
		}
		readErr := <-result
		closeErr := release.Close()
		if closeErr != nil {
			t.Errorf("close test-only FIFO release handle: %v", closeErr)
		}
		if readErr == nil || !strings.Contains(readErr.Error(), "is not a regular file") {
			t.Errorf("released readOptionalRegularFile() error = %v, want non-regular-file error", readErr)
		}
		t.Fatalf("readOptionalRegularFile() blocked opening a FIFO without a writer for %s", promptLimit)
	}
}

func TestStorePrepareUsesOpenedFileAcrossSymlinkPathSwap(t *testing.T) {
	const (
		section = "DOCKER CONTAINERS"
		decoy   = "127.0.0.1 decoy\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n"
		target  = "127.0.0.1 symlink-target\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n"
	)

	dir := t.TempDir()
	hostsPath := filepath.Join(dir, "hosts")
	openedPath := filepath.Join(dir, "opened-decoy")
	referencedPath := filepath.Join(dir, "referenced")
	backupPath := filepath.Join(dir, "hosts.backup")
	if err := os.WriteFile(hostsPath, []byte(decoy), 0o600); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	if err := os.WriteFile(referencedPath, []byte(target), 0o644); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}

	ops := newFaultOps()
	swapped := false
	ops.afterOpenRead = func(path string) {
		if path != hostsPath || swapped {
			return
		}
		swapped = true
		if err := os.Rename(hostsPath, openedPath); err != nil {
			t.Errorf("move opened decoy: %v", err)
			return
		}
		if err := os.Symlink(referencedPath, hostsPath); err != nil {
			t.Errorf("replace path with symlink: %v", err)
		}
	}
	store := NewStore(hostsPath, backupPath, section, "saltbox")
	store.ops = ops

	result, err := store.Prepare(t.Context())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result != (daemon.PrepareResult{}) {
		t.Fatalf("Prepare() result = %+v, want zero result", result)
	}
	if !swapped {
		t.Fatal("path swap did not run")
	}
	assertFileContent(t, openedPath, []byte(decoy))
	assertFileContent(t, referencedPath, []byte(target))
	assertFileContent(t, backupPath, []byte(decoy))
	assertFileMode(t, backupPath, 0o600)
	info, err := os.Lstat(hostsPath)
	if err != nil {
		t.Fatalf("lstat swapped path: %v", err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("swapped path mode = %v, want symlink", info.Mode())
	}
}

func TestMetadataFromInfoPreservesOwnershipAndApplicableModeBits(t *testing.T) {
	wantMode := fs.FileMode(0o640) | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	info := staticFileInfo{
		mode: wantMode,
		stat: &syscall.Stat_t{Uid: 123, Gid: 456},
	}

	got := metadataFromInfo(info)
	if got.mode != wantMode {
		t.Errorf("metadata mode = %v, want %v", got.mode, wantMode)
	}
	if got.uid != 123 || got.gid != 456 || !got.setOwnership {
		t.Errorf("metadata ownership = (%d, %d, %v), want (123, 456, true)", got.uid, got.gid, got.setOwnership)
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
		"create_temp":    1,
		"chmod":          2,
		"chown":          1,
		"write":          1,
		"file_sync":      1,
		"file_close":     1,
		"rename":         1,
		"open_dir":       1,
		"dir_sync":       1,
		"dir_close":      1,
		"readback_open":  1,
		"readback_stat":  1,
		"readback":       1,
		"readback_close": 1,
	}
	for operation, want := range wantCalls {
		if got := ops.calls[operation]; got != want {
			t.Errorf("%s calls = %d, want %d", operation, got, want)
		}
	}
	if got := ops.calls["remove"]; got != 0 {
		t.Errorf("remove calls = %d, want 0 after rename", got)
	}
	wantOrder := []string{
		"create_temp",
		"chmod",
		"chown",
		"write",
		"chmod",
		"file_sync",
		"file_close",
		"rename",
		"open_dir",
		"dir_sync",
		"dir_close",
		"readback_open",
		"readback_stat",
		"readback",
		"readback_close",
	}
	if !slices.Equal(ops.callOrder, wantOrder) {
		t.Errorf("file operation order = %q, want %q", ops.callOrder, wantOrder)
	}
}

func TestReplaceFileIfUnchangedRejectsConcurrentDestinationChanges(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, string)
	}{
		{
			name: "content",
			change: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("external\n"), 0o640); err != nil {
					t.Fatalf("write concurrent content: %v", err)
				}
			},
		},
		{
			name: "metadata",
			change: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatalf("change concurrent mode: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, metadata := createTarget(t, "old\n", 0o640)
			expected := fileSnapshot{data: []byte("old\n"), metadata: metadata, exists: true}
			ops := newFaultOps()
			ops.beforeOpenRead = func(path string) {
				ops.beforeOpenRead = nil
				tt.change(t, path)
			}
			store := newStore(ops)

			renamed, err := store.replaceFileIfUnchanged(t.Context(), target, []byte("new\n"), metadata, expected)
			if !errors.Is(err, errConcurrentModification) {
				t.Fatalf("replaceFileIfUnchanged() error = %v, want %v", err, errConcurrentModification)
			}
			if renamed {
				t.Fatal("replaceFileIfUnchanged() renamed = true, want false")
			}
			assertNoTempFiles(t, target, ops.tempPaths)
		})
	}
}

func TestReplaceFileIfUnchangedRejectsConcurrentCreation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "hosts")
	metadata := defaultFileMetadata()
	ops := newFaultOps()
	ops.beforeOpenRead = func(path string) {
		ops.beforeOpenRead = nil
		if err := os.WriteFile(path, []byte("external\n"), 0o640); err != nil {
			t.Fatalf("create concurrent destination: %v", err)
		}
	}
	store := newStore(ops)

	renamed, err := store.replaceFileIfUnchanged(
		t.Context(),
		target,
		[]byte("new\n"),
		metadata,
		fileSnapshot{exists: false},
	)
	if !errors.Is(err, errConcurrentModification) {
		t.Fatalf("replaceFileIfUnchanged() error = %v, want %v", err, errConcurrentModification)
	}
	if renamed {
		t.Fatal("replaceFileIfUnchanged() renamed = true, want false")
	}
	assertFileContent(t, target, []byte("external\n"))
	assertNoTempFiles(t, target, ops.tempPaths)
}

func TestReplaceFileRestrictsOwnedTempBeforeChownAndWrite(t *testing.T) {
	target, metadata := createTarget(t, "old\n", 0o750)
	metadata.mode |= fs.ModeSetuid | fs.ModeSetgid
	ops := newFaultOps()
	var (
		modeAfterChown fs.FileMode
		statErr        error
	)
	ops.afterChown = func(path string) {
		var info fs.FileInfo
		info, statErr = os.Lstat(path)
		if statErr == nil {
			modeAfterChown = info.Mode().Perm()
		}
	}
	store := newStore(ops)

	if _, err := store.replaceFile(t.Context(), target, []byte("new\n"), metadata); err != nil {
		t.Fatalf("replaceFile() error = %v", err)
	}
	if statErr != nil {
		t.Fatalf("lstat temporary file after chown: %v", statErr)
	}
	if modeAfterChown != 0 {
		t.Fatalf("temporary mode after chown = %04o, want 0000", modeAfterChown)
	}
	wantChmodModes := []fs.FileMode{0, metadata.mode}
	if !slices.Equal(ops.chmodModes, wantChmodModes) {
		t.Fatalf("temporary chmod modes = %v, want %v", ops.chmodModes, wantChmodModes)
	}
	assertFileContent(t, target, []byte("new\n"))
	assertFileModeBits(t, target, metadata.mode)
}

func TestReplaceFilePreservesSpecialModeBitsOnOwnedFile(t *testing.T) {
	target, _ := createTarget(t, "old\n", 0o640)
	wantMode := fs.FileMode(0o640) | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	if err := os.Chmod(target, wantMode); err != nil {
		t.Fatalf("set target mode: %v", err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("lstat target before replacement: %v", err)
	}
	metadata := metadataFromInfo(info)
	store := newStore(newFaultOps())

	if _, err := store.replaceFile(t.Context(), target, []byte("new\n"), metadata); err != nil {
		t.Fatalf("replaceFile() error = %v", err)
	}
	assertFileContent(t, target, []byte("new\n"))
	assertFileModeBits(t, target, wantMode)
}

func TestReplaceFilePreRenameFailuresLeaveTargetUnchanged(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		failAt    int
	}{
		{name: "create temp", operation: "create_temp", failAt: 1},
		{name: "restrictive chmod", operation: "chmod", failAt: 1},
		{name: "chown", operation: "chown", failAt: 1},
		{name: "write", operation: "write", failAt: 1},
		{name: "final chmod", operation: "chmod", failAt: 2},
		{name: "file sync", operation: "file_sync", failAt: 1},
		{name: "file close", operation: "file_close", failAt: 1},
		{name: "rename", operation: "rename", failAt: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, metadata := createTarget(t, "old\n", 0o640)
			ops := newFaultOps()
			ops.failAt[tt.operation] = tt.failAt
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
		{name: "open destination for readback", operation: "readback_open"},
		{name: "stat destination for readback", operation: "readback_stat"},
		{name: "read destination back", operation: "readback"},
		{name: "close destination after readback", operation: "readback_close"},
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

	t.Run("valid target refreshes a stale valid backup", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(validA), []byte(validB))
		store := NewStore(target, backup, section, "saltbox")

		result, err := store.Prepare(t.Context())
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if result != (daemon.PrepareResult{}) {
			t.Fatalf("Prepare() result = %+v, want zero result", result)
		}
		assertFileContent(t, target, []byte(validA))
		assertFileContent(t, backup, []byte(validA))
	})

	t.Run("valid target seeds a missing backup", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(validA), nil)
		store := NewStore(target, backup, section, "saltbox")

		result, err := store.Prepare(t.Context())
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if result != (daemon.PrepareResult{}) {
			t.Fatalf("Prepare() result = %+v, want zero result", result)
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

		result, err := store.Prepare(t.Context())
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if result != (daemon.PrepareResult{}) {
			t.Fatalf("Prepare() result = %+v, want zero result", result)
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

		result, err := store.Prepare(t.Context())
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if result != (daemon.PrepareResult{}) {
			t.Fatalf("Prepare() result = %+v, want zero result", result)
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

		result, err := store.Prepare(t.Context())
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if !result.RecoveredFromBackup {
			t.Fatal("Prepare() did not report validated backup recovery")
		}
		assertFileContent(t, target, []byte(validB))
		assertFileMode(t, target, 0o600)
		assertFileContent(t, backup, []byte(validB))
		assertFileMode(t, backup, 0o640)
	})

	t.Run("corrupt target and invalid backup fail without mutation", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(corrupt), []byte("127.0.0.1 backup-without-markers\n"))
		store := NewStore(target, backup, section, "saltbox")

		result, err := store.Prepare(t.Context())
		if err == nil {
			t.Fatal("Prepare() error = nil, want invalid recovery error")
		}
		if result != (daemon.PrepareResult{}) {
			t.Fatalf("Prepare() result = %+v, want zero result", result)
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

		result, err := store.Prepare(t.Context())
		if err == nil {
			t.Fatal("Prepare() error = nil, want missing-target error")
		}
		if result != (daemon.PrepareResult{}) {
			t.Fatalf("Prepare() result = %+v, want zero result", result)
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

		result, err := store.Prepare(t.Context())
		if err == nil {
			t.Fatal("Prepare() error = nil, want symlink-target error")
		}
		if result != (daemon.PrepareResult{}) {
			t.Fatalf("Prepare() result = %+v, want zero result", result)
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

		result, err := store.Prepare(t.Context())
		if err == nil {
			t.Fatal("Prepare() error = nil, want directory-target error")
		}
		if result != (daemon.PrepareResult{}) {
			t.Fatalf("Prepare() result = %+v, want zero result", result)
		}
		assertFileContent(t, sentinel, []byte("unchanged\n"))
	})
}

func TestStoreApplyNoChangeRepairsBackupMirror(t *testing.T) {
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
	assertFileContent(t, backup, current)
}

func TestStoreApplyRejectsConcurrentTargetChange(t *testing.T) {
	const current = "127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n10.0.0.1 old old.saltbox\n# END DOCKER CONTAINERS\n# unmanaged old\n"
	target, backup := createStoreFiles(t, []byte(current), []byte(current))
	ops := newFaultOps()
	targetReads := 0
	ops.beforeOpenRead = func(path string) {
		if path != target {
			return
		}
		targetReads++
		if targetReads != 2 {
			return
		}
		if err := os.WriteFile(target, []byte("external writer\n"), 0o640); err != nil {
			t.Fatalf("write concurrent target: %v", err)
		}
	}
	store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")
	store.ops = ops

	err := store.Apply(t.Context(), []daemon.Endpoint{{
		Network: "saltbox",
		IP:      netip.MustParseAddr("172.18.0.2"),
		Aliases: []string{"radarr"},
	}})
	if !errors.Is(err, errConcurrentModification) {
		t.Fatalf("Apply() error = %v, want %v", err, errConcurrentModification)
	}
	assertFileContent(t, target, []byte("external writer\n"))
	assertFileContent(t, backup, []byte(current))
}

func TestStoreApplyRejectsConcurrentBackupChange(t *testing.T) {
	const current = "127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n10.0.0.1 old old.saltbox\n# END DOCKER CONTAINERS\n"
	target, backup := createStoreFiles(t, []byte(current), []byte(current))
	ops := newFaultOps()
	backupReads := 0
	ops.beforeOpenRead = func(path string) {
		if path != backup {
			return
		}
		backupReads++
		if backupReads != 2 {
			return
		}
		if err := os.WriteFile(backup, []byte("external backup writer\n"), 0o600); err != nil {
			t.Fatalf("write concurrent backup: %v", err)
		}
	}
	store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")
	store.ops = ops

	err := store.Apply(t.Context(), []daemon.Endpoint{{
		Network: "saltbox",
		IP:      netip.MustParseAddr("172.18.0.2"),
		Aliases: []string{"radarr"},
	}})
	if !errors.Is(err, errConcurrentModification) {
		t.Fatalf("Apply() error = %v, want %v", err, errConcurrentModification)
	}
	assertFileContent(t, target, []byte(current))
	assertFileContent(t, backup, []byte("external backup writer\n"))
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
		assertFileContent(t, backup, want)
	})

	t.Run("secondary default moves the bare alias", func(t *testing.T) {
		target, backup := createStoreFiles(t, []byte(old), nil)
		store := NewStore(target, backup, "DOCKER CONTAINERS", "backend")

		if err := store.Apply(t.Context(), endpoints); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}
		want := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n172.20.0.2 radarr radarr.backend\n172.18.0.2 radarr.saltbox\n# END DOCKER CONTAINERS\n# unmanaged suffix\n")
		assertFileContent(t, target, want)
		assertFileContent(t, backup, want)
	})
}

func TestStoreApplyReportsPostCommitMirrorFailureAndHealsOnRetry(t *testing.T) {
	old := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n10.0.0.1 old old.saltbox\n# END DOCKER CONTAINERS\n")
	target, backup := createStoreFiles(t, old, old)
	ops := newFaultOps()
	ops.failAt["write"] = 3
	store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")
	store.ops = ops
	endpoints := []daemon.Endpoint{{
		Network: "saltbox",
		IP:      netip.MustParseAddr("172.18.0.2"),
		Aliases: []string{"radarr"},
	}}
	want := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n172.18.0.2 radarr radarr.saltbox\n# END DOCKER CONTAINERS\n")

	err := store.Apply(t.Context(), endpoints)
	if !errors.Is(err, ops.failErr["write"]) {
		t.Fatalf("Apply() error = %v, want post-commit backup write failure", err)
	}
	assertFileContent(t, target, want)
	assertFileContent(t, backup, old)

	ops.failAt["write"] = 0
	if err := store.Apply(t.Context(), endpoints); err != nil {
		t.Fatalf("Apply() retry error = %v", err)
	}
	assertFileContent(t, target, want)
	assertFileContent(t, backup, want)
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
	ops.failAt["readback_open"] = 2
	store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")
	store.ops = ops
	endpoints := []daemon.Endpoint{{
		Network: "saltbox",
		IP:      netip.MustParseAddr("172.18.0.2"),
		Aliases: []string{"radarr"},
	}}

	err := store.Apply(t.Context(), endpoints)
	if !errors.Is(err, ops.failErr["readback_open"]) {
		t.Fatalf("Apply() error = %v, want target readback sentinel", err)
	}
	if !strings.Contains(err.Error(), "target restored from retained snapshot") {
		t.Fatalf("Apply() error = %q, want successful rollback outcome", err)
	}
	assertFileContent(t, target, old)
	assertFileContent(t, backup, old)
	if got := ops.calls["rename"]; got != 3 {
		t.Fatalf("rename calls = %d, want backup, target, and target-only rollback", got)
	}
}

func TestStoreApplyCancellationAfterTargetRenameStillRollsBack(t *testing.T) {
	old := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n10.0.0.1 old old.saltbox\n# END DOCKER CONTAINERS\n")
	target, backup := createStoreFiles(t, old, []byte("backup sentinel\n"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ops := newFaultOps()
	ops.afterRename = func(_, newPath string) {
		if newPath == target && ops.calls["rename"] == 2 {
			cancel()
		}
	}
	store := NewStore(target, backup, "DOCKER CONTAINERS", "saltbox")
	store.ops = ops
	endpoints := []daemon.Endpoint{{
		Network: "saltbox",
		IP:      netip.MustParseAddr("172.18.0.2"),
		Aliases: []string{"radarr"},
	}}

	err := store.Apply(ctx, endpoints)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply() error = %v, want context.Canceled", err)
	}
	assertFileContent(t, target, old)
	assertFileContent(t, backup, old)
	if got := ops.calls["rename"]; got != 3 {
		t.Fatalf("rename calls = %d, want backup, target, and detached-context rollback", got)
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
	if !strings.Contains(err.Error(), "restore target after replacement failure") {
		t.Errorf("Apply() error = %q, want failed rollback outcome", err)
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

func TestStoreRegenerateMirrorsFreshTargetOverPriorBackup(t *testing.T) {
	prior := []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n172.18.0.2 old old.saltbox\n# END DOCKER CONTAINERS\n# retained suffix\n")
	oldBackup := []byte("127.0.0.1 older\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n")
	target, backup := createStoreFiles(t, prior, oldBackup)
	store := NewStore(target, backup, "DOCKER CONTAINERS", "")
	store.hostname = func() (string, error) { return "saltbox-host", nil }

	if err := store.Regenerate(t.Context()); err != nil {
		t.Fatalf("Regenerate() error = %v", err)
	}
	assertFileContent(t, target, freshHostsFixture())
	assertFileContent(t, backup, freshHostsFixture())
}

func TestStoreRegenerateCreatesMissingBackupWithDefaultMetadata(t *testing.T) {
	const validPrior = "127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n172.18.0.2 old old.saltbox\n# END DOCKER CONTAINERS\n"

	tests := []struct {
		name       string
		prior      []byte
		wantBackup []byte
	}{
		{
			name:       "valid target is replaced by current mirror",
			prior:      []byte(validPrior),
			wantBackup: freshHostsFixture(),
		},
		{
			name:       "corrupt target is never copied to backup",
			prior:      []byte("127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n"),
			wantBackup: freshHostsFixture(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, backup := createStoreFiles(t, tt.prior, nil)
			if err := os.Chmod(target, 0o600); err != nil {
				t.Fatalf("chmod target: %v", err)
			}
			store := NewStore(target, backup, "DOCKER CONTAINERS", "")
			store.hostname = func() (string, error) { return "saltbox-host", nil }

			if err := store.Regenerate(t.Context()); err != nil {
				t.Fatalf("Regenerate() error = %v", err)
			}
			assertFileContent(t, target, freshHostsFixture())
			assertFileMode(t, target, 0o600)
			assertFileContent(t, backup, tt.wantBackup)
			assertFileMode(t, backup, 0o644)
			assertFileOwnership(t, backup, os.Getuid(), os.Getgid())
		})
	}
}

func TestStoreRegenerateRefreshesValidBackupWhenPriorTargetIsCorrupt(t *testing.T) {
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
	assertFileContent(t, backup, freshHostsFixture())
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
	assertFileOwnership(t, backup, os.Getuid(), os.Getgid())
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

func assertFileOwnership(t *testing.T, path string, wantUID, wantGID int) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat metadata for %s has type %T, want *syscall.Stat_t", path, info.Sys())
	}
	if got := int(stat.Uid); got != wantUID {
		t.Errorf("uid of %s = %d, want %d", path, got, wantUID)
	}
	if got := int(stat.Gid); got != wantGID {
		t.Errorf("gid of %s = %d, want %d", path, got, wantGID)
	}
}

func assertFileModeBits(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	mask := fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	if got := info.Mode() & mask; got != want {
		t.Fatalf("mode bits of %s = %v, want %v", path, got, want)
	}
}

type staticFileInfo struct {
	mode fs.FileMode
	stat *syscall.Stat_t
}

func (i staticFileInfo) Name() string       { return "hosts" }
func (i staticFileInfo) Size() int64        { return 0 }
func (i staticFileInfo) Mode() fs.FileMode  { return i.mode }
func (i staticFileInfo) ModTime() time.Time { return time.Time{} }
func (i staticFileInfo) IsDir() bool        { return false }
func (i staticFileInfo) Sys() any           { return i.stat }

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
