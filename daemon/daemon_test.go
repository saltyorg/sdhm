package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saltyorg/sdhm/health"
)

type operationLog struct {
	mu    sync.Mutex
	names []string
}

func (l *operationLog) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

func (l *operationLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.names)
}

type orderedSource struct {
	log *operationLog

	pingErr     error
	snapshotErr error
	closeErr    error
	endpoints   []Endpoint
	onPing      func(context.Context)
	onSnapshot  func(context.Context)

	mu               sync.Mutex
	pingContexts     []context.Context
	snapshotContexts []context.Context
	snapshotNetworks [][]string
}

func (s *orderedSource) Ping(ctx context.Context) error {
	s.log.add("ping")
	s.mu.Lock()
	s.pingContexts = append(s.pingContexts, ctx)
	s.mu.Unlock()
	if s.onPing != nil {
		s.onPing(ctx)
	}
	return s.pingErr
}

func (s *orderedSource) Snapshot(ctx context.Context, networks []string) ([]Endpoint, error) {
	s.log.add("snapshot")
	s.mu.Lock()
	s.snapshotContexts = append(s.snapshotContexts, ctx)
	s.snapshotNetworks = append(s.snapshotNetworks, slices.Clone(networks))
	s.mu.Unlock()
	if s.onSnapshot != nil {
		s.onSnapshot(ctx)
	}
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	return cloneEndpoints(s.endpoints), nil
}

func (*orderedSource) Events(context.Context, []string) (<-chan Event, <-chan error) {
	return nil, nil
}

func (s *orderedSource) Close() error {
	s.log.add("source_close")
	return s.closeErr
}

func (s *orderedSource) calls() ([]context.Context, []context.Context, [][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.pingContexts), slices.Clone(s.snapshotContexts), cloneStrings2D(s.snapshotNetworks)
}

type orderedStore struct {
	log *operationLog

	prepareErr error
	applyErr   error
	onPrepare  func(context.Context)
	onApply    func(context.Context)

	mu             sync.Mutex
	applyContexts  []context.Context
	applyEndpoints [][]Endpoint
	current        []Endpoint
}

func (s *orderedStore) Prepare(ctx context.Context) error {
	s.log.add("hosts_prepare")
	if s.onPrepare != nil {
		s.onPrepare(ctx)
	}
	return s.prepareErr
}

func (s *orderedStore) Apply(ctx context.Context, endpoints []Endpoint) error {
	s.log.add("hosts_apply")
	s.mu.Lock()
	s.applyContexts = append(s.applyContexts, ctx)
	s.applyEndpoints = append(s.applyEndpoints, cloneEndpoints(endpoints))
	s.mu.Unlock()
	if s.onApply != nil {
		s.onApply(ctx)
	}
	if s.applyErr != nil {
		return s.applyErr
	}
	s.mu.Lock()
	s.current = cloneEndpoints(endpoints)
	s.mu.Unlock()
	return nil
}

func (s *orderedStore) calls() ([]context.Context, [][]Endpoint, []Endpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.applyContexts), cloneEndpoints2D(s.applyEndpoints), cloneEndpoints(s.current)
}

type orderedHealthServer struct {
	log  *operationLog
	done chan struct{}

	startErr    error
	shutdownErr error
	onStart     func()
	onErr       func()
	onShutdown  func(context.Context)

	finishOnce sync.Once
	mu         sync.Mutex
	err        error
	shutdown   []context.Context
	shutdownIn []error
	errCalls   int
}

func newOrderedHealthServer(log *operationLog) *orderedHealthServer {
	return &orderedHealthServer{log: log, done: make(chan struct{})}
}

func (s *orderedHealthServer) Start() error {
	s.log.add("health_start")
	if s.onStart != nil {
		s.onStart()
	}
	return s.startErr
}

func (s *orderedHealthServer) Done() <-chan struct{} {
	return s.done
}

func (s *orderedHealthServer) Err() error {
	if s.onErr != nil {
		s.onErr()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errCalls++
	return s.err
}

func (s *orderedHealthServer) Shutdown(ctx context.Context) error {
	s.log.add("health_shutdown")
	s.mu.Lock()
	s.shutdown = append(s.shutdown, ctx)
	s.shutdownIn = append(s.shutdownIn, ctx.Err())
	s.mu.Unlock()
	if s.onShutdown != nil {
		s.onShutdown(ctx)
	}
	s.finish(nil)
	return s.shutdownErr
}

func (s *orderedHealthServer) finish(err error) {
	s.finishOnce.Do(func() {
		s.mu.Lock()
		if err != nil || s.err == nil {
			s.err = err
		}
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *orderedHealthServer) setTerminalError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *orderedHealthServer) shutdownCalls() ([]context.Context, []error, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.shutdown), slices.Clone(s.shutdownIn), s.errCalls
}

func TestNewValidatesDependenciesConfigurationAndClonesNetworks(t *testing.T) {
	valid := validConfig()
	log := &operationLog{}
	source := &orderedSource{log: log}
	store := &orderedStore{log: log}
	tracker := health.NewTracker()
	server := newOrderedHealthServer(log)
	logger := testLogger()

	tests := []struct {
		name    string
		cfg     Config
		source  NetworkSource
		store   HostStore
		tracker *health.Tracker
		server  HealthServer
		logger  *slog.Logger
	}{
		{name: "nil source", cfg: valid, store: store, tracker: tracker, server: server, logger: logger},
		{name: "typed nil source", cfg: valid, source: (*orderedSource)(nil), store: store, tracker: tracker, server: server, logger: logger},
		{name: "nil store", cfg: valid, source: source, tracker: tracker, server: server, logger: logger},
		{name: "nil tracker", cfg: valid, source: source, store: store, server: server, logger: logger},
		{name: "nil server", cfg: valid, source: source, store: store, tracker: tracker, logger: logger},
		{name: "typed nil server", cfg: valid, source: source, store: store, tracker: tracker, server: (*orderedHealthServer)(nil), logger: logger},
		{name: "nil logger", cfg: valid, source: source, store: store, tracker: tracker, server: server},
		{name: "no networks", cfg: configWith(valid, func(cfg *Config) { cfg.Networks = nil }), source: source, store: store, tracker: tracker, server: server, logger: logger},
		{name: "empty network", cfg: configWith(valid, func(cfg *Config) { cfg.Networks = []string{""} }), source: source, store: store, tracker: tracker, server: server, logger: logger},
		{name: "untrimmed network", cfg: configWith(valid, func(cfg *Config) { cfg.Networks = []string{" saltbox"} }), source: source, store: store, tracker: tracker, server: server, logger: logger},
		{name: "duplicate network", cfg: configWith(valid, func(cfg *Config) { cfg.Networks = []string{"saltbox", "saltbox"} }), source: source, store: store, tracker: tracker, server: server, logger: logger},
		{name: "default absent", cfg: configWith(valid, func(cfg *Config) { cfg.DefaultNetwork = "missing" }), source: source, store: store, tracker: tracker, server: server, logger: logger},
		{name: "periodic interval not positive", cfg: configWith(valid, func(cfg *Config) { cfg.PeriodicInterval = 0 }), source: source, store: store, tracker: tracker, server: server, logger: logger},
		{name: "debounce delay not positive", cfg: configWith(valid, func(cfg *Config) { cfg.DebounceDelay = -time.Second }), source: source, store: store, tracker: tracker, server: server, logger: logger},
		{name: "maximum debounce delay not positive", cfg: configWith(valid, func(cfg *Config) { cfg.MaxDebounceDelay = 0 }), source: source, store: store, tracker: tracker, server: server, logger: logger},
		{name: "maximum debounce below debounce", cfg: configWith(valid, func(cfg *Config) { cfg.MaxDebounceDelay = cfg.DebounceDelay / 2 }), source: source, store: store, tracker: tracker, server: server, logger: logger},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg, tt.source, tt.store, tt.tracker, tt.server, tt.logger); err == nil {
				t.Fatal("New() error = nil, want validation error")
			}
		})
	}

	daemon, err := New(valid, source, store, tracker, server, logger)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	valid.Networks[0] = "mutated"
	if err := daemon.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	_, _, networks := source.calls()
	if len(networks) != 1 || !slices.Equal(networks[0], []string{"saltbox", "backend"}) {
		t.Fatalf("Snapshot() networks = %v, want cloned [saltbox backend]", networks)
	}
}

func TestRunSuccessfulStartupUsesExactOrderAndTimeouts(t *testing.T) {
	log := &operationLog{}
	source := &orderedSource{
		log: log,
		endpoints: []Endpoint{
			{Network: "saltbox", IP: netip.MustParseAddr("172.19.0.2"), Aliases: []string{"radarr"}},
			{Network: "backend", IP: netip.MustParseAddr("172.20.0.2"), Aliases: []string{"postgres"}},
		},
	}
	store := &orderedStore{log: log}
	server := newOrderedHealthServer(log)
	ctx, cancel := context.WithCancel(t.Context())
	store.onApply = func(context.Context) { cancel() }
	daemon := mustNewDaemon(t, validConfig(), source, store, health.NewTracker(), server)

	started := time.Now()
	if err := daemon.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	finished := time.Now()

	assertOperations(t, log, []string{
		"ping", "health_start", "hosts_prepare", "snapshot", "hosts_apply",
		"health_shutdown", "source_close",
	})
	pingContexts, snapshotContexts, networks := source.calls()
	if len(pingContexts) != 1 || len(snapshotContexts) != 1 {
		t.Fatalf("Docker context calls = ping %d snapshot %d, want 1 each", len(pingContexts), len(snapshotContexts))
	}
	assertDeadlineBetween(t, pingContexts[0], started.Add(dockerOperationTimeout), finished.Add(dockerOperationTimeout))
	assertDeadlineBetween(t, snapshotContexts[0], started.Add(dockerOperationTimeout), finished.Add(dockerOperationTimeout))
	if !slices.Equal(networks[0], []string{"saltbox", "backend"}) {
		t.Fatalf("Snapshot() networks = %v, want [saltbox backend]", networks[0])
	}
	applyContexts, applied, _ := store.calls()
	if len(applyContexts) != 1 || applyContexts[0].Err() != context.Canceled {
		t.Fatalf("Apply() context error = %v, want canceled daemon context after Run", applyContexts[0].Err())
	}
	assertEndpointSlices(t, applied[0], source.endpoints)
	shutdownContexts, shutdownInputErrors, _ := server.shutdownCalls()
	if len(shutdownContexts) != 1 {
		t.Fatalf("Shutdown() calls = %d, want 1", len(shutdownContexts))
	}
	if shutdownInputErrors[0] != nil {
		t.Fatalf("Shutdown() received canceled context: %v", shutdownInputErrors[0])
	}
	assertDeadlineBetween(t, shutdownContexts[0], started.Add(shutdownTimeout), finished.Add(shutdownTimeout))
}

func TestReconcilePassesCompleteSnapshotAndUpdatesExactHealthConcerns(t *testing.T) {
	log := &operationLog{}
	snapshotErr := errors.New("snapshot sentinel")
	applyErr := errors.New("apply sentinel")
	source := &orderedSource{log: log, snapshotErr: snapshotErr}
	store := &orderedStore{
		log:      log,
		applyErr: applyErr,
		current:  []Endpoint{{Network: "saltbox", IP: netip.MustParseAddr("172.19.0.9"), Aliases: []string{"old"}}},
	}
	tracker := health.NewTracker()
	daemon := mustNewDaemon(t, validConfig(), source, store, tracker, newOrderedHealthServer(log))

	if err := daemon.reconcile(t.Context()); !errors.Is(err, snapshotErr) {
		t.Fatalf("reconcile() error = %v, want wrapped snapshot error", err)
	}
	_, applied, current := store.calls()
	if len(applied) != 0 {
		t.Fatalf("Apply() calls = %d after snapshot failure, want 0", len(applied))
	}
	assertActiveConcerns(t, tracker, health.ConcernDockerSnapshot)
	assertNewestHealthMessage(t, tracker, snapshotErr.Error())
	assertEndpointSlices(t, current, store.current)

	source.snapshotErr = nil
	source.endpoints = []Endpoint{
		{Network: "saltbox", IP: netip.MustParseAddr("172.19.0.2"), Aliases: []string{"radarr"}},
		{Network: "backend", IP: netip.MustParseAddr("172.20.0.3"), Aliases: []string{"sonarr"}},
	}
	if err := daemon.reconcile(t.Context()); !errors.Is(err, applyErr) {
		t.Fatalf("reconcile() error = %v, want wrapped apply error", err)
	}
	assertActiveConcerns(t, tracker, health.ConcernHostsApply)
	assertNewestHealthMessage(t, tracker, applyErr.Error())
	_, _, current = store.calls()
	if len(current) != 1 || current[0].Aliases[0] != "old" {
		t.Fatalf("store state = %+v after Apply failure, want old state", current)
	}

	store.applyErr = nil
	if err := daemon.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile() recovery error = %v", err)
	}
	assertActiveConcerns(t, tracker)
	_, snapshotContexts, networks := source.calls()
	if len(snapshotContexts) != 3 {
		t.Fatalf("Snapshot() calls = %d, want 3", len(snapshotContexts))
	}
	for i, names := range networks {
		if !slices.Equal(names, []string{"saltbox", "backend"}) {
			t.Fatalf("Snapshot() call %d networks = %v, want [saltbox backend]", i, names)
		}
		if _, ok := snapshotContexts[i].Deadline(); !ok {
			t.Fatalf("Snapshot() call %d context has no deadline", i)
		}
	}
	_, applied, current = store.calls()
	if len(applied) != 2 {
		t.Fatalf("Apply() calls = %d, want 2", len(applied))
	}
	assertEndpointSlices(t, applied[1], source.endpoints)
	assertEndpointSlices(t, current, source.endpoints)
}

func TestRunInitialReconciliationFailureRemainsDegradedUntilAnotherStopCondition(t *testing.T) {
	tests := []struct {
		name      string
		concern   health.Concern
		configure func(*orderedSource, *orderedStore, <-chan struct{}, chan<- struct{}) error
		wantApply bool
	}{
		{
			name:    "snapshot failure",
			concern: health.ConcernDockerSnapshot,
			configure: func(source *orderedSource, _ *orderedStore, release <-chan struct{}, started chan<- struct{}) error {
				source.snapshotErr = errors.New("snapshot sentinel")
				source.onSnapshot = func(context.Context) {
					close(started)
					<-release
				}
				return source.snapshotErr
			},
		},
		{
			name:    "apply failure",
			concern: health.ConcernHostsApply,
			configure: func(_ *orderedSource, store *orderedStore, release <-chan struct{}, started chan<- struct{}) error {
				store.applyErr = errors.New("apply sentinel")
				store.onApply = func(context.Context) {
					close(started)
					<-release
				}
				return store.applyErr
			},
			wantApply: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &operationLog{}
			source := &orderedSource{log: log}
			store := &orderedStore{log: log}
			server := newOrderedHealthServer(log)
			started := make(chan struct{})
			release := make(chan struct{})
			reconcileErr := tt.configure(source, store, release, started)
			tracker := health.NewTracker()
			daemon := mustNewDaemon(t, validConfig(), source, store, tracker, server)
			result := make(chan error, 1)
			go func() { result <- daemon.Run(t.Context()) }()

			<-started
			serveErr := errors.New("serve sentinel")
			server.finish(serveErr)
			close(release)
			err := <-result
			if !errors.Is(err, serveErr) {
				t.Fatalf("Run() error = %v, want later health stop error", err)
			}
			if errors.Is(err, reconcileErr) {
				t.Fatalf("Run() error = %v, must not use initial reconciliation failure as initiating error", err)
			}
			assertActiveConcerns(t, tracker, tt.concern)
			wantOps := []string{"ping", "health_start", "hosts_prepare", "snapshot"}
			if tt.wantApply {
				wantOps = append(wantOps, "hosts_apply")
			}
			wantOps = append(wantOps, "health_shutdown", "source_close")
			assertOperations(t, log, wantOps)
		})
	}
}

func TestRunStartupFailuresCleanUpOnlyOwnedResources(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*orderedSource, *orderedStore, *orderedHealthServer) error
		wantOps    []string
		wantActive []health.Concern
	}{
		{
			name: "ping failure closes source",
			configure: func(source *orderedSource, _ *orderedStore, _ *orderedHealthServer) error {
				source.pingErr = errors.New("ping sentinel")
				return source.pingErr
			},
			wantOps: []string{"ping", "source_close"},
		},
		{
			name: "health start failure closes source",
			configure: func(_ *orderedSource, _ *orderedStore, server *orderedHealthServer) error {
				server.startErr = errors.New("start sentinel")
				return server.startErr
			},
			wantOps: []string{"ping", "health_start", "source_close"},
		},
		{
			name: "hosts preparation failure shuts down health then closes source",
			configure: func(_ *orderedSource, store *orderedStore, _ *orderedHealthServer) error {
				store.prepareErr = errors.New("prepare sentinel")
				return store.prepareErr
			},
			wantOps:    []string{"ping", "health_start", "hosts_prepare", "health_shutdown", "source_close"},
			wantActive: []health.Concern{health.ConcernRecovery},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &operationLog{}
			source := &orderedSource{log: log}
			store := &orderedStore{log: log}
			server := newOrderedHealthServer(log)
			wantErr := tt.configure(source, store, server)
			tracker := health.NewTracker()
			daemon := mustNewDaemon(t, validConfig(), source, store, tracker, server)

			err := daemon.Run(t.Context())
			if !errors.Is(err, wantErr) {
				t.Fatalf("Run() error = %v, want wrapped %v", err, wantErr)
			}
			assertOperations(t, log, tt.wantOps)
			assertActiveConcerns(t, tracker, tt.wantActive...)
		})
	}
}

func TestRunSuccessfulPreparationClearsRecoveryConcern(t *testing.T) {
	log := &operationLog{}
	source := &orderedSource{log: log}
	store := &orderedStore{log: log}
	server := newOrderedHealthServer(log)
	tracker := health.NewTracker()
	tracker.Fail(health.ConcernRecovery, "old preparation failure")
	ctx, cancel := context.WithCancel(t.Context())
	store.onPrepare = func(context.Context) { cancel() }
	daemon := mustNewDaemon(t, validConfig(), source, store, tracker, server)

	if err := daemon.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOperations(t, log, []string{"ping", "health_start", "hosts_prepare", "health_shutdown", "source_close"})
	assertActiveConcerns(t, tracker)
}

func TestRunCallerCancellationBeforeAndDuringStartupIsClean(t *testing.T) {
	t.Run("before startup", func(t *testing.T) {
		log := &operationLog{}
		source := &orderedSource{log: log}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		daemon := mustNewDaemon(t, validConfig(), source, &orderedStore{log: log}, health.NewTracker(), newOrderedHealthServer(log))

		if err := daemon.Run(ctx); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		assertOperations(t, log, []string{"source_close"})
	})

	t.Run("during ping", func(t *testing.T) {
		log := &operationLog{}
		ctx, cancel := context.WithCancel(t.Context())
		source := &orderedSource{log: log}
		source.onPing = func(context.Context) {
			cancel()
			source.pingErr = context.Canceled
		}
		daemon := mustNewDaemon(t, validConfig(), source, &orderedStore{log: log}, health.NewTracker(), newOrderedHealthServer(log))

		if err := daemon.Run(ctx); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		assertOperations(t, log, []string{"ping", "source_close"})
	})

	t.Run("during hosts apply", func(t *testing.T) {
		log := &operationLog{}
		ctx, cancel := context.WithCancel(t.Context())
		applyStarted := make(chan struct{})
		source := &orderedSource{log: log}
		store := &orderedStore{log: log}
		store.onApply = func(ctx context.Context) {
			close(applyStarted)
			<-ctx.Done()
			store.applyErr = ctx.Err()
		}
		server := newOrderedHealthServer(log)
		daemon := mustNewDaemon(t, validConfig(), source, store, health.NewTracker(), server)
		result := make(chan error, 1)
		go func() { result <- daemon.Run(ctx) }()

		<-applyStarted
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		assertOperations(t, log, []string{
			"ping", "health_start", "hosts_prepare", "snapshot", "hosts_apply",
			"health_shutdown", "source_close",
		})
	})
}

func TestRunCallerCancellationInLoopReturnsNilAfterCleanup(t *testing.T) {
	log := &operationLog{}
	ctx, cancel := context.WithCancel(t.Context())
	startupComplete := make(chan struct{})
	source := &orderedSource{log: log}
	store := &orderedStore{log: log, onApply: func(context.Context) { close(startupComplete) }}
	server := newOrderedHealthServer(log)
	daemon := mustNewDaemon(t, validConfig(), source, store, health.NewTracker(), server)
	result := make(chan error, 1)
	go func() { result <- daemon.Run(ctx) }()

	<-startupComplete
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	assertOperations(t, log, []string{
		"ping", "health_start", "hosts_prepare", "snapshot", "hosts_apply",
		"health_shutdown", "source_close",
	})
}

func TestRunUnexpectedHealthCompletionReturnsError(t *testing.T) {
	tests := []struct {
		name     string
		serveErr error
	}{
		{name: "terminal serve error", serveErr: errors.New("serve sentinel")},
		{name: "nil terminal error is still unexpected"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &operationLog{}
			source := &orderedSource{log: log}
			server := newOrderedHealthServer(log)
			store := &orderedStore{log: log, onApply: func(context.Context) { server.finish(tt.serveErr) }}
			daemon := mustNewDaemon(t, validConfig(), source, store, health.NewTracker(), server)

			err := daemon.Run(t.Context())
			if err == nil {
				t.Fatal("Run() error = nil, want unexpected health completion error")
			}
			if tt.serveErr != nil && !errors.Is(err, tt.serveErr) {
				t.Fatalf("Run() error = %v, want wrapped %v", err, tt.serveErr)
			}
			assertOperations(t, log, []string{
				"ping", "health_start", "hosts_prepare", "snapshot", "hosts_apply",
				"health_shutdown", "source_close",
			})
		})
	}
}

func TestRunCallerCancellationDoesNotHideConcurrentTerminalServeError(t *testing.T) {
	log := &operationLog{}
	serveErr := errors.New("serve sentinel")
	ctx, cancel := context.WithCancel(t.Context())
	source := &orderedSource{log: log}
	server := newOrderedHealthServer(log)
	server.onErr = cancel
	store := &orderedStore{log: log, onApply: func(context.Context) { server.finish(serveErr) }}
	daemon := mustNewDaemon(t, validConfig(), source, store, health.NewTracker(), server)

	err := daemon.Run(ctx)
	if !errors.Is(err, serveErr) {
		t.Fatalf("Run() error = %v, want terminal serve error despite concurrent caller cancellation", err)
	}
	if count := strings.Count(err.Error(), serveErr.Error()); count != 1 {
		t.Fatalf("Run() error contains serve error %d times, want once: %v", count, err)
	}
}

func TestRunJoinsIndependentStartupAndCleanupErrors(t *testing.T) {
	log := &operationLog{}
	prepareErr := errors.New("prepare sentinel")
	serveErr := errors.New("serve sentinel")
	shutdownErr := errors.New("shutdown sentinel")
	closeErr := errors.New("close sentinel")
	source := &orderedSource{log: log, closeErr: closeErr}
	store := &orderedStore{log: log, prepareErr: prepareErr}
	server := newOrderedHealthServer(log)
	server.setTerminalError(serveErr)
	server.shutdownErr = shutdownErr
	daemon := mustNewDaemon(t, validConfig(), source, store, health.NewTracker(), server)

	err := daemon.Run(t.Context())
	for _, wantErr := range []error{prepareErr, serveErr, shutdownErr, closeErr} {
		if !errors.Is(err, wantErr) {
			t.Errorf("Run() error = %v, want joined %v", err, wantErr)
		}
	}
	if count := strings.Count(err.Error(), serveErr.Error()); count != 1 {
		t.Fatalf("Run() error contains serve error %d times, want once: %v", count, err)
	}
	assertOperations(t, log, []string{
		"ping", "health_start", "hosts_prepare", "health_shutdown", "source_close",
	})
}

func TestRunJoinsUnexpectedServeAndCleanupErrorsWithoutDuplicatingServeError(t *testing.T) {
	log := &operationLog{}
	serveErr := errors.New("serve sentinel")
	shutdownErr := errors.New("shutdown sentinel")
	closeErr := errors.New("close sentinel")
	source := &orderedSource{log: log, closeErr: closeErr}
	server := newOrderedHealthServer(log)
	server.shutdownErr = shutdownErr
	store := &orderedStore{log: log, onApply: func(context.Context) { server.finish(serveErr) }}
	daemon := mustNewDaemon(t, validConfig(), source, store, health.NewTracker(), server)

	err := daemon.Run(t.Context())
	for _, wantErr := range []error{serveErr, shutdownErr, closeErr} {
		if !errors.Is(err, wantErr) {
			t.Errorf("Run() error = %v, want joined %v", err, wantErr)
		}
	}
	if count := strings.Count(err.Error(), serveErr.Error()); count != 1 {
		t.Fatalf("Run() error contains serve error %d times, want once: %v", count, err)
	}
	_, _, errCalls := server.shutdownCalls()
	if errCalls != 1 {
		t.Fatalf("HealthServer.Err() calls = %d, want one terminal read", errCalls)
	}
}

func validConfig() Config {
	return Config{
		Networks:         []string{"saltbox", "backend"},
		DefaultNetwork:   "saltbox",
		PeriodicInterval: 5 * time.Minute,
		DebounceDelay:    time.Second,
		MaxDebounceDelay: 5 * time.Second,
	}
}

func configWith(base Config, change func(*Config)) Config {
	base.Networks = slices.Clone(base.Networks)
	change(&base)
	return base
}

func mustNewDaemon(
	t *testing.T,
	cfg Config,
	source NetworkSource,
	store HostStore,
	tracker *health.Tracker,
	server HealthServer,
) *Daemon {
	t.Helper()
	daemon, err := New(cfg, source, store, tracker, server, testLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return daemon
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func assertOperations(t *testing.T, log *operationLog, want []string) {
	t.Helper()
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
}

func assertActiveConcerns(t *testing.T, tracker *health.Tracker, want ...health.Concern) {
	t.Helper()
	snapshot := tracker.Snapshot()
	got := make([]health.Concern, 0, len(snapshot.Active))
	for concern := range snapshot.Active {
		got = append(got, concern)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("active health concerns = %v, want %v", got, want)
	}
}

func assertNewestHealthMessage(t *testing.T, tracker *health.Tracker, want string) {
	t.Helper()
	history := tracker.Snapshot().History
	if len(history) == 0 || history[len(history)-1].Message != want {
		t.Fatalf("health history = %+v, want newest message %q", history, want)
	}
}

func assertDeadlineBetween(t *testing.T, ctx context.Context, earliest, latest time.Time) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context has no deadline")
	}
	if deadline.Before(earliest) || deadline.After(latest) {
		t.Fatalf("context deadline = %v, want between %v and %v", deadline, earliest, latest)
	}
}

func assertEndpointSlices(t *testing.T, got, want []Endpoint) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("endpoints = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Network != want[i].Network || got[i].IP != want[i].IP || !slices.Equal(got[i].Aliases, want[i].Aliases) {
			t.Fatalf("endpoints = %+v, want %+v", got, want)
		}
	}
}

func cloneEndpoints(endpoints []Endpoint) []Endpoint {
	cloned := make([]Endpoint, len(endpoints))
	for i, endpoint := range endpoints {
		cloned[i] = endpoint
		cloned[i].Aliases = slices.Clone(endpoint.Aliases)
	}
	return cloned
}

func cloneEndpoints2D(endpoints [][]Endpoint) [][]Endpoint {
	cloned := make([][]Endpoint, len(endpoints))
	for i := range endpoints {
		cloned[i] = cloneEndpoints(endpoints[i])
	}
	return cloned
}

func cloneStrings2D(values [][]string) [][]string {
	cloned := make([][]string, len(values))
	for i := range values {
		cloned[i] = slices.Clone(values[i])
	}
	return cloned
}
