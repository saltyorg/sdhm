package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRootRejectsOverlappingFilePathsBeforeRunning(t *testing.T) {
	tests := []struct {
		name  string
		paths func(string, string) (string, string)
		want  string
	}{
		{
			name: "relative hosts and absolute backup",
			paths: func(dir, _ string) (string, string) {
				return "hosts", filepath.Join(dir, "hosts")
			},
			want: "different paths",
		},
		{
			name: "absolute hosts and relative backup",
			paths: func(dir, _ string) (string, string) {
				return filepath.Join(dir, "hosts"), "hosts"
			},
			want: "different paths",
		},
		{
			name: "symlinked parent aliases",
			paths: func(dir, alias string) (string, string) {
				return filepath.Join(alias, "hosts"), filepath.Join(dir, "hosts")
			},
			want: "different paths",
		},
		{
			name: "backup is writer lock",
			paths: func(dir, _ string) (string, string) {
				return "hosts", filepath.Join(dir, "hosts.sdhm.lock")
			},
			want: "writer lock",
		},
		{
			name: "backup aliases writer lock through parent",
			paths: func(dir, alias string) (string, string) {
				return filepath.Join(dir, "hosts"), filepath.Join(alias, "hosts.sdhm.lock")
			},
			want: "writer lock",
		},
		{
			name: "missing paths under symlinked parent",
			paths: func(dir, alias string) (string, string) {
				return filepath.Join(dir, "missing", "hosts"), filepath.Join(alias, "missing", "hosts")
			},
			want: "different paths",
		},
		{
			name: "missing writer lock under symlinked parent",
			paths: func(dir, alias string) (string, string) {
				return filepath.Join(dir, "missing", "hosts"), filepath.Join(alias, "missing", "hosts.sdhm.lock")
			},
			want: "writer lock",
		},
	}
	for _, test := range tests {
		for _, regenerate := range []bool{false, true} {
			mode := "daemon"
			if regenerate {
				mode = "regenerate"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(dir, alias); err != nil {
					t.Fatal(err)
				}
				t.Chdir(dir)
				const original = "127.0.0.1 localhost\n"
				for _, name := range []string{"hosts", "hosts.sdhm.lock"} {
					if err := os.WriteFile(name, []byte(original), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				hostsPath, backupPath := test.paths(dir, alias)
				args := []string{"--hosts-file", hostsPath, "--backup-file", backupPath}
				if regenerate {
					args = append([]string{"regenerate"}, args...)
				}
				called := false
				result, stdout, stderr := executeRoot(t, t.Context(), args,
					func(context.Context, Config) error { called = true; return nil },
					func(context.Context, RegenerateConfig) error { called = true; return nil },
				)
				if result.err == nil || !strings.Contains(result.err.Error(), test.want) {
					t.Errorf("ExecuteContext() error = %v, want %q", result.err, test.want)
				}
				if called {
					t.Error("runtime called for overlapping file paths")
				}
				if stdout != "" || stderr != "" {
					t.Errorf("output = stdout %q stderr %q, want returned error only", stdout, stderr)
				}
				for _, name := range []string{"hosts", "hosts.sdhm.lock"} {
					data, err := os.ReadFile(name)
					if err != nil || string(data) != original {
						t.Errorf("fixture %s = %q, error %v, want unchanged contents", name, data, err)
					}
				}
				if _, err := os.Lstat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
					t.Errorf("missing parent error = %v, want no directory created", err)
				}
			})
		}
	}
}

func TestNewRootAcceptsDistinctMissingFilePaths(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	called := false
	result, _, _ := executeRoot(t, t.Context(), []string{
		"regenerate", "--hosts-file", filepath.Join(dir, "missing", "hosts"),
		"--backup-file", filepath.Join(alias, "missing", "backup"),
	}, nil, func(context.Context, RegenerateConfig) error { called = true; return nil })
	if result.err != nil || !called {
		t.Fatalf("ExecuteContext() error = %v, called = %v, want distinct missing paths accepted", result.err, called)
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Errorf("missing parent error = %v, want validation to leave filesystem unchanged", err)
	}
}
