package command

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNewRootBuildsValidatedRuntimeConfig(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want Config
	}{
		{
			name: "defaults",
			want: Config{
				Networks:         []string{"saltbox"},
				DefaultNetwork:   "saltbox",
				HostsFile:        "/etc/hosts",
				BackupFile:       "/etc/hosts.backup",
				SectionName:      "DOCKER CONTAINERS",
				PeriodicInterval: 5 * time.Minute,
				DebounceDelay:    time.Second,
				MaxDebounceDelay: 5 * time.Second,
				HealthAddr:       "127.0.0.1",
				HealthPort:       8080,
			},
		},
		{
			name: "preserved short runtime flags",
			args: []string{"-n", "saltbox,backend", "-i", "30s", "-p", "8090"},
			want: Config{
				Networks:         []string{"saltbox", "backend"},
				DefaultNetwork:   "saltbox",
				HostsFile:        "/etc/hosts",
				BackupFile:       "/etc/hosts.backup",
				SectionName:      "DOCKER CONTAINERS",
				PeriodicInterval: 30 * time.Second,
				DebounceDelay:    time.Second,
				MaxDebounceDelay: 5 * time.Second,
				HealthAddr:       "127.0.0.1",
				HealthPort:       8090,
			},
		},
		{
			name: "normalized networks and explicit default",
			args: []string{"--networks", " saltbox, ,backend,saltbox,backend ", "--default-network", " backend "},
			want: Config{
				Networks:         []string{"saltbox", "backend"},
				DefaultNetwork:   "backend",
				HostsFile:        "/etc/hosts",
				BackupFile:       "/etc/hosts.backup",
				SectionName:      "DOCKER CONTAINERS",
				PeriodicInterval: 5 * time.Minute,
				DebounceDelay:    time.Second,
				MaxDebounceDelay: 5 * time.Second,
				HealthAddr:       "127.0.0.1",
				HealthPort:       8080,
			},
		},
		{
			name: "implicit Saltbox preference",
			args: []string{"--networks", "backend,saltbox"},
			want: Config{
				Networks:         []string{"backend", "saltbox"},
				DefaultNetwork:   "saltbox",
				HostsFile:        "/etc/hosts",
				BackupFile:       "/etc/hosts.backup",
				SectionName:      "DOCKER CONTAINERS",
				PeriodicInterval: 5 * time.Minute,
				DebounceDelay:    time.Second,
				MaxDebounceDelay: 5 * time.Second,
				HealthAddr:       "127.0.0.1",
				HealthPort:       8080,
			},
		},
		{
			name: "first network fallback",
			args: []string{"--networks", "backend,frontend"},
			want: Config{
				Networks:         []string{"backend", "frontend"},
				DefaultNetwork:   "backend",
				HostsFile:        "/etc/hosts",
				BackupFile:       "/etc/hosts.backup",
				SectionName:      "DOCKER CONTAINERS",
				PeriodicInterval: 5 * time.Minute,
				DebounceDelay:    time.Second,
				MaxDebounceDelay: 5 * time.Second,
				HealthAddr:       "127.0.0.1",
				HealthPort:       8080,
			},
		},
		{
			name: "all existing runtime flags",
			args: []string{
				"--networks", "backend", "--default-network", "backend",
				"--interval", "1h", "--health-port", "8190", "--health-addr", "0.0.0.0",
				"--hosts-file", "/tmp/hosts", "--backup-file", "/tmp/hosts.backup",
				"--section-name", "CUSTOM", "--debounce-delay", "500ms",
				"--debounce-max-delay", "3s",
			},
			want: Config{
				Networks:         []string{"backend"},
				DefaultNetwork:   "backend",
				HostsFile:        "/tmp/hosts",
				BackupFile:       "/tmp/hosts.backup",
				SectionName:      "CUSTOM",
				PeriodicInterval: time.Hour,
				DebounceDelay:    500 * time.Millisecond,
				MaxDebounceDelay: 3 * time.Second,
				HealthAddr:       "0.0.0.0",
				HealthPort:       8190,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got Config
			var gotContext context.Context
			calls := 0
			run := func(ctx context.Context, cfg Config) error {
				calls++
				gotContext = ctx
				got = cfg
				return nil
			}
			root, stdout, stderr := executeRoot(t, t.Context(), test.args, run, nil)
			if root.err != nil {
				t.Fatalf("ExecuteContext() error = %v", root.err)
			}
			if calls != 1 {
				t.Fatalf("run calls = %d, want 1", calls)
			}
			if gotContext != root.ctx {
				t.Errorf("run context differs from ExecuteContext context")
			}
			if !equalConfig(got, test.want) {
				t.Errorf("run config = %#v, want %#v", got, test.want)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("output = stdout %q stderr %q, want both empty", stdout, stderr)
			}
		})
	}
}

func TestNewRootAcceptsHistoricalSaltboxInvocations(t *testing.T) {
	for _, test := range []struct {
		portArg  string
		wantPort int
	}{
		{portArg: "8090", wantPort: 8090},
		{portArg: "8190", wantPort: 8190},
	} {
		t.Run("health port "+test.portArg, func(t *testing.T) {
			var got Config
			run := func(_ context.Context, cfg Config) error {
				got = cfg
				return nil
			}
			result, _, stderr := executeRoot(t, t.Context(), []string{
				"--networks", "saltbox,backend",
				"--interval", "5m",
				"--health-port", test.portArg,
			}, run, nil)
			if result.err != nil {
				t.Fatalf("historical ExecuteContext() error = %v", result.err)
			}
			if !slices.Equal(got.Networks, []string{"saltbox", "backend"}) {
				t.Errorf("networks = %v, want [saltbox backend]", got.Networks)
			}
			if got.DefaultNetwork != "saltbox" {
				t.Errorf("default network = %q, want saltbox", got.DefaultNetwork)
			}
			if got.PeriodicInterval != 5*time.Minute {
				t.Errorf("interval = %v, want 5m", got.PeriodicInterval)
			}
			if got.HealthPort != test.wantPort {
				t.Errorf("health port = %d, want %d", got.HealthPort, test.wantPort)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestNewRootRejectsExplicitBlankDefaultNetwork(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "empty assignment", args: []string{"--default-network="}},
		{name: "whitespace value", args: []string{"--default-network", "   "}},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			result, stdout, stderr := executeRoot(t, t.Context(), test.args, func(context.Context, Config) error {
				called = true
				return nil
			}, nil)
			if result.err == nil || !strings.Contains(result.err.Error(), "default network") {
				t.Fatalf("ExecuteContext() error = %v, want non-empty default-network error", result.err)
			}
			if called {
				t.Error("run called for explicitly blank default network")
			}
			if stdout != "" || stderr != "" {
				t.Errorf("output = stdout %q stderr %q, want concise returned error only", stdout, stderr)
			}
		})
	}
}

func TestNewRootRejectsExplicitDefaultOutsideNetworks(t *testing.T) {
	called := false
	result, stdout, stderr := executeRoot(t, t.Context(), []string{
		"--networks", "saltbox,backend",
		"--default-network", "frontend",
	}, func(context.Context, Config) error {
		called = true
		return nil
	}, nil)
	if result.err == nil || !strings.Contains(result.err.Error(), "default network") {
		t.Fatalf("ExecuteContext() error = %v, want default-network validation error", result.err)
	}
	if called {
		t.Error("run called for invalid configuration")
	}
	if stdout != "" || stderr != "" {
		t.Errorf("output = stdout %q stderr %q, want concise returned error only", stdout, stderr)
	}
}

func TestNewRootRegenerateRoutesContextAndValidatedPaths(t *testing.T) {
	var gotContext context.Context
	var got RegenerateConfig
	calls := 0
	regenerate := func(ctx context.Context, cfg RegenerateConfig) error {
		calls++
		gotContext = ctx
		got = cfg
		return nil
	}
	result, stdout, stderr := executeRoot(t, t.Context(), []string{
		"regenerate",
		"--hosts-file", "/tmp/dir/../hosts",
		"--backup-file", "/tmp/hosts.backup",
		"--section-name", "CUSTOM",
	}, nil, regenerate)
	if result.err != nil {
		t.Fatalf("ExecuteContext() error = %v", result.err)
	}
	if calls != 1 {
		t.Fatalf("regenerate calls = %d, want 1", calls)
	}
	if gotContext != result.ctx {
		t.Errorf("regenerate context differs from ExecuteContext context")
	}
	want := RegenerateConfig{HostsFile: "/tmp/hosts", BackupFile: "/tmp/hosts.backup", SectionName: "CUSTOM"}
	if got != want {
		t.Errorf("regenerate config = %#v, want %#v", got, want)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("output = stdout %q stderr %q, want both empty", stdout, stderr)
	}
}

func TestNewRootRejectsPositionalArguments(t *testing.T) {
	for _, args := range [][]string{{"unexpected"}, {"regenerate", "unexpected"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			result, stdout, stderr := executeRoot(t, t.Context(), args, nil, nil)
			if result.err == nil {
				t.Fatal("ExecuteContext() error = nil, want positional-argument error")
			}
			if stdout != "" || stderr != "" {
				t.Errorf("output = stdout %q stderr %q, want both empty", stdout, stderr)
			}
		})
	}
}

func TestNewRootPinsVersionOutput(t *testing.T) {
	result, stdout, stderr := executeRoot(t, t.Context(), []string{"--version"}, nil, nil)
	if result.err != nil {
		t.Fatalf("ExecuteContext() error = %v", result.err)
	}
	if stdout != "sdhm version v-test\n" {
		t.Errorf("stdout = %q, want exact three-token version output", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestNewRootPreservesLegacyHelp(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantAll []string
	}{
		{
			name: "root",
			args: []string{"--help"},
			wantAll: []string{
				"A daemon that monitors Docker network events",
				"Features:",
				"Periodic validation to ensure sync",
				"Use 'sdhm regenerate' to reset a corrupted hosts file.",
			},
		},
		{
			name: "regenerate",
			args: []string{"regenerate", "--help"},
			wantAll: []string{
				"Regenerates the hosts file with Ubuntu Server defaults",
				"Standard localhost entries",
				"Empty managed section markers for Docker containers",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, stdout, stderr := executeRoot(t, t.Context(), test.args, nil, nil)
			if result.err != nil {
				t.Fatalf("ExecuteContext() error = %v", result.err)
			}
			for _, want := range test.wantAll {
				if !strings.Contains(stdout, want) {
					t.Errorf("help output missing %q:\n%s", want, stdout)
				}
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestNewRootReturnsRuntimeErrorWithoutUsage(t *testing.T) {
	runtimeErr := errors.New("runtime failed")
	result, stdout, stderr := executeRoot(t, t.Context(), nil, func(context.Context, Config) error {
		return runtimeErr
	}, nil)
	if !errors.Is(result.err, runtimeErr) {
		t.Fatalf("ExecuteContext() error = %v, want %v", result.err, runtimeErr)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("output = stdout %q stderr %q, want returned error without usage", stdout, stderr)
	}
}

func TestNewRootPropagatesCanceledContextAsCleanRunnerResult(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	result, stdout, stderr := executeRoot(t, ctx, nil, func(got context.Context, _ Config) error {
		called = true
		if !errors.Is(got.Err(), context.Canceled) {
			t.Errorf("run context error = %v, want context.Canceled", got.Err())
		}
		return nil
	}, nil)
	if result.err != nil {
		t.Fatalf("ExecuteContext() error = %v, want clean runner result", result.err)
	}
	if !called {
		t.Fatal("run was not called with canceled context")
	}
	if stdout != "" || stderr != "" {
		t.Errorf("output = stdout %q stderr %q, want both empty", stdout, stderr)
	}
}

type executeResult struct {
	ctx context.Context
	err error
}

func executeRoot(
	t *testing.T,
	ctx context.Context,
	args []string,
	run RunFunc,
	regenerate RegenerateFunc,
) (executeResult, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	root := NewRoot("v-test", run, regenerate)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return executeResult{ctx: ctx, err: err}, stdout.String(), stderr.String()
}

func equalConfig(left, right Config) bool {
	return slices.Equal(left.Networks, right.Networks) &&
		left.DefaultNetwork == right.DefaultNetwork &&
		left.HostsFile == right.HostsFile &&
		left.BackupFile == right.BackupFile &&
		left.SectionName == right.SectionName &&
		left.PeriodicInterval == right.PeriodicInterval &&
		left.DebounceDelay == right.DebounceDelay &&
		left.MaxDebounceDelay == right.MaxDebounceDelay &&
		left.HealthAddr == right.HealthAddr &&
		left.HealthPort == right.HealthPort
}
