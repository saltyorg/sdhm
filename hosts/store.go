package hosts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/saltyorg/sdhm/daemon"
)

type fileMetadata struct {
	mode         fs.FileMode
	uid          int
	gid          int
	setOwnership bool
}

type fileSnapshot struct {
	data     []byte
	metadata fileMetadata
	exists   bool
}

const rollbackTimeout = 5 * time.Second

var errConcurrentModification = errors.New("destination changed during replacement")

type Store struct {
	ops            fileOps
	hostsPath      string
	backupPath     string
	beginMarker    string
	endMarker      string
	defaultNetwork string
	hostname       func() (string, error)
}

func newStore(ops fileOps) *Store {
	return &Store{ops: ops, hostname: os.Hostname}
}

// NewStore creates a transactional managed-hosts store.
func NewStore(hostsPath, backupPath, sectionName, defaultNetwork string) *Store {
	return &Store{
		ops:            osFileOps{},
		hostsPath:      hostsPath,
		backupPath:     backupPath,
		beginMarker:    "# BEGIN " + sectionName,
		endMarker:      "# END " + sectionName,
		defaultNetwork: defaultNetwork,
		hostname:       os.Hostname,
	}
}

var _ daemon.HostStore = (*Store)(nil)

// Prepare validates the hosts file, creates an initial managed section when
// none exists, and recovers corrupt markers only from a validated backup.
func (s *Store) Prepare(ctx context.Context) (daemon.PrepareResult, error) {
	if err := checkContext(ctx, "prepare hosts file"); err != nil {
		return daemon.PrepareResult{}, err
	}

	current, metadata, err := s.readRegularFile(s.hostsPath)
	if err != nil {
		return daemon.PrepareResult{}, fmt.Errorf("read hosts file: %w", err)
	}
	state, _, markerErr := locateMarkers(current, s.beginMarker, s.endMarker)
	if markerErr == nil && state == validMarkers {
		if err := s.ensureBackupMatches(ctx, current, metadata); err != nil {
			return daemon.PrepareResult{}, err
		}
		return daemon.PrepareResult{}, nil
	}
	if markerErr == nil && state == noMarkers {
		proposed := appendEmptySection(current, s.beginMarker, s.endMarker)
		if err := requireValidMarkers(proposed, s.beginMarker, s.endMarker); err != nil {
			return daemon.PrepareResult{}, fmt.Errorf("validate initialized hosts file: %w", err)
		}
		if err := s.initializeManagedSection(ctx, current, proposed, metadata); err != nil {
			return daemon.PrepareResult{}, err
		}
		return daemon.PrepareResult{}, nil
	}

	backup, _, err := s.readRegularFile(s.backupPath)
	if err != nil {
		return daemon.PrepareResult{}, fmt.Errorf("recover corrupt hosts file: %w", errors.Join(markerErr, err))
	}
	if err := requireValidMarkers(backup, s.beginMarker, s.endMarker); err != nil {
		return daemon.PrepareResult{}, fmt.Errorf("recover corrupt hosts file: invalid backup: %w", errors.Join(markerErr, err))
	}
	expectedTarget := fileSnapshot{data: current, metadata: metadata, exists: true}
	if err := s.restoreTargetIfUnchanged(ctx, s.hostsPath, backup, metadata, expectedTarget); err != nil {
		return daemon.PrepareResult{}, fmt.Errorf("recover corrupt hosts file: %w", err)
	}
	return daemon.PrepareResult{RecoveredFromBackup: true}, nil
}

// Apply replaces the managed body from one complete endpoint snapshot.
func (s *Store) Apply(ctx context.Context, endpoints []daemon.Endpoint) error {
	if err := checkContext(ctx, "apply hosts snapshot"); err != nil {
		return err
	}

	current, metadata, err := s.readRegularFile(s.hostsPath)
	if err != nil {
		return fmt.Errorf("read hosts file: %w", err)
	}
	state, section, err := locateMarkers(current, s.beginMarker, s.endMarker)
	if err != nil {
		return fmt.Errorf("validate hosts file: %w", err)
	}
	if state != validMarkers {
		return fmt.Errorf("validate hosts file: managed markers are missing")
	}

	body, err := renderEndpoints(endpoints, s.defaultNetwork)
	if err != nil {
		return fmt.Errorf("render hosts entries: %w", err)
	}
	proposed := replaceManagedSection(current, section, body)
	if err := requireValidMarkers(proposed, s.beginMarker, s.endMarker); err != nil {
		return fmt.Errorf("validate proposed hosts file: %w", err)
	}
	if bytes.Equal(current, proposed) {
		return s.ensureBackupMatches(ctx, current, metadata)
	}

	return s.applyReplacement(ctx, current, proposed, metadata, metadata)
}

// Regenerate replaces the hosts file with Ubuntu-compatible baseline content
// and leaves the backup as a current validated mirror.
func (s *Store) Regenerate(ctx context.Context) error {
	if err := checkContext(ctx, "regenerate hosts file"); err != nil {
		return err
	}

	hostname, err := s.hostname()
	if err != nil {
		return fmt.Errorf("read system hostname: %w", err)
	}
	if err := validateHostnamePart(hostname); err != nil {
		return fmt.Errorf("system hostname %q: %w", hostname, err)
	}
	fresh := freshHostsFile(hostname, s.beginMarker, s.endMarker)
	if err := requireValidMarkers(fresh, s.beginMarker, s.endMarker); err != nil {
		return fmt.Errorf("validate regenerated hosts file: %w", err)
	}

	current, targetMetadata, targetExists, err := s.readOptionalRegularFile(s.hostsPath)
	if err != nil {
		return fmt.Errorf("inspect hosts file: %w", err)
	}
	if !targetExists {
		targetMetadata = defaultFileMetadata()
	}
	if targetExists {
		if err := requireValidMarkers(current, s.beginMarker, s.endMarker); err == nil {
			return s.applyReplacement(ctx, current, fresh, targetMetadata, defaultFileMetadata())
		}
	}

	if err := s.ensureBackupMatches(ctx, fresh, defaultFileMetadata()); err != nil {
		return fmt.Errorf("seed regenerated backup: %w", err)
	}

	expectedTarget := fileSnapshot{data: current, metadata: targetMetadata, exists: targetExists}
	if _, err := s.replaceFileIfUnchanged(ctx, s.hostsPath, fresh, targetMetadata, expectedTarget); err != nil {
		return fmt.Errorf("replace hosts file with regenerated content: %w", err)
	}
	return nil
}

// applyReplacement retains the caller-validated current bytes in the backup,
// installs caller-validated proposed bytes, refreshes the backup to the
// committed target, and restores only the target after a late target failure.
func (s *Store) applyReplacement(ctx context.Context, current, proposed []byte, targetMetadata, newBackupMetadata fileMetadata) error {
	backup, backupMetadata, exists, err := s.readOptionalRegularFile(s.backupPath)
	if err != nil {
		return fmt.Errorf("inspect backup: %w", err)
	}
	backupSnapshot := fileSnapshot{data: backup, metadata: backupMetadata, exists: exists}
	if !exists {
		backupMetadata = newBackupMetadata
	}

	if !exists || !bytes.Equal(backup, current) {
		if _, err := s.replaceFileIfUnchanged(ctx, s.backupPath, current, backupMetadata, backupSnapshot); err != nil {
			return fmt.Errorf("refresh backup: %w", err)
		}
	}

	targetSnapshot := fileSnapshot{data: current, metadata: targetMetadata, exists: true}
	renamed, err := s.replaceFileIfUnchanged(ctx, s.hostsPath, proposed, targetMetadata, targetSnapshot)
	if err == nil {
		if err := s.ensureBackupMatches(ctx, proposed, newBackupMetadata); err != nil {
			return fmt.Errorf("refresh committed backup: %w", err)
		}
		return nil
	}
	primaryErr := fmt.Errorf("replace hosts file: %w", err)
	if !renamed {
		return primaryErr
	}
	committedSnapshot := fileSnapshot{data: proposed, metadata: targetMetadata, exists: true}
	rollbackErr := s.rollbackTarget(ctx, s.hostsPath, current, targetMetadata, committedSnapshot)
	if rollbackErr == nil {
		return fmt.Errorf("%w; target restored from retained snapshot", primaryErr)
	}
	return errors.Join(
		primaryErr,
		fmt.Errorf("restore target after replacement failure: %w", rollbackErr),
	)
}

func (s *Store) ensureBackupMatches(ctx context.Context, validTarget []byte, targetMetadata fileMetadata) error {
	backup, metadata, exists, err := s.readOptionalRegularFile(s.backupPath)
	if err != nil {
		return fmt.Errorf("inspect backup: %w", err)
	}
	if exists && bytes.Equal(backup, validTarget) {
		return nil
	}
	backupSnapshot := fileSnapshot{data: backup, metadata: metadata, exists: exists}
	if !exists {
		metadata = targetMetadata
	}

	if _, err := s.replaceFileIfUnchanged(ctx, s.backupPath, validTarget, metadata, backupSnapshot); err != nil {
		return fmt.Errorf("refresh backup mirror: %w", err)
	}
	return nil
}

func (s *Store) initializeManagedSection(ctx context.Context, current, proposed []byte, targetMetadata fileMetadata) error {
	if err := s.ensureBackupMatches(ctx, proposed, targetMetadata); err != nil {
		return fmt.Errorf("seed initialized backup: %w", err)
	}

	targetSnapshot := fileSnapshot{data: current, metadata: targetMetadata, exists: true}
	renamed, err := s.replaceFileIfUnchanged(ctx, s.hostsPath, proposed, targetMetadata, targetSnapshot)
	if err == nil {
		return nil
	}
	primaryErr := fmt.Errorf("initialize managed section: %w", err)
	if !renamed {
		return primaryErr
	}
	committedSnapshot := fileSnapshot{data: proposed, metadata: targetMetadata, exists: true}
	rollbackErr := s.rollbackTarget(ctx, s.hostsPath, current, targetMetadata, committedSnapshot)
	return errors.Join(primaryErr, rollbackErr)
}

func (s *Store) rollbackTarget(
	ctx context.Context,
	target string,
	retainedValidatedBytes []byte,
	metadata fileMetadata,
	expected fileSnapshot,
) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	return s.restoreTargetIfUnchanged(rollbackCtx, target, retainedValidatedBytes, metadata, expected)
}

func (s *Store) readRegularFile(path string) ([]byte, fileMetadata, error) {
	data, metadata, exists, err := s.readOptionalRegularFile(path)
	if err != nil {
		return nil, fileMetadata{}, err
	}
	if !exists {
		return nil, fileMetadata{}, fmt.Errorf("%q does not exist: %w", path, fs.ErrNotExist)
	}
	return data, metadata, nil
}

func (s *Store) readOptionalRegularFile(path string) ([]byte, fileMetadata, bool, error) {
	file, err := s.ops.OpenReadNoFollow(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fileMetadata{}, false, nil
	}
	if err != nil {
		return nil, fileMetadata{}, false, fmt.Errorf("open %q without following symlinks: %w", path, err)
	}

	info, err := file.Stat()
	if err != nil {
		primary := fmt.Errorf("stat opened file %q: %w", path, err)
		return nil, fileMetadata{}, false, joinReadClose(primary, file, path)
	}
	if !info.Mode().IsRegular() {
		primary := fmt.Errorf("%q is not a regular file", path)
		return nil, fileMetadata{}, false, joinReadClose(primary, file, path)
	}

	data, err := io.ReadAll(file)
	if err != nil {
		primary := fmt.Errorf("read opened file %q: %w", path, err)
		return nil, fileMetadata{}, false, joinReadClose(primary, file, path)
	}
	if err := joinReadClose(nil, file, path); err != nil {
		return nil, fileMetadata{}, false, err
	}
	return data, metadataFromInfo(info), true, nil
}

func joinReadClose(primary error, file readHandle, path string) error {
	if err := file.Close(); err != nil {
		return errors.Join(primary, fmt.Errorf("close opened file %q: %w", path, err))
	}
	return primary
}

func metadataFromInfo(info fs.FileInfo) fileMetadata {
	modeMask := fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	metadata := fileMetadata{mode: info.Mode() & modeMask}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		metadata.uid = int(stat.Uid)
		metadata.gid = int(stat.Gid)
		metadata.setOwnership = true
	}
	return metadata
}

func defaultFileMetadata() fileMetadata {
	return fileMetadata{
		mode:         0o644,
		uid:          os.Getuid(),
		gid:          os.Getgid(),
		setOwnership: true,
	}
}

func freshHostsFile(hostname, beginMarker, endMarker string) []byte {
	return fmt.Appendf(nil, "127.0.0.1\tlocalhost\n"+
		"127.0.1.1\t%s\n"+
		"\n"+
		"# The following lines are desirable for IPv6 capable hosts\n"+
		"::1\tip6-localhost ip6-loopback\n"+
		"fe00::0\tip6-localnet\n"+
		"ff00::0\tip6-mcastprefix\n"+
		"ff02::1\tip6-allnodes\n"+
		"ff02::2\tip6-allrouters\n"+
		"ff02::3\tip6-allhosts\n"+
		"\n"+
		"%s\n"+
		"%s\n", hostname, beginMarker, endMarker)
}

func appendEmptySection(data []byte, beginMarker, endMarker string) []byte {
	appended := bytes.Clone(data)
	if len(appended) > 0 && appended[len(appended)-1] != '\n' {
		appended = append(appended, '\n')
	}
	appended = fmt.Appendf(appended, "%s\n%s\n", beginMarker, endMarker)
	return appended
}

func requireValidMarkers(data []byte, beginMarker, endMarker string) error {
	state, _, err := locateMarkers(data, beginMarker, endMarker)
	if err != nil {
		return err
	}
	if state != validMarkers {
		return fmt.Errorf("managed markers are missing")
	}
	return nil
}

// replaceFile replaces target through an adjacent temporary file. The returned
// boolean reports whether the rename occurred, including when a later
// durability or validation step fails.
func (s *Store) replaceFile(ctx context.Context, target string, data []byte, metadata fileMetadata) (bool, error) {
	return s.replaceFileWithExpectation(ctx, target, data, metadata, nil)
}

func (s *Store) replaceFileIfUnchanged(
	ctx context.Context,
	target string,
	data []byte,
	metadata fileMetadata,
	expected fileSnapshot,
) (bool, error) {
	return s.replaceFileWithExpectation(ctx, target, data, metadata, &expected)
}

func (s *Store) replaceFileWithExpectation(
	ctx context.Context,
	target string,
	data []byte,
	metadata fileMetadata,
	expected *fileSnapshot,
) (bool, error) {
	tempPath, err := s.stageReplacement(ctx, target, data, metadata)
	if err != nil {
		return false, err
	}
	return s.commitReplacement(ctx, tempPath, target, data, expected)
}

func (s *Store) stageReplacement(
	ctx context.Context,
	target string,
	data []byte,
	metadata fileMetadata,
) (stagedPath string, result error) {
	if err := checkContext(ctx, "create temporary file"); err != nil {
		return "", err
	}

	parent := filepath.Dir(target)
	temp, err := s.ops.CreateTemp(parent, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temporary file for %q: %w", target, err)
	}
	tempPath := temp.Name()
	closeAttempted := false
	defer func() {
		if result != nil {
			result = s.cleanupTemporary(temp, tempPath, closeAttempted, result)
		}
	}()

	if err := configureTemporaryOwnership(ctx, temp, metadata); err != nil {
		return "", err
	}
	if err := writeAndSyncTemporary(ctx, temp, data, metadata.mode); err != nil {
		return "", err
	}
	closeAttempted = true
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close temporary file: %w", err)
	}
	return tempPath, nil
}

func configureTemporaryOwnership(ctx context.Context, temp syncFile, metadata fileMetadata) error {
	if !metadata.setOwnership {
		return nil
	}
	if err := checkContext(ctx, "restrict temporary file mode"); err != nil {
		return err
	}
	if err := temp.Chmod(0); err != nil {
		return fmt.Errorf("restrict temporary file mode: %w", err)
	}
	if err := checkContext(ctx, "set temporary file ownership"); err != nil {
		return err
	}
	if err := temp.Chown(metadata.uid, metadata.gid); err != nil {
		return fmt.Errorf("set temporary file ownership: %w", err)
	}
	return nil
}

func writeAndSyncTemporary(ctx context.Context, temp syncFile, data []byte, mode fs.FileMode) error {
	if err := checkContext(ctx, "write temporary file"); err != nil {
		return err
	}
	written, err := temp.Write(data)
	if err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if written != len(data) {
		return fmt.Errorf("write temporary file: %w", io.ErrShortWrite)
	}
	if err := checkContext(ctx, "set final temporary file mode"); err != nil {
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		return fmt.Errorf("set final temporary file mode: %w", err)
	}
	if err := checkContext(ctx, "sync temporary file"); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	return nil
}

func (s *Store) cleanupTemporary(temp syncFile, tempPath string, closeAttempted bool, primary error) error {
	if !closeAttempted {
		if closeErr := temp.Close(); closeErr != nil {
			primary = errors.Join(primary, fmt.Errorf("close temporary file %q: %w", tempPath, closeErr))
		}
	}
	if removeErr := s.ops.Remove(tempPath); removeErr != nil {
		primary = errors.Join(primary, fmt.Errorf("remove temporary file %q: %w", tempPath, removeErr))
	}
	return primary
}

func (s *Store) commitReplacement(
	ctx context.Context,
	tempPath string,
	target string,
	data []byte,
	expected *fileSnapshot,
) (bool, error) {
	cleanup := func(primary error) (bool, error) {
		if removeErr := s.ops.Remove(tempPath); removeErr != nil {
			primary = errors.Join(primary, fmt.Errorf("remove temporary file %q: %w", tempPath, removeErr))
		}
		return false, primary
	}

	if expected != nil {
		if err := checkContext(ctx, "validate destination before rename"); err != nil {
			return cleanup(err)
		}
		if err := s.requireSnapshot(target, *expected); err != nil {
			return cleanup(err)
		}
	}

	if err := checkContext(ctx, "rename temporary file"); err != nil {
		return cleanup(err)
	}
	if err := s.ops.Rename(tempPath, target); err != nil {
		return cleanup(fmt.Errorf("rename temporary file over %q: %w", target, err))
	}

	if err := s.syncParentDirectory(ctx, filepath.Dir(target)); err != nil {
		return true, err
	}
	if err := s.validateReplacement(ctx, target, data); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Store) syncParentDirectory(ctx context.Context, parent string) error {
	if err := checkContext(ctx, "open parent directory"); err != nil {
		return err
	}
	dir, err := s.ops.OpenDir(parent)
	if err != nil {
		return fmt.Errorf("open parent directory %q: %w", parent, err)
	}
	if err := checkContext(ctx, "sync parent directory"); err != nil {
		return joinDirClose(err, dir, parent)
	}
	if err := dir.Sync(); err != nil {
		return joinDirClose(fmt.Errorf("sync parent directory %q: %w", parent, err), dir, parent)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close parent directory %q: %w", parent, err)
	}
	return nil
}

func (s *Store) validateReplacement(ctx context.Context, target string, data []byte) error {
	if err := checkContext(ctx, "read destination back"); err != nil {
		return err
	}
	readback, _, err := s.readRegularFile(target)
	if err != nil {
		return fmt.Errorf("read destination %q back: %w", target, err)
	}
	if !bytes.Equal(readback, data) {
		return fmt.Errorf("read destination %q back: content mismatch", target)
	}
	return nil
}

func (s *Store) requireSnapshot(path string, expected fileSnapshot) error {
	data, metadata, exists, err := s.readOptionalRegularFile(path)
	if err != nil {
		return fmt.Errorf("verify destination %q before replacement: %w", path, err)
	}
	if exists != expected.exists || (exists && (!bytes.Equal(data, expected.data) || metadata != expected.metadata)) {
		return fmt.Errorf("%w: %q", errConcurrentModification, path)
	}
	return nil
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

func (s *Store) restoreTargetIfUnchanged(
	ctx context.Context,
	target string,
	retainedValidatedBytes []byte,
	metadata fileMetadata,
	expected fileSnapshot,
) error {
	_, err := s.replaceFileIfUnchanged(ctx, target, retainedValidatedBytes, metadata, expected)
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
