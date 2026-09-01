package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/saltyorg/sdhm/command"
	"github.com/saltyorg/sdhm/daemon"
	"github.com/saltyorg/sdhm/docker"
	"github.com/saltyorg/sdhm/health"
	"github.com/saltyorg/sdhm/hosts"
)

var _ daemon.NetworkSource = (*docker.Client)(nil)
var _ daemon.HostStore = (*hosts.Store)(nil)
var _ daemon.HealthServer = (*health.Server)(nil)

func TestRunDaemonWithWiresDependenciesInConstructionOrder(t *testing.T) {
	cfg := command.Config{
		Networks:         []string{"backend", "frontend"},
		DefaultNetwork:   "frontend",
		HostsFile:        "/tmp/hosts",
		BackupFile:       "/tmp/hosts.backup",
		SectionName:      "CONTAINERS",
		PeriodicInterval: 7 * time.Minute,
		DebounceDelay:    250 * time.Millisecond,
		MaxDebounceDelay: 2 * time.Second,
		HealthAddr:       "2001:db8::1",
		HealthPort:       9090,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	source := &wiringNetworkSource{}
	store := &wiringHostStore{}
	server := newWiringHealthServer()

	var calls []string
	var gotStoreArgs []string
	var gotServerAddr string
	var gotServerHandler http.Handler
	var gotDaemonConfig daemon.Config
	var gotRunContext context.Context
	wiring := daemonWiring{
		newDocker: func() (daemon.NetworkSource, error) {
			calls = append(calls, "docker")
			return source, nil
		},
		newStore: func(hostsPath, backupPath, sectionName, defaultNetwork string) daemon.HostStore {
			calls = append(calls, "store")
			gotStoreArgs = []string{hostsPath, backupPath, sectionName, defaultNetwork}
			return store
		},
		newTracker: func() *health.Tracker {
			calls = append(calls, "tracker")
			return health.NewTracker()
		},
		newHandler: func(tracker *health.Tracker) http.Handler {
			calls = append(calls, "handler")
			return health.NewHandler(tracker)
		},
		newHealthServer: func(addr string, handler http.Handler) daemon.HealthServer {
			calls = append(calls, "health-server")
			gotServerAddr = addr
			gotServerHandler = handler
			return server
		},
		newDaemon: func(
			got daemon.Config,
			gotSource daemon.NetworkSource,
			gotStore daemon.HostStore,
			tracker *health.Tracker,
			gotServer daemon.HealthServer,
			gotLogger *slog.Logger,
		) (daemonRunner, error) {
			calls = append(calls, "daemon")
			gotDaemonConfig = got
			if gotSource != source {
				t.Error("daemon source is not the constructed Docker source")
			}
			if gotStore != store {
				t.Error("daemon store is not the constructed hosts store")
			}
			if tracker == nil {
				t.Error("daemon tracker is nil")
			}
			if gotServer != server {
				t.Error("daemon server is not the constructed health server")
			}
			if gotLogger != logger {
				t.Error("daemon logger is not the wiring logger")
			}
			return daemonRunnerFunc(func(ctx context.Context) error {
				calls = append(calls, "run")
				gotRunContext = ctx
				return nil
			}), nil
		},
	}

	ctx := t.Context()
	if err := runDaemonWith(ctx, cfg, logger, wiring); err != nil {
		t.Fatalf("runDaemonWith() error = %v", err)
	}

	wantCalls := []string{"docker", "store", "tracker", "handler", "health-server", "daemon", "run"}
	if !slices.Equal(calls, wantCalls) {
		t.Errorf("construction calls = %v, want %v", calls, wantCalls)
	}
	wantStoreArgs := []string{"/tmp/hosts", "/tmp/hosts.backup", "CONTAINERS", "frontend"}
	if !slices.Equal(gotStoreArgs, wantStoreArgs) {
		t.Errorf("store constructor args = %q, want %q", gotStoreArgs, wantStoreArgs)
	}
	if gotServerAddr != "[2001:db8::1]:9090" {
		t.Errorf("health server address = %q, want %q", gotServerAddr, "[2001:db8::1]:9090")
	}
	if gotRunContext != ctx {
		t.Error("daemon did not receive the command context")
	}
	assertDaemonConfig(t, gotDaemonConfig, cfg)
	assertHealthRoute(t, gotServerHandler)
	if source.closeCalls != 0 {
		t.Errorf("Docker close calls after ownership transfer = %d, want 0 from wiring", source.closeCalls)
	}
}

func TestRunDaemonWithClosesDockerWhenDaemonConstructionFails(t *testing.T) {
	constructErr := errors.New("construct daemon")
	closeErr := errors.New("close Docker")
	source := &wiringNetworkSource{closeErr: closeErr}
	wiring := daemonWiring{
		newDocker: func() (daemon.NetworkSource, error) { return source, nil },
		newStore: func(string, string, string, string) daemon.HostStore {
			return &wiringHostStore{}
		},
		newTracker: health.NewTracker,
		newHandler: health.NewHandler,
		newHealthServer: func(string, http.Handler) daemon.HealthServer {
			return newWiringHealthServer()
		},
		newDaemon: func(
			daemon.Config,
			daemon.NetworkSource,
			daemon.HostStore,
			*health.Tracker,
			daemon.HealthServer,
			*slog.Logger,
		) (daemonRunner, error) {
			return nil, constructErr
		},
	}

	err := runDaemonWith(
		t.Context(),
		validCommandConfig(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		wiring,
	)
	if !errors.Is(err, constructErr) {
		t.Errorf("runDaemonWith() error = %v, want construction error", err)
	}
	if !errors.Is(err, closeErr) {
		t.Errorf("runDaemonWith() error = %v, want Docker close error", err)
	}
	if source.closeCalls != 1 {
		t.Errorf("Docker close calls = %d, want 1", source.closeCalls)
	}
}

func TestRegenerateHostsWithUsesDefaultIndependentStore(t *testing.T) {
	cfg := command.RegenerateConfig{
		HostsFile:   "/tmp/hosts",
		BackupFile:  "/tmp/hosts.backup",
		SectionName: "CONTAINERS",
	}
	regenerator := &wiringRegenerator{}
	var gotArgs []string

	err := regenerateHostsWith(t.Context(), cfg, func(hostsPath, backupPath, sectionName, defaultNetwork string) hostsRegenerator {
		gotArgs = []string{hostsPath, backupPath, sectionName, defaultNetwork}
		return regenerator
	})
	if err != nil {
		t.Fatalf("regenerateHostsWith() error = %v", err)
	}

	wantArgs := []string{"/tmp/hosts", "/tmp/hosts.backup", "CONTAINERS", ""}
	if !slices.Equal(gotArgs, wantArgs) {
		t.Errorf("store constructor args = %q, want %q", gotArgs, wantArgs)
	}
	if regenerator.calls != 1 {
		t.Errorf("Regenerate() calls = %d, want 1", regenerator.calls)
	}
}

func validCommandConfig() command.Config {
	return command.Config{
		Networks:         []string{"saltbox"},
		DefaultNetwork:   "saltbox",
		HostsFile:        "/tmp/hosts",
		BackupFile:       "/tmp/hosts.backup",
		SectionName:      "CONTAINERS",
		PeriodicInterval: time.Minute,
		DebounceDelay:    time.Second,
		MaxDebounceDelay: 5 * time.Second,
		HealthAddr:       "127.0.0.1",
		HealthPort:       8080,
	}
}

func assertDaemonConfig(t *testing.T, got daemon.Config, want command.Config) {
	t.Helper()
	if !slices.Equal(got.Networks, want.Networks) {
		t.Errorf("daemon networks = %q, want %q", got.Networks, want.Networks)
	}
	if got.DefaultNetwork != want.DefaultNetwork {
		t.Errorf("daemon default network = %q, want %q", got.DefaultNetwork, want.DefaultNetwork)
	}
	if got.PeriodicInterval != want.PeriodicInterval {
		t.Errorf("daemon periodic interval = %v, want %v", got.PeriodicInterval, want.PeriodicInterval)
	}
	if got.DebounceDelay != want.DebounceDelay {
		t.Errorf("daemon debounce delay = %v, want %v", got.DebounceDelay, want.DebounceDelay)
	}
	if got.MaxDebounceDelay != want.MaxDebounceDelay {
		t.Errorf("daemon maximum debounce delay = %v, want %v", got.MaxDebounceDelay, want.MaxDebounceDelay)
	}
}

func assertHealthRoute(t *testing.T, handler http.Handler) {
	t.Helper()
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{name: "exact get", method: http.MethodGet, path: "/health", wantStatus: http.StatusOK},
		{name: "wrong method", method: http.MethodPost, path: "/health", wantStatus: http.StatusMethodNotAllowed},
		{name: "wrong path", method: http.MethodGet, path: "/", wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, tt.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.wantStatus {
				t.Errorf("%s %s status = %d, want %d", tt.method, tt.path, response.Code, tt.wantStatus)
			}
		})
	}
}

type daemonRunnerFunc func(context.Context) error

func (f daemonRunnerFunc) Run(ctx context.Context) error {
	return f(ctx)
}

type wiringNetworkSource struct {
	closeErr   error
	closeCalls int
}

func (*wiringNetworkSource) Ping(context.Context) error { return nil }

func (*wiringNetworkSource) Snapshot(context.Context, []string) ([]daemon.Endpoint, error) {
	return nil, nil
}

func (*wiringNetworkSource) Events(context.Context, []string) (<-chan daemon.Event, <-chan error) {
	events := make(chan daemon.Event)
	errs := make(chan error)
	close(events)
	close(errs)
	return events, errs
}

func (s *wiringNetworkSource) Close() error {
	s.closeCalls++
	return s.closeErr
}

type wiringHostStore struct{}

func (*wiringHostStore) Prepare(context.Context) error { return nil }

func (*wiringHostStore) Apply(context.Context, []daemon.Endpoint) error { return nil }

type wiringHealthServer struct {
	done chan struct{}
}

func newWiringHealthServer() *wiringHealthServer {
	return &wiringHealthServer{done: make(chan struct{})}
}

func (*wiringHealthServer) Start() error { return nil }

func (s *wiringHealthServer) Done() <-chan struct{} { return s.done }

func (*wiringHealthServer) Err() error { return nil }

func (*wiringHealthServer) Shutdown(context.Context) error { return nil }

type wiringRegenerator struct {
	calls int
}

func (r *wiringRegenerator) Regenerate(context.Context) error {
	r.calls++
	return nil
}
