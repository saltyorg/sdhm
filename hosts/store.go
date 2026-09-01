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

	"github.com/saltyorg/sdhm/daemon"
)

type fileMetadata struct {
	mode         fs.FileMode
	uid          int
	gid          int
	setOwnership bool
}

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
func (s *Store) Prepare(ctx context.Context) error {
	if err := checkContext(ctx, "prepare hosts file"); err != nil {
		return err
	}

	current, metadata, err := s.readRegularFile(s.hostsPath)
	if err != nil {
		return fmt.Errorf("read hosts file: %w", err)
	}
	state, _, markerErr := locateMarkers(current, s.beginMarker, s.endMarker)
	if markerErr == nil && state == validMarkers {
		return s.ensureBackup(ctx, current, metadata)
	}
	if markerErr == nil && state == noMarkers {
		proposed := appendEmptySection(current, s.beginMarker, s.endMarker)
		if err := requireValidMarkers(proposed, s.beginMarker, s.endMarker); err != nil {
			return fmt.Errorf("validate initialized hosts file: %w", err)
		}
		return s.initializeManagedSection(ctx, current, proposed, metadata)
	}

	backup, _, err := s.readRegularFile(s.backupPath)
	if err != nil {
		return fmt.Errorf("recover corrupt hosts file: %w", errors.Join(markerErr, err))
	}
	if err := requireValidMarkers(backup, s.beginMarker, s.endMarker); err != nil {
		return fmt.Errorf("recover corrupt hosts file: invalid backup: %w", errors.Join(markerErr, err))
	}
	if err := s.restoreTarget(ctx, s.hostsPath, backup, metadata); err != nil {
		return fmt.Errorf("recover corrupt hosts file: %w", err)
	}
	return nil
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
		return nil
	}

	return s.applyReplacement(ctx, current, proposed, metadata)
}

// Regenerate replaces the hosts file with Ubuntu-compatible baseline content.
// A valid prior target becomes the backup; corrupt target bytes never do.
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
			return s.applyReplacement(ctx, current, fresh, targetMetadata)
		}
	}

	backup, backupMetadata, backupExists, err := s.readOptionalRegularFile(s.backupPath)
	if err != nil {
		return fmt.Errorf("inspect backup: %w", err)
	}
	backupValid := false
	if backupExists {
		backupValid = requireValidMarkers(backup, s.beginMarker, s.endMarker) == nil
	} else {
		backupMetadata = targetMetadata
	}
	if !backupValid {
		if _, err := s.replaceFile(ctx, s.backupPath, fresh, backupMetadata); err != nil {
			return fmt.Errorf("seed regenerated backup: %w", err)
		}
	}

	if _, err := s.replaceFile(ctx, s.hostsPath, fresh, targetMetadata); err != nil {
		return fmt.Errorf("replace hosts file with regenerated content: %w", err)
	}
	return nil
}

// applyReplacement refreshes the backup from caller-validated current bytes,
// installs caller-validated proposed bytes, and restores only the target after
// a failure that occurs after the target rename.
func (s *Store) applyReplacement(ctx context.Context, current, proposed []byte, targetMetadata fileMetadata) error {
	_, backupMetadata, exists, err := s.readOptionalRegularFile(s.backupPath)
	if err != nil {
		return fmt.Errorf("inspect backup: %w", err)
	}
	if !exists {
		backupMetadata = targetMetadata
	}

	if _, err := s.replaceFile(ctx, s.backupPath, current, backupMetadata); err != nil {
		return fmt.Errorf("refresh backup: %w", err)
	}

	renamed, err := s.replaceFile(ctx, s.hostsPath, proposed, targetMetadata)
	if err == nil {
		return nil
	}
	primaryErr := fmt.Errorf("replace hosts file: %w", err)
	if !renamed {
		return primaryErr
	}
	rollbackErr := s.restoreTarget(ctx, s.hostsPath, current, targetMetadata)
	return errors.Join(primaryErr, rollbackErr)
}

func (s *Store) ensureBackup(ctx context.Context, validTarget []byte, targetMetadata fileMetadata) error {
	backup, metadata, exists, err := s.readOptionalRegularFile(s.backupPath)
	if err != nil {
		return fmt.Errorf("inspect backup: %w", err)
	}
	if exists {
		if err := requireValidMarkers(backup, s.beginMarker, s.endMarker); err == nil {
			return nil
		}
	} else {
		metadata = targetMetadata
	}

	if _, err := s.replaceFile(ctx, s.backupPath, validTarget, metadata); err != nil {
		return fmt.Errorf("seed backup: %w", err)
	}
	return nil
}

func (s *Store) initializeManagedSection(ctx context.Context, current, proposed []byte, targetMetadata fileMetadata) error {
	_, backupMetadata, exists, err := s.readOptionalRegularFile(s.backupPath)
	if err != nil {
		return fmt.Errorf("inspect backup: %w", err)
	}
	if !exists {
		backupMetadata = targetMetadata
	}
	if _, err := s.replaceFile(ctx, s.backupPath, proposed, backupMetadata); err != nil {
		return fmt.Errorf("seed initialized backup: %w", err)
	}

	renamed, err := s.replaceFile(ctx, s.hostsPath, proposed, targetMetadata)
	if err == nil {
		return nil
	}
	primaryErr := fmt.Errorf("initialize managed section: %w", err)
	if !renamed {
		return primaryErr
	}
	rollbackErr := s.restoreTarget(ctx, s.hostsPath, current, targetMetadata)
	return errors.Join(primaryErr, rollbackErr)
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
	info, err := s.ops.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fileMetadata{}, false, nil
	}
	if err != nil {
		return nil, fileMetadata{}, false, fmt.Errorf("lstat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fileMetadata{}, false, fmt.Errorf("%q is not a regular file", path)
	}

	data, err := s.ops.ReadFile(path)
	if err != nil {
		return nil, fileMetadata{}, false, fmt.Errorf("read %q: %w", path, err)
	}
	return data, metadataFromInfo(info), true, nil
}

func metadataFromInfo(info fs.FileInfo) fileMetadata {
	metadata := fileMetadata{mode: info.Mode().Perm()}
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
