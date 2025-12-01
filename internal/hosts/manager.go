package hosts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"syscall"
)

// atomicMoveFile moves a file from src to dst, with fallback to copy for cross-filesystem moves.
func atomicMoveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}

	// Check for cross-device link error (EXDEV)
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
		// Fallback to copy for cross-filesystem
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("cross-filesystem copy failed: %w", err)
		}
		os.Remove(src) // Clean up source after successful copy
		return nil
	}

	return fmt.Errorf("failed to move file: %w", err)
}

// copyFile copies a file from src to dst with proper permissions.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to create destination: %w", err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("failed to copy content: %w", err)
	}

	if err := dstFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync destination: %w", err)
	}

	return nil
}

// wrapDiskError wraps common disk errors with user-friendly messages.
func wrapDiskError(err error, operation string) error {
	if err == nil {
		return nil
	}

	errStr := err.Error()
	if strings.Contains(errStr, "no space left on device") {
		return fmt.Errorf("%s: disk full", operation)
	}
	if strings.Contains(errStr, "read-only file system") {
		return fmt.Errorf("%s: filesystem is read-only", operation)
	}

	return fmt.Errorf("%s: %w", operation, err)
}

// HostsFileManager manages the /etc/hosts file with safe operations
type HostsFileManager struct {
	hostsFile   string
	backupFile  string
	beginMarker string
	endMarker   string
}

// NewHostsFileManager creates a new HostsFileManager
// sectionName is the name of the managed section (e.g., "DOCKER CONTAINERS")
// Begin and end markers are automatically generated as "# BEGIN <sectionName>" and "# END <sectionName>"
func NewHostsFileManager(hostsFile, backupFile, sectionName string) *HostsFileManager {
	return &HostsFileManager{
		hostsFile:   hostsFile,
		backupFile:  backupFile,
		beginMarker: fmt.Sprintf("# BEGIN %s", sectionName),
		endMarker:   fmt.Sprintf("# END %s", sectionName),
	}
}

// CreateBackup creates a backup of the hosts file
func (m *HostsFileManager) CreateBackup(ctx context.Context) error {
	data, err := os.ReadFile(m.hostsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("hosts file does not exist: %w", err)
		}
		return fmt.Errorf("failed to read hosts file: %w", err)
	}

	if err := os.WriteFile(m.backupFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write backup: %w", err)
	}

	return nil
}

// RestoreBackup restores the hosts file from backup
func (m *HostsFileManager) RestoreBackup(ctx context.Context) error {
	data, err := os.ReadFile(m.backupFile)
	if err != nil {
		return fmt.Errorf("failed to read backup file: %w", err)
	}

	if err := os.WriteFile(m.hostsFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write hosts file: %w", err)
	}

	return nil
}

// ValidateWrittenContent verifies that a file contains the expected content after a write operation.
// This is used to detect write failures or corruption.
func (m *HostsFileManager) ValidateWrittenContent(ctx context.Context, filepath string, expectedContent string) error {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return fmt.Errorf("failed to read file for validation: %w", err)
	}

	if string(data) != expectedContent {
		return fmt.Errorf("file content does not match expected content (possible write failure)")
	}

	return nil
}

// EnsureMarkersValid checks that the managed section markers are in a valid state.
// If no markers exist, they are added. If markers are corrupt (one missing or wrong order),
// an error is returned and the user must run 'sdhm regenerate' to fix.
func (m *HostsFileManager) EnsureMarkersValid(ctx context.Context) error {
	data, err := os.ReadFile(m.hostsFile)
	if err != nil {
		return fmt.Errorf("failed to read hosts file: %w", err)
	}

	content := string(data)

	hasBegin := strings.Contains(content, m.beginMarker)
	hasEnd := strings.Contains(content, m.endMarker)

	// Neither marker exists - add them
	if !hasBegin && !hasEnd {
		return m.EnsureManagedSectionExists(ctx)
	}

	// Both exist - check order
	if hasBegin && hasEnd {
		beginIdx := strings.Index(content, m.beginMarker)
		endIdx := strings.Index(content, m.endMarker)

		if beginIdx > endIdx {
			return fmt.Errorf("END marker appears before BEGIN marker")
		}
		return nil
	}

	// One marker missing - corrupt state
	if hasBegin && !hasEnd {
		return fmt.Errorf("has BEGIN marker but no END marker")
	}

	return fmt.Errorf("has END marker but no BEGIN marker")
}

// readSystemHostname reads the system hostname from /etc/hostname
// Falls back to os.Hostname() if /etc/hostname is unavailable
func readSystemHostname() (string, error) {
	data, err := os.ReadFile("/etc/hostname")
	if err != nil {
		if os.IsNotExist(err) {
			// Fallback to OS hostname
			return os.Hostname()
		}
		return "", fmt.Errorf("failed to read /etc/hostname: %w", err)
	}

	hostname := strings.TrimSpace(string(data))
	if hostname == "" {
		// Fallback if file is empty
		return os.Hostname()
	}

	return hostname, nil
}

// GenerateFreshHostsFile creates a fresh hosts file with standard entries
func (m *HostsFileManager) GenerateFreshHostsFile(ctx context.Context) error {
	// Read system hostname
	hostname, err := readSystemHostname()
	if err != nil {
		return fmt.Errorf("failed to get system hostname: %w", err)
	}

	// Build fresh hosts file content matching Ubuntu Server defaults
	var content strings.Builder
	content.WriteString("127.0.0.1\tlocalhost\n")
	content.WriteString(fmt.Sprintf("127.0.1.1\t%s\n", hostname))
	content.WriteString("\n")
	content.WriteString("# The following lines are desirable for IPv6 capable hosts\n")
	content.WriteString("::1\tip6-localhost ip6-loopback\n")
	content.WriteString("fe00::0\tip6-localnet\n")
	content.WriteString("ff00::0\tip6-mcastprefix\n")
	content.WriteString("ff02::1\tip6-allnodes\n")
	content.WriteString("ff02::2\tip6-allrouters\n")
	content.WriteString("ff02::3\tip6-allhosts\n")
	content.WriteString("\n")
	content.WriteString(fmt.Sprintf("%s\n", m.beginMarker))
	content.WriteString(fmt.Sprintf("%s\n", m.endMarker))

	// Write to temporary file first
	tmpFile, err := os.CreateTemp("/tmp", "hosts_fresh_*")
	if err != nil {
		return wrapDiskError(err, "failed to create temp file")
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(content.String()); err != nil {
		tmpFile.Close()
		return wrapDiskError(err, "failed to write to temp file")
	}

	// Sync to ensure data is on disk before moving
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return wrapDiskError(err, "failed to sync temp file")
	}
	tmpFile.Close()

	// Replace hosts file (handles cross-filesystem moves)
	if err := atomicMoveFile(tmpPath, m.hostsFile); err != nil {
		return wrapDiskError(err, "failed to replace hosts file")
	}

	// Set proper permissions
	if err := os.Chmod(m.hostsFile, 0644); err != nil {
		return wrapDiskError(err, "failed to set permissions")
	}

	// Validate the written file matches expected content
	if err := m.ValidateWrittenContent(ctx, m.hostsFile, content.String()); err != nil {
		return fmt.Errorf("write validation failed: %w", err)
	}

	return nil
}

// EnsureManagedSectionExists ensures the managed section markers exist
func (m *HostsFileManager) EnsureManagedSectionExists(ctx context.Context) error {
	data, err := os.ReadFile(m.hostsFile)
	if err != nil {
		return fmt.Errorf("failed to read hosts file: %w", err)
	}

	content := string(data)

	// Check if markers already exist
	hasBegin := strings.Contains(content, m.beginMarker)
	hasEnd := strings.Contains(content, m.endMarker)

	if hasBegin && hasEnd {
		return nil // Already exists
	}

	// Create backup first
	if err := m.CreateBackup(ctx); err != nil {
		return fmt.Errorf("failed to create backup: %w", err)
	}

	// Append markers
	content = strings.TrimRight(content, "\n")
	content += fmt.Sprintf("\n%s\n%s\n", m.beginMarker, m.endMarker)

	if err := os.WriteFile(m.hostsFile, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write hosts file: %w", err)
	}

	return nil
}

// FixNonBreakingSpaces replaces non-breaking spaces with regular spaces
func (m *HostsFileManager) FixNonBreakingSpaces(ctx context.Context) error {
	data, err := os.ReadFile(m.hostsFile)
	if err != nil {
		return fmt.Errorf("failed to read hosts file: %w", err)
	}

	content := string(data)
	fixedContent := strings.ReplaceAll(content, "\u00a0", " ")

	// Only write if there were changes
	if content != fixedContent {
		// Create backup first
		if err := m.CreateBackup(ctx); err != nil {
			return fmt.Errorf("failed to create backup: %w", err)
		}

		if err := os.WriteFile(m.hostsFile, []byte(fixedContent), 0644); err != nil {
			return fmt.Errorf("failed to write hosts file: %w", err)
		}
	}

	return nil
}

// GetManagedSectionEntries reads and parses the current managed section from hosts file
func (m *HostsFileManager) GetManagedSectionEntries(ctx context.Context) ([]HostEntry, error) {
	// Read current hosts file
	data, err := os.ReadFile(m.hostsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read hosts file: %w", err)
	}

	content := string(data)

	// Find marker positions
	beginIdx := strings.Index(content, m.beginMarker)
	endIdx := strings.Index(content, m.endMarker)

	if beginIdx == -1 || endIdx == -1 {
		return nil, fmt.Errorf("managed section markers not found")
	}

	// Extract managed section content (between markers)
	managedStart := beginIdx + len(m.beginMarker)
	managedContent := content[managedStart:endIdx]

	// Parse entries from managed section
	var entries []HostEntry
	lines := strings.SplitSeq(managedContent, "\n")

	for line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse the line: IP hostname1 hostname2 ...
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue // Invalid entry, skip
		}

		ip := net.ParseIP(parts[0])
		if ip == nil {
			continue // Invalid IP, skip
		}

		hostnames := parts[1:]
		entries = append(entries, HostEntry{
			IP:        ip,
			Hostnames: hostnames,
		})
	}

	return entries, nil
}

// UpdateManagedSection updates the content between the markers
func (m *HostsFileManager) UpdateManagedSection(ctx context.Context, entries []HostEntry) error {
	// Read current hosts file
	data, err := os.ReadFile(m.hostsFile)
	if err != nil {
		return fmt.Errorf("failed to read hosts file: %w", err)
	}

	content := string(data)

	// Find marker positions
	beginIdx := strings.Index(content, m.beginMarker)
	endIdx := strings.Index(content, m.endMarker)

	if beginIdx == -1 || endIdx == -1 {
		return fmt.Errorf("managed section markers not found")
	}

	// Extract parts
	beforeSection := content[:beginIdx+len(m.beginMarker)]
	afterSection := content[endIdx:]

	// Sort entries by first hostname (alphabetically)
	sort.Slice(entries, func(i, j int) bool {
		if len(entries[i].Hostnames) == 0 && len(entries[j].Hostnames) == 0 {
			return false
		}
		if len(entries[i].Hostnames) == 0 {
			return false
		}
		if len(entries[j].Hostnames) == 0 {
			return true
		}
		return entries[i].Hostnames[0] < entries[j].Hostnames[0]
	})

	// Calculate maximum IP width for alignment
	maxIPWidth := 0
	for _, entry := range entries {
		ipLen := len(entry.IP.String())
		if ipLen > maxIPWidth {
			maxIPWidth = ipLen
		}
	}

	// Build new managed section with aligned columns
	var managedSection strings.Builder
	managedSection.WriteString("\n")
	for _, entry := range entries {
		if len(entry.Hostnames) == 0 {
			continue
		}
		// Format: "<IP padded to maxIPWidth> <hostnames>"
		line := fmt.Sprintf("%-*s %s", maxIPWidth, entry.IP.String(), strings.Join(entry.Hostnames, " "))
		managedSection.WriteString(line)
		managedSection.WriteString("\n")
	}

	// Combine all parts
	newContent := beforeSection + managedSection.String() + afterSection

	// Skip write if content hasn't changed
	if content == newContent {
		return nil
	}

	// Create temporary file
	tmpFile, err := os.CreateTemp("/tmp", "hosts_*")
	if err != nil {
		return wrapDiskError(err, "failed to create temp file")
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(newContent); err != nil {
		tmpFile.Close()
		return wrapDiskError(err, "failed to write to temp file")
	}

	// Sync to ensure data is on disk before moving
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return wrapDiskError(err, "failed to sync temp file")
	}
	tmpFile.Close()

	// Create backup
	if err := m.CreateBackup(ctx); err != nil {
		return fmt.Errorf("failed to create backup: %w", err)
	}

	// Atomic replace (handles cross-filesystem moves)
	if err := atomicMoveFile(tmpPath, m.hostsFile); err != nil {
		return wrapDiskError(err, "failed to replace hosts file")
	}

	// Set proper permissions
	if err := os.Chmod(m.hostsFile, 0644); err != nil {
		return wrapDiskError(err, "failed to set permissions")
	}

	// Validate the written file matches expected content
	if err := m.ValidateWrittenContent(ctx, m.hostsFile, newContent); err != nil {
		return fmt.Errorf("write validation failed: %w", err)
	}

	return nil
}
