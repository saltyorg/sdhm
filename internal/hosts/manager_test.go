package hosts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createTempFile creates a temporary file with given content for testing
func createTempFile(t *testing.T, content string) string {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "hosts_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		t.Fatalf("Failed to write to temp file: %v", err)
	}

	tmpFile.Close()
	return tmpFile.Name()
}

func TestHostsFileManager_ValidateHostsFile(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantErr     bool
		errContains string
	}{
		{
			name: "valid hosts file with markers",
			content: `127.0.0.1 localhost
::1 localhost ip6-localhost
# BEGIN DOCKER CONTAINERS
# END DOCKER CONTAINERS
`,
			wantErr: false,
		},
		{
			name: "valid hosts file without markers",
			content: `127.0.0.1 localhost
::1 localhost ip6-localhost
`,
			wantErr: false,
		},
		{
			name:        "empty file",
			content:     "",
			wantErr:     true,
			errContains: "empty",
		},
		{
			name: "missing localhost",
			content: `# BEGIN DOCKER CONTAINERS
# END DOCKER CONTAINERS
`,
			wantErr:     true,
			errContains: "localhost",
		},
		{
			name: "BEGIN marker without END",
			content: `127.0.0.1 localhost
# BEGIN DOCKER CONTAINERS
`,
			wantErr:     true,
			errContains: "no END marker",
		},
		{
			name: "END marker without BEGIN",
			content: `127.0.0.1 localhost
# END DOCKER CONTAINERS
`,
			wantErr:     true,
			errContains: "no BEGIN marker",
		},
		{
			name: "END before BEGIN",
			content: `127.0.0.1 localhost
# END DOCKER CONTAINERS
# BEGIN DOCKER CONTAINERS
`,
			wantErr:     true,
			errContains: "before BEGIN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile := createTempFile(t, tt.content)
			defer os.Remove(tmpFile)

			manager := NewHostsFileManager(tmpFile, tmpFile+".backup", "DOCKER CONTAINERS")
			err := manager.ValidateHostsFile(context.Background(), tmpFile)

			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateHostsFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Expected error to contain %q, got %q", tt.errContains, err.Error())
				}
			}
		})
	}
}

func TestHostsFileManager_CreateAndRestoreBackup(t *testing.T) {
	// Create temp directory for test
	tmpDir, err := os.MkdirTemp("", "hosts_backup_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hostsFile := filepath.Join(tmpDir, "hosts")
	backupFile := filepath.Join(tmpDir, "hosts.backup")

	// Create initial hosts file
	initialContent := `127.0.0.1 localhost
::1 localhost
`
	if err := os.WriteFile(hostsFile, []byte(initialContent), 0644); err != nil {
		t.Fatalf("Failed to create hosts file: %v", err)
	}

	manager := NewHostsFileManager(hostsFile, backupFile, "TEST")
	ctx := context.Background()

	// Test backup creation
	if err := manager.CreateBackup(ctx); err != nil {
		t.Fatalf("CreateBackup() failed: %v", err)
	}

	// Verify backup exists and has correct content
	backupData, err := os.ReadFile(backupFile)
	if err != nil {
		t.Fatalf("Failed to read backup file: %v", err)
	}

	if string(backupData) != initialContent {
		t.Errorf("Backup content = %q, want %q", string(backupData), initialContent)
	}

	// Modify original file
	modifiedContent := "modified content\n"
	if err := os.WriteFile(hostsFile, []byte(modifiedContent), 0644); err != nil {
		t.Fatalf("Failed to modify hosts file: %v", err)
	}

	// Test restore
	if err := manager.RestoreBackup(ctx); err != nil {
		t.Fatalf("RestoreBackup() failed: %v", err)
	}

	// Verify restored content
	restoredData, err := os.ReadFile(hostsFile)
	if err != nil {
		t.Fatalf("Failed to read restored file: %v", err)
	}

	if string(restoredData) != initialContent {
		t.Errorf("Restored content = %q, want %q", string(restoredData), initialContent)
	}
}

func TestHostsFileManager_FixNonBreakingSpaces(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hosts_nbsp_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hostsFile := filepath.Join(tmpDir, "hosts")
	backupFile := filepath.Join(tmpDir, "hosts.backup")

	// Content with non-breaking space (U+00A0)
	contentWithNBSP := "127.0.0.1\u00a0localhost\n"
	expectedContent := "127.0.0.1 localhost\n"

	if err := os.WriteFile(hostsFile, []byte(contentWithNBSP), 0644); err != nil {
		t.Fatalf("Failed to create hosts file: %v", err)
	}

	manager := NewHostsFileManager(hostsFile, backupFile, "TEST")
	ctx := context.Background()

	// Fix non-breaking spaces
	if err := manager.FixNonBreakingSpaces(ctx); err != nil {
		t.Fatalf("FixNonBreakingSpaces() failed: %v", err)
	}

	// Verify content is fixed
	fixedData, err := os.ReadFile(hostsFile)
	if err != nil {
		t.Fatalf("Failed to read fixed file: %v", err)
	}

	if string(fixedData) != expectedContent {
		t.Errorf("Fixed content = %q, want %q", string(fixedData), expectedContent)
	}
}

func TestHostsFileManager_EnsureManagedSectionExists(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hosts_section_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hostsFile := filepath.Join(tmpDir, "hosts")
	backupFile := filepath.Join(tmpDir, "hosts.backup")

	initialContent := "127.0.0.1 localhost\n"
	if err := os.WriteFile(hostsFile, []byte(initialContent), 0644); err != nil {
		t.Fatalf("Failed to create hosts file: %v", err)
	}

	manager := NewHostsFileManager(hostsFile, backupFile, "DOCKER CONTAINERS")
	ctx := context.Background()

	// Ensure section exists
	if err := manager.EnsureManagedSectionExists(ctx); err != nil {
		t.Fatalf("EnsureManagedSectionExists() failed: %v", err)
	}

	// Verify markers were added
	data, err := os.ReadFile(hostsFile)
	if err != nil {
		t.Fatalf("Failed to read hosts file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "# BEGIN DOCKER CONTAINERS") {
		t.Error("BEGIN marker not found in hosts file")
	}
	if !strings.Contains(content, "# END DOCKER CONTAINERS") {
		t.Error("END marker not found in hosts file")
	}

	// Call again - should be idempotent
	if err := manager.EnsureManagedSectionExists(ctx); err != nil {
		t.Fatalf("Second EnsureManagedSectionExists() failed: %v", err)
	}
}

func TestHostsFileManager_UpdateManagedSection(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hosts_update_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hostsFile := filepath.Join(tmpDir, "hosts")
	backupFile := filepath.Join(tmpDir, "hosts.backup")

	initialContent := `127.0.0.1 localhost
::1 localhost
# BEGIN DOCKER CONTAINERS
# END DOCKER CONTAINERS
`

	if err := os.WriteFile(hostsFile, []byte(initialContent), 0644); err != nil {
		t.Fatalf("Failed to create hosts file: %v", err)
	}

	manager := NewHostsFileManager(hostsFile, backupFile, "DOCKER CONTAINERS")
	ctx := context.Background()

	// Create test entries
	entries := []HostEntry{
		{
			IP:        []byte{172, 20, 0, 2},
			Hostnames: []string{"app1", "app1.saltbox"},
		},
		{
			IP:        []byte{172, 20, 0, 3},
			Hostnames: []string{"app2", "app2.saltbox"},
		},
	}

	// Update managed section
	if err := manager.UpdateManagedSection(ctx, entries); err != nil {
		t.Fatalf("UpdateManagedSection() failed: %v", err)
	}

	// Verify content
	data, err := os.ReadFile(hostsFile)
	if err != nil {
		t.Fatalf("Failed to read hosts file: %v", err)
	}

	content := string(data)

	// Check that original content is preserved
	if !strings.Contains(content, "127.0.0.1 localhost") {
		t.Error("Original localhost entry missing")
	}

	// Check that managed section has new entries
	if !strings.Contains(content, "172.20.0.2 app1 app1.saltbox") {
		t.Error("Expected app1 entry not found")
	}
	if !strings.Contains(content, "172.20.0.3 app2 app2.saltbox") {
		t.Error("Expected app2 entry not found")
	}

	// Check markers are intact
	if !strings.Contains(content, "# BEGIN DOCKER CONTAINERS") || !strings.Contains(content, "# END DOCKER CONTAINERS") {
		t.Error("Markers missing after update")
	}
}
