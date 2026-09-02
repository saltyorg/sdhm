package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
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

func TestLogStartupIncludesRuntimeConfiguration(t *testing.T) {
	cfg := validCommandConfig()
	cfg.Networks = []string{"backend", "frontend"}
	cfg.DefaultNetwork = "frontend"
	cfg.PeriodicInterval = 7 * time.Minute
	cfg.HealthAddr = "2001:db8::1"
	cfg.HealthPort = 9090
	recorder := newRunLogRecorder()

	logStartup(slog.New(recorder), cfg)

	records := recorder.Records()
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	record := records[0]
	if record.Level != slog.LevelInfo || record.Message != "starting SDHM" {
		t.Fatalf("record = (%v, %q), want INFO starting SDHM", record.Level, record.Message)
	}
	if got := runLogStringAttr(t, record, "version"); got != version {
		t.Errorf("version = %q, want %q", got, version)
	}
	if got := runLogStringSliceAttr(t, record, "networks"); !slices.Equal(got, cfg.Networks) {
		t.Errorf("networks = %q, want %q", got, cfg.Networks)
	}
	if got := runLogStringAttr(t, record, "default_network"); got != cfg.DefaultNetwork {
		t.Errorf("default_network = %q, want %q", got, cfg.DefaultNetwork)
	}
	if got := runLogDurationAttr(t, record, "interval"); got != cfg.PeriodicInterval {
		t.Errorf("interval = %v, want %v", got, cfg.PeriodicInterval)
	}
	wantHealthAddr := "[2001:db8::1]:" + strconv.Itoa(cfg.HealthPort)
	if got := runLogStringAttr(t, record, "health_addr"); got != wantHealthAddr {
		t.Errorf("health_addr = %q, want %q", got, wantHealthAddr)
	}
}

func TestRunDaemonWithLogsCleanStop(t *testing.T) {
	sentinel := errors.New("runner sentinel")
	tests := []struct {
		name      string
		runnerErr error
		wantStops int
	}{
		{name: "clean stop", wantStops: 1},
		{name: "runner failure", runnerErr: sentinel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := newRunLogRecorder()
			wiring := daemonWiring{
				newDocker: func() (daemon.NetworkSource, error) { return &wiringNetworkSource{}, nil },
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
					return daemonRunnerFunc(func(context.Context) error { return tt.runnerErr }), nil
				},
			}

			err := runDaemonWith(t.Context(), validCommandConfig(), slog.New(recorder), wiring)
			if err != tt.runnerErr {
				t.Fatalf("runDaemonWith() error = %v, want exact %v", err, tt.runnerErr)
			}

			stops := 0
			for _, record := range recorder.Records() {
				if record.Level == slog.LevelInfo && record.Message == "SDHM stopped" {
					stops++
				}
			}
			if stops != tt.wantStops {
				t.Fatalf("SDHM stopped records = %d, want %d", stops, tt.wantStops)
			}
		})
	}
}

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

type runLogRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func newRunLogRecorder() *runLogRecorder {
	return &runLogRecorder{}
}

func (*runLogRecorder) Enabled(context.Context, slog.Level) bool {
	return true
}

func (r *runLogRecorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record.Clone())
	return nil
}

func (r *runLogRecorder) WithAttrs([]slog.Attr) slog.Handler {
	return r
}

func (r *runLogRecorder) WithGroup(string) slog.Handler {
	return r
}

func (r *runLogRecorder) Records() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()

	records := make([]slog.Record, len(r.records))
	for i, record := range r.records {
		records[i] = record.Clone()
	}
	return records
}

func runLogAttr(t *testing.T, record slog.Record, key string) slog.Value {
	t.Helper()
	var value slog.Value
	found := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			value = attr.Value
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("record %q is missing %q", record.Message, key)
	}
	return value
}

func runLogStringAttr(t *testing.T, record slog.Record, key string) string {
	t.Helper()
	value := runLogAttr(t, record, key)
	if value.Kind() != slog.KindString {
		t.Fatalf("record %q %s kind = %v, want string", record.Message, key, value.Kind())
	}
	return value.String()
}

func runLogStringSliceAttr(t *testing.T, record slog.Record, key string) []string {
	t.Helper()
	value := runLogAttr(t, record, key)
	networks, ok := value.Any().([]string)
	if !ok {
		t.Fatalf("record %q %s = %T, want []string", record.Message, key, value.Any())
	}
	return networks
}

func runLogDurationAttr(t *testing.T, record slog.Record, key string) time.Duration {
	t.Helper()
	value := runLogAttr(t, record, key)
	if value.Kind() != slog.KindDuration {
		t.Fatalf("record %q %s kind = %v, want duration", record.Message, key, value.Kind())
	}
	return value.Duration()
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

func (*wiringHostStore) Prepare(context.Context) (daemon.PrepareResult, error) {
	return daemon.PrepareResult{}, nil
}

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
