package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/saltyorg/sdhm/health"
)

type loopTestSnapshot struct {
	at       time.Time
	networks []string
}

type loopTestStreamCall struct {
	at       time.Time
	ctx      context.Context
	networks []string
}

type loopTestStream struct {
	events chan Event
	errors chan error
}

type loopTestResult struct {
	err              error
	serveErrCaptured bool
}

type loopTestSource struct {
	streams         chan loopTestStream
	streamCalls     chan loopTestStreamCall
	snapshots       chan loopTestSnapshot
	snapshotResults chan error
	closed          chan struct{}
	allCanceled     chan bool
	mutateNetworks  func([]string)
	onSnapshot      func(context.Context, int)
	onEvents        func(context.Context, int)
	closeOnce       sync.Once
	streamMu        sync.Mutex
	streamContexts  []context.Context
	snapshotCount   atomic.Int64
	eventCallCount  atomic.Int64
}

type loopTestStore struct {
	applyCalls chan int
	onApply    func(context.Context, int) error
	count      atomic.Int64
}

func newLoopTestStore() *loopTestStore {
	return &loopTestStore{applyCalls: make(chan int, 64)}
}

func (*loopTestStore) Prepare(context.Context) (PrepareResult, error) {
	return PrepareResult{}, nil
}

func (s *loopTestStore) Apply(ctx context.Context, _ []Endpoint) error {
	call := int(s.count.Add(1))
	s.applyCalls <- call
	if s.onApply != nil {
		return s.onApply(ctx, call)
	}
	return nil
}

func newLoopTestSource() *loopTestSource {
	return &loopTestSource{
		streams:         make(chan loopTestStream, 16),
		streamCalls:     make(chan loopTestStreamCall, 16),
		snapshots:       make(chan loopTestSnapshot, 64),
		snapshotResults: make(chan error, 16),
		closed:          make(chan struct{}),
		allCanceled:     make(chan bool, 1),
	}
}

func (*loopTestSource) Ping(context.Context) error {
	return nil
}

func (s *loopTestSource) Snapshot(ctx context.Context, networks []string) ([]Endpoint, error) {
	callNumber := int(s.snapshotCount.Add(1))
	call := loopTestSnapshot{at: time.Now(), networks: slices.Clone(networks)}
	select {
	case s.snapshots <- call:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.onSnapshot != nil {
		s.onSnapshot(ctx, callNumber)
	}

	select {
	case err := <-s.snapshotResults:
		return nil, err
	default:
		return nil, nil
	}
}

func (s *loopTestSource) Events(ctx context.Context, networks []string) (<-chan Event, <-chan error) {
	callNumber := int(s.eventCallCount.Add(1))
	s.streamMu.Lock()
	s.streamContexts = append(s.streamContexts, ctx)
	s.streamMu.Unlock()
	call := loopTestStreamCall{at: time.Now(), ctx: ctx, networks: slices.Clone(networks)}
	if s.mutateNetworks != nil {
		s.mutateNetworks(networks)
	}
	select {
	case s.streamCalls <- call:
	case <-ctx.Done():
		return nil, nil
	}
	if s.onEvents != nil {
		s.onEvents(ctx, callNumber)
	}

	select {
	case stream := <-s.streams:
		return stream.events, stream.errors
	case <-ctx.Done():
		return nil, nil
	}
}

func (s *loopTestSource) Close() error {
	s.closeOnce.Do(func() {
		s.streamMu.Lock()
		allCanceled := true
		for _, ctx := range s.streamContexts {
			if ctx.Err() == nil {
				allCanceled = false
				break
			}
		}
		s.streamMu.Unlock()
		s.allCanceled <- allCanceled
		close(s.closed)
	})
	return nil
}

type loopTestHarness struct {
	source   *loopTestSource
	stream   loopTestStream
	server   *orderedHealthServer
	daemon   *Daemon
	recorder *logRecorder
	cancel   context.CancelFunc
	result   chan error
	stopOnce sync.Once
	stopErr  error
}

func newLoopTestHarness(t *testing.T, cfg Config) *loopTestHarness {
	t.Helper()
	return newLoopTestHarnessWith(t, cfg, newLoopTestSource(), newLoopTestStore(), health.NewTracker(), nil)
}

func newLoopTestHarnessWith(
	t *testing.T,
	cfg Config,
	source *loopTestSource,
	store HostStore,
	tracker *health.Tracker,
	configure func(*Daemon),
) *loopTestHarness {
	t.Helper()

	stream := newLoopTestStream()
	source.streams <- stream
	log := &operationLog{}
	server := newOrderedHealthServer(log)
	recorder := newLogRecorder()
	daemon := mustNewDaemonWithLogger(
		t,
		cfg,
		source,
		store,
		tracker,
		server,
		slog.New(recorder),
	)
	if configure != nil {
		configure(daemon)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- daemon.Run(ctx) }()

	return &loopTestHarness{
		source:   source,
		stream:   stream,
		server:   server,
		daemon:   daemon,
		recorder: recorder,
		cancel:   cancel,
		result:   result,
	}
}

func (h *loopTestHarness) start(t *testing.T) (loopTestSnapshot, loopTestStreamCall) {
	t.Helper()
	initial := receiveLoopValue(t, h.source.snapshots, "initial reconciliation")
	stream := receiveLoopValue(t, h.source.streamCalls, "initial event stream")
	return initial, stream
}

func (h *loopTestHarness) stop(t *testing.T) {
	t.Helper()
	if err := h.shutdown(t); err != nil {
		t.Errorf("Run() error = %v, want nil", err)
	}
}

func (h *loopTestHarness) shutdown(t *testing.T) error {
	t.Helper()
	h.cancel()
	return h.wait(t)
}

func (h *loopTestHarness) wait(t *testing.T) error {
	t.Helper()
	h.stopOnce.Do(func() {
		h.stopErr = receiveLoopValue(t, h.result, "daemon shutdown")
	})
	return h.stopErr
}

func newLoopTestStream() loopTestStream {
	return loopTestStream{events: make(chan Event, 16), errors: make(chan error, 16)}
}

func failLoopTestStream(stream loopTestStream, err error) {
	stream.errors <- err
	close(stream.errors)
	close(stream.events)
}

func TestLoopPeriodicIntervalReconcilesImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		harness := newLoopTestHarness(t, cfg)
		defer harness.stop(t)
		_, stream := harness.start(t)

		got := receiveLoopValue(t, harness.source.snapshots, "periodic reconciliation")
		if elapsed := got.at.Sub(stream.at); elapsed != cfg.PeriodicInterval {
			t.Fatalf("periodic reconciliation delay = %v, want %v", elapsed, cfg.PeriodicInterval)
		}
	})
}

func TestLoopDebounceResetsForRapidEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		harness := newLoopTestHarness(t, cfg)
		defer harness.stop(t)
		harness.start(t)

		harness.stream.events <- Event{Action: "connect", Network: "saltbox"}
		firstAt := time.Now()
		advanceLoopTime(t, cfg.DebounceDelay/2)
		harness.stream.events <- Event{Action: "disconnect", Network: "backend"}
		lastAt := time.Now()

		got := receiveLoopValue(t, harness.source.snapshots, "debounced reconciliation")
		if elapsed := got.at.Sub(lastAt); elapsed != cfg.DebounceDelay {
			t.Fatalf("reconciliation delay after last event = %v, want %v", elapsed, cfg.DebounceDelay)
		}
		if elapsed := got.at.Sub(firstAt); elapsed <= cfg.DebounceDelay {
			t.Fatalf("reconciliation delay after first event = %v, want reset beyond %v", elapsed, cfg.DebounceDelay)
		}
		assertNoLoopSnapshot(t, harness.source.snapshots)
	})
}

func TestLoopMaximumDelayForcesReconciliationDuringContinuousEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		harness := newLoopTestHarness(t, cfg)
		defer harness.stop(t)
		harness.start(t)

		harness.stream.events <- Event{Action: "connect", Network: "saltbox"}
		firstAt := time.Now()
		for range 4 {
			advanceLoopTime(t, cfg.DebounceDelay/2)
			harness.stream.events <- Event{Action: "connect", Network: "backend"}
		}

		got := receiveLoopValue(t, harness.source.snapshots, "maximum-delay reconciliation")
		if elapsed := got.at.Sub(firstAt); elapsed != cfg.MaxDebounceDelay {
			t.Fatalf("continuous-event reconciliation delay = %v, want maximum %v", elapsed, cfg.MaxDebounceDelay)
		}
		assertNoLoopSnapshot(t, harness.source.snapshots)
	})
}

func TestLoopPeriodicTriggerClearsPendingDebounce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		cfg.PeriodicInterval = 3 * time.Second
		cfg.DebounceDelay = 5 * time.Second
		cfg.MaxDebounceDelay = 10 * time.Second
		harness := newLoopTestHarness(t, cfg)
		defer harness.stop(t)
		_, stream := harness.start(t)

		harness.stream.events <- Event{Action: "connect", Network: "saltbox"}
		got := receiveLoopValue(t, harness.source.snapshots, "periodic preemption reconciliation")
		if elapsed := got.at.Sub(stream.at); elapsed != cfg.PeriodicInterval {
			t.Fatalf("preempting reconciliation delay = %v, want periodic %v", elapsed, cfg.PeriodicInterval)
		}

		advanceLoopTime(t, cfg.DebounceDelay-cfg.PeriodicInterval+time.Second/2)
		assertNoLoopSnapshot(t, harness.source.snapshots)
	})
}

func TestLoopEventOnEveryNetworkDebouncesCompleteSnapshot(t *testing.T) {
	for _, network := range []string{"saltbox", "backend"} {
		t.Run(network, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := loopTestConfig()
				harness := newLoopTestHarness(t, cfg)
				defer harness.stop(t)
				harness.start(t)

				harness.stream.events <- Event{Action: "connect", Network: network}
				got := receiveLoopValue(t, harness.source.snapshots, "event reconciliation")
				if !slices.Equal(got.networks, []string{"saltbox", "backend"}) {
					t.Fatalf("Snapshot() networks = %v, want complete [saltbox backend]", got.networks)
				}
			})
		})
	}
}

func TestLoopSerializesReconciliationAndCoalescesEventsDuringApply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		store := newLoopTestStore()
		applyRelease := make(chan struct{})
		store.onApply = func(ctx context.Context, call int) error {
			if call != 2 {
				return nil
			}
			select {
			case <-applyRelease:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		harness := newLoopTestHarnessWith(t, cfg, source, store, health.NewTracker(), nil)
		defer harness.stop(t)
		harness.start(t)
		if call := receiveLoopValue(t, store.applyCalls, "initial apply"); call != 1 {
			t.Fatalf("initial Apply() call = %d, want 1", call)
		}

		harness.stream.events <- Event{Action: "connect", Network: "saltbox"}
		receiveLoopValue(t, source.snapshots, "blocked reconciliation")
		if call := receiveLoopValue(t, store.applyCalls, "blocked apply"); call != 2 {
			t.Fatalf("blocked Apply() call = %d, want 2", call)
		}

		for range 6 {
			harness.stream.events <- Event{Action: "disconnect", Network: "backend"}
		}
		close(applyRelease)

		receiveLoopValue(t, source.snapshots, "coalesced following reconciliation")
		if call := receiveLoopValue(t, store.applyCalls, "coalesced following apply"); call != 3 {
			t.Fatalf("following Apply() call = %d, want 3", call)
		}
		advanceLoopTime(t, cfg.MaxDebounceDelay+cfg.DebounceDelay)
		assertNoLoopSnapshot(t, source.snapshots)
	})
}

func TestLoopRetryBackoffDoublesAndResetsAfterSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		tracker := health.NewTracker()
		firstFailure := errors.New("first snapshot failure")
		secondFailure := errors.New("second snapshot failure")
		source.snapshotResults <- firstFailure
		source.snapshotResults <- secondFailure
		harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), tracker, nil)
		defer harness.stop(t)
		initial, _ := harness.start(t)

		firstRetry := receiveLoopValue(t, source.snapshots, "first reconciliation retry")
		if delay := firstRetry.at.Sub(initial.at); delay != retryInitialDelay {
			t.Fatalf("first retry delay = %v, want %v", delay, retryInitialDelay)
		}
		secondRetry := receiveLoopValue(t, source.snapshots, "second reconciliation retry")
		if delay := secondRetry.at.Sub(firstRetry.at); delay != 2*retryInitialDelay {
			t.Fatalf("second retry delay = %v, want %v", delay, 2*retryInitialDelay)
		}
		synctest.Wait()
		assertActiveConcerns(t, tracker)

		afterSuccessFailure := errors.New("post-success snapshot failure")
		source.snapshotResults <- afterSuccessFailure
		harness.stream.events <- Event{Action: "connect", Network: "saltbox"}
		failedAttempt := receiveLoopValue(t, source.snapshots, "post-success failed reconciliation")
		synctest.Wait()
		assertActiveConcerns(t, tracker, health.ConcernDockerSnapshot)

		resetRetry := receiveLoopValue(t, source.snapshots, "reset reconciliation retry")
		if delay := resetRetry.at.Sub(failedAttempt.at); delay != retryInitialDelay {
			t.Fatalf("retry delay after success = %v, want reset %v", delay, retryInitialDelay)
		}
		synctest.Wait()
		assertActiveConcerns(t, tracker)
	})
}

func TestLoopLogsReconciliationTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		tracker := health.NewTracker()
		firstFailure := errors.New("snapshot unavailable")
		sameFailure := errors.New("snapshot unavailable")
		changedFailure := errors.New("snapshot denied")
		source.snapshotResults <- firstFailure
		source.snapshotResults <- sameFailure
		source.snapshotResults <- changedFailure
		harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), tracker, nil)
		defer harness.stop(t)
		harness.start(t)

		initialFailures := logRecords(harness.recorder.Records(), slog.LevelWarn, "reconciliation failed")
		if len(initialFailures) != 1 {
			t.Fatalf("initial reconciliation failures = %d, want 1", len(initialFailures))
		}
		if got := logAttr(t, initialFailures[0], "phase"); got.Kind() != slog.KindString || got.String() != "initial" {
			t.Errorf("initial phase = %v, want initial", got)
		}
		if got := logAttr(t, initialFailures[0], "retry_in"); got.Kind() != slog.KindDuration || got.Duration() != retryInitialDelay {
			t.Errorf("initial retry_in = %v, want %v", got, retryInitialDelay)
		}

		receiveLoopValue(t, source.snapshots, "identical reconciliation retry")
		synctest.Wait()
		if history := tracker.Snapshot().History; len(history) != 2 {
			t.Fatalf("health history after identical retry = %d, want 2", len(history))
		}
		if failures := logRecords(harness.recorder.Records(), slog.LevelWarn, "reconciliation failed"); len(failures) != 1 {
			t.Fatalf("reconciliation failures after identical retry = %d, want 1", len(failures))
		}

		receiveLoopValue(t, source.snapshots, "changed reconciliation retry")
		synctest.Wait()
		failures := logRecords(harness.recorder.Records(), slog.LevelWarn, "reconciliation failed")
		if len(failures) != 2 {
			t.Fatalf("reconciliation failures after changed retry = %d, want 2", len(failures))
		}
		if got := logAttr(t, failures[1], "err"); got.Kind() != slog.KindAny || !errors.Is(got.Any().(error), changedFailure) {
			t.Errorf("changed retry err = %v, want %v", got, changedFailure)
		}
		if got := logAttr(t, failures[1], "retry_in"); got.Kind() != slog.KindDuration || got.Duration() != 4*time.Second {
			t.Errorf("changed retry_in = %v, want %v", got, 4*time.Second)
		}

		receiveLoopValue(t, source.snapshots, "successful reconciliation retry")
		synctest.Wait()
		if recovered := logRecords(harness.recorder.Records(), slog.LevelInfo, "reconciliation recovered"); len(recovered) != 1 {
			t.Fatalf("reconciliation recovered records = %d, want 1", len(recovered))
		}

		harness.stream.events <- Event{Action: "connect", Network: "saltbox"}
		receiveLoopValue(t, source.snapshots, "later healthy reconciliation")
		synctest.Wait()
		if recovered := logRecords(harness.recorder.Records(), slog.LevelInfo, "reconciliation recovered"); len(recovered) != 1 {
			t.Fatalf("reconciliation recovered records after healthy reconciliation = %d, want 1", len(recovered))
		}
	})
}

func TestLoopLogsFirstRuntimeReconciliationFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := newLoopTestSource()
		failure := errors.New("snapshot unavailable")
		harness := newLoopTestHarnessWith(t, loopTestConfig(), source, newLoopTestStore(), health.NewTracker(), nil)
		defer harness.stop(t)
		harness.start(t)

		source.snapshotResults <- failure
		harness.stream.events <- Event{Action: "connect", Network: "saltbox"}
		receiveLoopValue(t, source.snapshots, "first failed runtime reconciliation")
		synctest.Wait()

		failures := logRecords(harness.recorder.Records(), slog.LevelWarn, "reconciliation failed")
		if len(failures) != 1 {
			t.Fatalf("runtime reconciliation failures = %d, want 1", len(failures))
		}
		if got := logAttr(t, failures[0], "err"); got.Kind() != slog.KindAny || !errors.Is(got.Any().(error), failure) {
			t.Errorf("runtime err = %v, want %v", got, failure)
		}
		if got := logAttr(t, failures[0], "retry_in"); got.Kind() != slog.KindDuration || got.Duration() != retryInitialDelay {
			t.Errorf("runtime retry_in = %v, want %v", got, retryInitialDelay)
		}
		if _, present := logAttrIfPresent(failures[0], "phase"); present {
			t.Fatal("runtime reconciliation failure has initial-only phase")
		}
	})
}

func TestLoopLogsCancellationDuringReconciliation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := newLoopTestSource()
		initialFailure := errors.New("initial snapshot unavailable")
		source.snapshotResults <- initialFailure
		retryStarted := make(chan struct{}, 1)
		source.onSnapshot = func(ctx context.Context, call int) {
			if call != 2 {
				return
			}
			retryStarted <- struct{}{}
			<-ctx.Done()
		}
		harness := newLoopTestHarnessWith(t, loopTestConfig(), source, newLoopTestStore(), health.NewTracker(), nil)
		harness.start(t)

		receiveLoopValue(t, retryStarted, "blocked reconciliation retry")
		harness.cancel()
		if err := harness.wait(t); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}

		if failures := logRecords(harness.recorder.Records(), slog.LevelWarn, "reconciliation failed"); len(failures) != 1 {
			t.Fatalf("reconciliation failures after cancellation = %d, want only initial failure", len(failures))
		}
		if recovered := logRecords(harness.recorder.Records(), slog.LevelInfo, "reconciliation recovered"); len(recovered) != 0 {
			t.Fatalf("reconciliation recoveries after cancellation = %d, want 0", len(recovered))
		}
	})
}

func TestLoopHealthServerStopDuringFailedRuntimeReconciliationSuppressesTransition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		store := newLoopTestStore()
		tracker := health.NewTracker()
		stream := newLoopTestStream()
		source.streams <- stream
		server := newOrderedHealthServer(&operationLog{})
		recorder := newLogRecorder()
		failure := errors.New("runtime snapshot failure")
		serveErr := errors.New("serve failure during runtime snapshot")
		source.onSnapshot = func(_ context.Context, call int) {
			if call == 1 {
				server.finish(serveErr)
			}
		}
		source.snapshotResults <- failure
		daemon := mustNewDaemonWithLogger(t, cfg, source, store, tracker, server, slog.New(recorder))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan loopTestResult, 1)
		go func() {
			err, captured := daemon.loop(ctx, nil)
			result <- loopTestResult{err: err, serveErrCaptured: captured}
		}()
		receiveLoopValue(t, source.streamCalls, "initial event stream")

		stream.events <- Event{Action: "connect", Network: "saltbox"}
		receiveLoopValue(t, source.snapshots, "failed reconciliation that stops the health server")

		got := receiveLoopValue(t, result, "loop termination")
		if !got.serveErrCaptured || !errors.Is(got.err, serveErr) || got.err.Error() != "health server stopped: "+serveErr.Error() {
			t.Fatalf("loop result = (%v, captured %t), want health server classification wrapping %v", got.err, got.serveErrCaptured, serveErr)
		}
		if failures := logRecords(recorder.Records(), slog.LevelWarn, "reconciliation failed"); len(failures) != 0 {
			t.Fatalf("post-stop reconciliation failure records = %d, want 0", len(failures))
		}
		if recoveries := logRecords(recorder.Records(), slog.LevelInfo, "reconciliation recovered"); len(recoveries) != 0 {
			t.Fatalf("post-stop reconciliation recovery records = %d, want 0", len(recoveries))
		}

		snapshot := tracker.Snapshot()
		assertActiveConcerns(t, tracker, health.ConcernDockerSnapshot)
		if !snapshot.Ready {
			t.Fatal("health readiness = false, want completed initialization preserved")
		}
		if len(snapshot.History) != 1 || snapshot.History[0].Message != failure.Error() {
			t.Fatalf("health history = %+v, want preserved runtime failure %q", snapshot.History, failure)
		}

		advanceLoopTime(t, cfg.PeriodicInterval+retryInitialDelay)
		if calls := source.snapshotCount.Load(); calls != 1 {
			t.Fatalf("Snapshot() calls = %d, want only the stopped runtime attempt", calls)
		}
		if calls := store.count.Load(); calls != 0 {
			t.Fatalf("Apply() calls = %d, want none after failed snapshot", calls)
		}
		if calls := source.eventCallCount.Load(); calls != 1 {
			t.Fatalf("Events() calls = %d, want no post-stop reconnect", calls)
		}
		_, _, errCalls := server.shutdownCalls()
		if errCalls != 1 {
			t.Fatalf("HealthServer.Err() calls = %d, want one terminal classification read", errCalls)
		}
	})
}

func TestLoopHealthServerStopDuringRecoveredRuntimeReconciliationSuppressesTransition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		store := newLoopTestStore()
		tracker := health.NewTracker()
		stream := newLoopTestStream()
		source.streams <- stream
		server := newOrderedHealthServer(&operationLog{})
		recorder := newLogRecorder()
		initialFailure := errors.New("initial snapshot failure")
		initialReconcileErr := fmt.Errorf("snapshot Docker networks: %w", initialFailure)
		serveErr := errors.New("serve failure during runtime recovery")
		tracker.Fail(health.ConcernDockerSnapshot, initialFailure.Error())
		store.onApply = func(_ context.Context, call int) error {
			if call == 1 {
				server.finish(serveErr)
			}
			return nil
		}
		daemon := mustNewDaemonWithLogger(t, cfg, source, store, tracker, server, slog.New(recorder))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan loopTestResult, 1)
		go func() {
			err, captured := daemon.loop(ctx, initialReconcileErr)
			result <- loopTestResult{err: err, serveErrCaptured: captured}
		}()
		receiveLoopValue(t, source.streamCalls, "initial event stream")
		receiveLoopValue(t, source.snapshots, "successful reconciliation that stops the health server")

		got := receiveLoopValue(t, result, "loop termination")
		if !got.serveErrCaptured || !errors.Is(got.err, serveErr) || got.err.Error() != "health server stopped: "+serveErr.Error() {
			t.Fatalf("loop result = (%v, captured %t), want health server classification wrapping %v", got.err, got.serveErrCaptured, serveErr)
		}
		if failures := logRecords(recorder.Records(), slog.LevelWarn, "reconciliation failed"); len(failures) != 0 {
			t.Fatalf("post-stop reconciliation failure records = %d, want 0", len(failures))
		}
		if recoveries := logRecords(recorder.Records(), slog.LevelInfo, "reconciliation recovered"); len(recoveries) != 0 {
			t.Fatalf("post-stop reconciliation recovery records = %d, want 0", len(recoveries))
		}

		snapshot := tracker.Snapshot()
		assertActiveConcerns(t, tracker)
		if !snapshot.Ready {
			t.Fatal("health readiness = false, want completed initialization preserved")
		}
		if len(snapshot.History) != 1 || snapshot.History[0].Message != initialFailure.Error() {
			t.Fatalf("health history = %+v, want retained initial failure %q", snapshot.History, initialFailure)
		}

		advanceLoopTime(t, cfg.PeriodicInterval+retryInitialDelay)
		if calls := source.snapshotCount.Load(); calls != 1 {
			t.Fatalf("Snapshot() calls = %d, want only the stopped recovery attempt", calls)
		}
		if calls := store.count.Load(); calls != 1 {
			t.Fatalf("Apply() calls = %d, want only the completed recovery apply", calls)
		}
		if calls := source.eventCallCount.Load(); calls != 1 {
			t.Fatalf("Events() calls = %d, want no post-stop reconnect", calls)
		}
		_, _, errCalls := server.shutdownCalls()
		if errCalls != 1 {
			t.Fatalf("HealthServer.Err() calls = %d, want one terminal classification read", errCalls)
		}
	})
}

func TestLoopLogsEventStreamFailureTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := newLoopTestSource()
		firstFailure := errors.New("event stream unavailable")
		sameFailure := errors.New("event stream unavailable")
		changedFailure := errors.New("event stream denied")
		harness := newLoopTestHarnessWith(t, loopTestConfig(), source, newLoopTestStore(), health.NewTracker(), nil)
		defer harness.stop(t)
		harness.start(t)

		if recovered := logRecords(harness.recorder.Records(), slog.LevelInfo, "Docker event stream recovered"); len(recovered) != 0 {
			t.Fatalf("event stream recoveries before a failure = %d, want 0", len(recovered))
		}

		secondStream := newLoopTestStream()
		source.streams <- secondStream
		failLoopTestStream(harness.stream, firstFailure)
		receiveLoopValue(t, source.streamCalls, "first reconnected event stream")
		synctest.Wait()

		failures := logRecords(harness.recorder.Records(), slog.LevelWarn, "Docker event stream unavailable")
		if len(failures) != 1 {
			t.Fatalf("event stream failures = %d, want 1", len(failures))
		}
		if got := logAttr(t, failures[0], "err"); got.Kind() != slog.KindString || got.String() != firstFailure.Error() {
			t.Errorf("first stream err = %v, want %q", got, firstFailure)
		}
		if got := logAttr(t, failures[0], "retry_in"); got.Kind() != slog.KindDuration || got.Duration() != retryInitialDelay {
			t.Errorf("first stream retry_in = %v, want %v", got, retryInitialDelay)
		}

		thirdStream := newLoopTestStream()
		source.streams <- thirdStream
		failLoopTestStream(secondStream, sameFailure)
		receiveLoopValue(t, source.streamCalls, "second reconnected event stream")
		synctest.Wait()
		if failures := logRecords(harness.recorder.Records(), slog.LevelWarn, "Docker event stream unavailable"); len(failures) != 1 {
			t.Fatalf("event stream failures after identical reconnect failure = %d, want 1", len(failures))
		}

		fourthStream := newLoopTestStream()
		source.streams <- fourthStream
		failLoopTestStream(thirdStream, changedFailure)
		receiveLoopValue(t, source.streamCalls, "third reconnected event stream")
		synctest.Wait()

		failures = logRecords(harness.recorder.Records(), slog.LevelWarn, "Docker event stream unavailable")
		if len(failures) != 2 {
			t.Fatalf("event stream failures after changed reconnect failure = %d, want 2", len(failures))
		}
		if got := logAttr(t, failures[1], "err"); got.Kind() != slog.KindString || got.String() != changedFailure.Error() {
			t.Errorf("changed stream err = %v, want %q", got, changedFailure)
		}
		if got := logAttr(t, failures[1], "retry_in"); got.Kind() != slog.KindDuration || got.Duration() != 4*time.Second {
			t.Errorf("changed stream retry_in = %v, want %v", got, 4*time.Second)
		}
	})
}

func TestLoopLogsEventStreamRecoveryFromEventOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := newLoopTestSource()
		harness := newLoopTestHarnessWith(t, loopTestConfig(), source, newLoopTestStore(), health.NewTracker(), nil)
		defer harness.stop(t)
		harness.start(t)

		secondStream := newLoopTestStream()
		source.streams <- secondStream
		failLoopTestStream(harness.stream, errors.New("event stream unavailable"))
		receiveLoopValue(t, source.streamCalls, "reconnected event stream")

		secondStream.events <- Event{Action: "connect", Network: "saltbox"}
		synctest.Wait()
		recovered := logRecords(harness.recorder.Records(), slog.LevelInfo, "Docker event stream recovered")
		if len(recovered) != 1 {
			t.Fatalf("event stream recoveries after an event = %d, want 1", len(recovered))
		}
		if got := logAttr(t, recovered[0], "evidence"); got.Kind() != slog.KindString || got.String() != "event" {
			t.Errorf("event stream recovery evidence = %v, want event", got)
		}

		secondStream.events <- Event{Action: "disconnect", Network: "backend"}
		advanceLoopTime(t, streamStabilityDelay)
		synctest.Wait()
		if recovered := logRecords(harness.recorder.Records(), slog.LevelInfo, "Docker event stream recovered"); len(recovered) != 1 {
			t.Fatalf("event stream recoveries after later event and stable time = %d, want 1", len(recovered))
		}
	})
}

func TestLoopLogsEventStreamRecoveryAfterStableConnectionOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := newLoopTestSource()
		harness := newLoopTestHarnessWith(t, loopTestConfig(), source, newLoopTestStore(), health.NewTracker(), nil)
		defer harness.stop(t)
		harness.start(t)

		secondStream := newLoopTestStream()
		source.streams <- secondStream
		failLoopTestStream(harness.stream, errors.New("event stream unavailable"))
		receiveLoopValue(t, source.streamCalls, "reconnected event stream")

		advanceLoopTime(t, 29*time.Second)
		synctest.Wait()
		if recovered := logRecords(harness.recorder.Records(), slog.LevelInfo, "Docker event stream recovered"); len(recovered) != 0 {
			t.Fatalf("event stream recoveries before thirty stable seconds = %d, want 0", len(recovered))
		}

		advanceLoopTime(t, time.Second)
		synctest.Wait()
		recovered := logRecords(harness.recorder.Records(), slog.LevelInfo, "Docker event stream recovered")
		if len(recovered) != 1 {
			t.Fatalf("event stream recoveries after stable connection = %d, want 1", len(recovered))
		}
		if got := logAttr(t, recovered[0], "evidence"); got.Kind() != slog.KindString || got.String() != "stable" {
			t.Errorf("stable stream recovery evidence = %v, want stable", got)
		}

		secondStream.events <- Event{Action: "connect", Network: "saltbox"}
		advanceLoopTime(t, 30*time.Second)
		synctest.Wait()
		if recovered := logRecords(harness.recorder.Records(), slog.LevelInfo, "Docker event stream recovered"); len(recovered) != 1 {
			t.Fatalf("event stream recoveries after later event and stable time = %d, want 1", len(recovered))
		}
	})
}

func TestLoopLogsEventStreamClosedReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := newLoopTestSource()
		harness := newLoopTestHarnessWith(t, loopTestConfig(), source, newLoopTestStore(), health.NewTracker(), nil)
		defer harness.stop(t)
		harness.start(t)

		source.streams <- newLoopTestStream()
		close(harness.stream.errors)
		receiveLoopValue(t, source.streamCalls, "reconnected event stream after closure")
		synctest.Wait()

		failures := logRecords(harness.recorder.Records(), slog.LevelWarn, "Docker event stream unavailable")
		if len(failures) != 1 {
			t.Fatalf("event stream failures after closure = %d, want 1", len(failures))
		}
		if got := logAttr(t, failures[0], "err"); got.Kind() != slog.KindString || got.String() != eventStreamClosedMessage {
			t.Errorf("closed stream err = %v, want %q", got, eventStreamClosedMessage)
		}
		if got := logAttr(t, failures[0], "retry_in"); got.Kind() != slog.KindDuration || got.Duration() != retryInitialDelay {
			t.Errorf("closed stream retry_in = %v, want %v", got, retryInitialDelay)
		}
	})
}

func TestLoopStreamStopReady(t *testing.T) {
	tests := []struct {
		name        string
		cancelCtx   bool
		closeServer bool
		want        bool
	}{
		{name: "neither stop is ready", want: false},
		{name: "caller cancellation is ready", cancelCtx: true, want: true},
		{name: "health server termination is ready", closeServer: true, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			serverDone := make(chan struct{})
			if tt.cancelCtx {
				cancel()
			}
			if tt.closeServer {
				close(serverDone)
			}
			if got := loopStreamStopReady(ctx, serverDone); got != tt.want {
				t.Fatalf("loopStreamStopReady() = %t, want %t", got, tt.want)
			}
		})
	}
}

type loopReadyStopCase struct {
	name    string
	stop    func(*loopTestHarness)
	wantErr error
}

func loopReadyStopCases() []loopReadyStopCase {
	serveErr := errors.New("serve failure")
	return []loopReadyStopCase{
		{name: "caller cancellation", stop: func(harness *loopTestHarness) { harness.cancel() }},
		{name: "health server termination", stop: func(harness *loopTestHarness) { harness.server.finish(serveErr) }, wantErr: serveErr},
	}
}

func reconnectLoopTestStream(t *testing.T, harness *loopTestHarness, message string) loopTestStream {
	t.Helper()
	next := newLoopTestStream()
	harness.source.streams <- next
	failLoopTestStream(harness.stream, errors.New(message))
	receiveLoopValue(t, harness.source.streamCalls, "reconnected event stream")
	synctest.Wait()
	return next
}

func assertReadyStopResult(t *testing.T, harness *loopTestHarness, wantErr error) {
	t.Helper()
	err := harness.wait(t)
	if wantErr == nil {
		if err != nil {
			t.Fatalf("Run() error = %v after caller cancellation, want nil", err)
		}
		return
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want wrapped %v", err, wantErr)
	}
}

func assertNoLoopWorkScheduled(t *testing.T, harness *loopTestHarness) {
	t.Helper()
	if calls := harness.source.snapshotCount.Load(); calls != 1 {
		t.Fatalf("Snapshot() calls = %d, want only initial reconciliation", calls)
	}
	select {
	case call := <-harness.source.streamCalls:
		t.Fatalf("unexpected event stream reconnect at %v", call.at)
	default:
	}
}

func assertEventStreamTransitionRecords(t *testing.T, recorder *logRecorder, wantWarnings, wantRecoveries int) {
	t.Helper()
	if warnings := logRecords(recorder.Records(), slog.LevelWarn, "Docker event stream unavailable"); len(warnings) != wantWarnings {
		t.Fatalf("event stream warnings = %d, want %d", len(warnings), wantWarnings)
	}
	if recoveries := logRecords(recorder.Records(), slog.LevelInfo, "Docker event stream recovered"); len(recoveries) != wantRecoveries {
		t.Fatalf("event stream recoveries = %d, want %d", len(recoveries), wantRecoveries)
	}
}

func TestLoopSuppressesReadyStopStreamErrorTransition(t *testing.T) {
	for _, tt := range loopReadyStopCases() {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tracker := health.NewTracker()
				harness := newLoopTestHarnessWith(t, loopTestConfig(), newLoopTestSource(), newLoopTestStore(), tracker, nil)
				harness.start(t)

				tt.stop(harness)
				harness.stream.errors <- errors.New("stream failure during stop")
				assertReadyStopResult(t, harness, tt.wantErr)

				assertActiveConcerns(t, tracker)
				if history := tracker.Snapshot().History; len(history) != 0 {
					t.Fatalf("event stream health history = %d, want 0", len(history))
				}
				assertEventStreamTransitionRecords(t, harness.recorder, 0, 0)
				assertNoLoopWorkScheduled(t, harness)
			})
		})
	}
}

func TestLoopSuppressesReadyStopEventTransition(t *testing.T) {
	for _, tt := range loopReadyStopCases() {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tracker := health.NewTracker()
				harness := newLoopTestHarnessWith(t, loopTestConfig(), newLoopTestSource(), newLoopTestStore(), tracker, nil)
				harness.start(t)
				stream := reconnectLoopTestStream(t, harness, "stream failure before stop")

				tt.stop(harness)
				stream.events <- Event{Action: "connect", Network: "saltbox"}
				assertReadyStopResult(t, harness, tt.wantErr)

				assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
				if history := tracker.Snapshot().History; len(history) != 1 {
					t.Fatalf("event stream health history = %d, want only the original failure", len(history))
				}
				assertEventStreamTransitionRecords(t, harness.recorder, 1, 0)
				assertNoLoopWorkScheduled(t, harness)
			})
		})
	}
}

func TestLoopSuppressesReadyStopStabilityTransition(t *testing.T) {
	for _, tt := range loopReadyStopCases() {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tracker := health.NewTracker()
				harness := newLoopTestHarnessWith(t, loopTestConfig(), newLoopTestSource(), newLoopTestStore(), tracker, nil)
				harness.start(t)
				reconnectLoopTestStream(t, harness, "stream failure before stop")
				advanceLoopTime(t, 29*time.Second)

				tt.stop(harness)
				advanceLoopTime(t, time.Second)
				assertReadyStopResult(t, harness, tt.wantErr)

				assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
				if history := tracker.Snapshot().History; len(history) != 1 {
					t.Fatalf("event stream health history = %d, want only the original failure", len(history))
				}
				assertEventStreamTransitionRecords(t, harness.recorder, 1, 0)
				assertNoLoopWorkScheduled(t, harness)
			})
		})
	}
}

func TestLoopSuppressesReadyStopReconnectTransition(t *testing.T) {
	for _, tt := range loopReadyStopCases() {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tracker := health.NewTracker()
				harness := newLoopTestHarnessWith(t, loopTestConfig(), newLoopTestSource(), newLoopTestStore(), tracker, nil)
				harness.start(t)
				failLoopTestStream(harness.stream, errors.New("stream failure before stop"))
				synctest.Wait()

				tt.stop(harness)
				advanceLoopTime(t, retryInitialDelay)
				assertReadyStopResult(t, harness, tt.wantErr)

				assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
				if history := tracker.Snapshot().History; len(history) != 1 {
					t.Fatalf("event stream health history = %d, want only the original failure", len(history))
				}
				assertEventStreamTransitionRecords(t, harness.recorder, 1, 0)
				assertNoLoopWorkScheduled(t, harness)
			})
		})
	}
}

func TestLoopRetryBackoffStopsAtMaximum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		failure := errors.New("snapshot failure")
		for range 4 {
			source.snapshotResults <- failure
		}
		maximum := 4 * time.Second
		harness := newLoopTestHarnessWith(
			t,
			cfg,
			source,
			newLoopTestStore(),
			health.NewTracker(),
			func(daemon *Daemon) { daemon.timing.retryMaxDelay = maximum },
		)
		defer harness.stop(t)
		previous, _ := harness.start(t)

		for i, want := range []time.Duration{time.Second, 2 * time.Second, maximum, maximum} {
			got := receiveLoopValue(t, source.snapshots, "bounded reconciliation retry")
			if delay := got.at.Sub(previous.at); delay != want {
				t.Fatalf("retry %d delay = %v, want %v", i+1, delay, want)
			}
			previous = got
		}
	})
}

func TestLoopSimultaneousPeriodicAndRetryTriggersOneAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for range 100 {
			cfg := loopTestConfig()
			cfg.PeriodicInterval = retryInitialDelay
			source := newLoopTestSource()
			source.snapshotResults <- errors.New("initial snapshot failure")
			source.snapshotResults <- errors.New("simultaneous-trigger snapshot failure")
			attemptStarted := make(chan struct{}, 1)
			releaseAttempt := make(chan struct{})
			source.onSnapshot = func(ctx context.Context, call int) {
				if call != 2 {
					return
				}
				attemptStarted <- struct{}{}
				select {
				case <-releaseAttempt:
				case <-ctx.Done():
				}
			}
			harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), health.NewTracker(), nil)
			defer harness.stop(t)
			initial, _ := harness.start(t)

			attempt := receiveLoopValue(t, source.snapshots, "simultaneous periodic/retry attempt")
			receiveLoopValue(t, attemptStarted, "blocked simultaneous periodic/retry attempt")
			if delay := attempt.at.Sub(initial.at); delay != retryInitialDelay {
				t.Fatalf("simultaneous-trigger attempt delay = %v, want %v", delay, retryInitialDelay)
			}
			harness.daemon.config.PeriodicInterval = time.Hour
			close(releaseAttempt)
			synctest.Wait()
			assertNoLoopSnapshot(t, source.snapshots)

			nextRetry := receiveLoopValue(t, source.snapshots, "retry after simultaneous triggers")
			if delay := nextRetry.at.Sub(attempt.at); delay != 2*retryInitialDelay {
				t.Fatalf("retry delay after simultaneous triggers = %v, want %v", delay, 2*retryInitialDelay)
			}
			harness.stop(t)
		}
	})
}

func TestLoopEventPreemptsRetryWithoutLeavingDuplicateTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		cfg.DebounceDelay = 250 * time.Millisecond
		cfg.MaxDebounceDelay = 500 * time.Millisecond
		source := newLoopTestSource()
		harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), health.NewTracker(), nil)
		defer harness.stop(t)
		harness.start(t)

		source.snapshotResults <- errors.New("preempted snapshot failure")
		harness.stream.events <- Event{Action: "connect", Network: "saltbox"}
		failedAttempt := receiveLoopValue(t, source.snapshots, "failed reconciliation before preemption")
		synctest.Wait()

		harness.stream.events <- Event{Action: "disconnect", Network: "backend"}
		earlyAttempt := receiveLoopValue(t, source.snapshots, "event-preempted reconciliation")
		if delay := earlyAttempt.at.Sub(failedAttempt.at); delay != cfg.DebounceDelay {
			t.Fatalf("event-preempted attempt delay = %v, want %v", delay, cfg.DebounceDelay)
		}

		advanceLoopTime(t, retryInitialDelay-cfg.DebounceDelay+100*time.Millisecond)
		assertNoLoopSnapshot(t, source.snapshots)

		source.snapshotResults <- errors.New("later snapshot failure")
		harness.stream.events <- Event{Action: "connect", Network: "saltbox"}
		laterFailure := receiveLoopValue(t, source.snapshots, "later failed reconciliation")
		retry := receiveLoopValue(t, source.snapshots, "later reconciliation retry")
		if delay := retry.at.Sub(laterFailure.at); delay != retryInitialDelay {
			t.Fatalf("later retry delay = %v, want %v", delay, retryInitialDelay)
		}
	})
}

func TestLoopRetryPreemptsPendingDebounceWithoutDuplicateAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		source.snapshotResults <- errors.New("initial snapshot failure")
		harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), health.NewTracker(), nil)
		defer harness.stop(t)
		harness.start(t)

		harness.stream.events <- Event{Action: "connect", Network: "saltbox"}
		receiveLoopValue(t, source.snapshots, "retry preempting pending debounce")
		advanceLoopTime(t, cfg.DebounceDelay)
		assertNoLoopSnapshot(t, source.snapshots)
	})
}

func TestLoopReconnectBackoffProgressesAcrossFreshChannels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		source.mutateNetworks = func(networks []string) { networks[0] = "mutated" }
		tracker := health.NewTracker()
		harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), tracker, nil)
		defer harness.stop(t)
		_, currentCall := harness.start(t)
		if !slices.Equal(currentCall.networks, []string{"saltbox", "backend"}) {
			t.Fatalf("initial Events() networks = %v, want complete [saltbox backend]", currentCall.networks)
		}
		currentStream := harness.stream

		for i, wantDelay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
			nextStream := newLoopTestStream()
			source.streams <- nextStream
			failedAt := time.Now()
			failLoopTestStream(currentStream, errors.New("event stream failure"))
			synctest.Wait()
			assertActiveConcerns(t, tracker, health.ConcernDockerEvents)

			nextCall := receiveLoopValue(t, source.streamCalls, "reconnected event stream")
			if delay := nextCall.at.Sub(failedAt); delay != wantDelay {
				t.Fatalf("reconnect %d delay = %v, want %v", i+1, delay, wantDelay)
			}
			if currentCall.ctx.Err() != context.Canceled {
				t.Fatalf("stream %d context error = %v before reconnect, want canceled", i+1, currentCall.ctx.Err())
			}
			if !slices.Equal(nextCall.networks, []string{"saltbox", "backend"}) {
				t.Fatalf("Events() call %d networks = %v, want complete [saltbox backend]", i+2, nextCall.networks)
			}
			synctest.Wait()
			assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
			currentCall = nextCall
			currentStream = nextStream
		}

		harness.stop(t)
		if currentCall.ctx.Err() != context.Canceled {
			t.Fatalf("current stream context error after Run = %v, want canceled", currentCall.ctx.Err())
		}
		if allCanceled := receiveLoopValue(t, source.allCanceled, "source close stream check"); !allCanceled {
			t.Fatal("NetworkSource.Close() observed an uncanceled event stream context")
		}
	})
}

func TestLoopProductionShapedStreamFailurePreservesBufferedError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for range 100 {
			tracker := health.NewTracker()
			harness := newLoopTestHarnessWith(
				t,
				loopTestConfig(),
				newLoopTestSource(),
				newLoopTestStore(),
				tracker,
				nil,
			)
			defer harness.stop(t)
			harness.start(t)

			streamErr := errors.New("production-shaped stream failure")
			failLoopTestStream(harness.stream, streamErr)
			synctest.Wait()
			assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
			assertNewestHealthMessage(t, tracker, streamErr.Error())
			harness.stop(t)
		}
	})
}

func TestLoopValidEventResetsReconnectBackoffAndRecoversHealth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		tracker := health.NewTracker()
		harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), tracker, nil)
		defer harness.stop(t)
		harness.start(t)

		secondStream := newLoopTestStream()
		source.streams <- secondStream
		failLoopTestStream(harness.stream, errors.New("first stream failure"))
		receiveLoopValue(t, source.streamCalls, "first reconnected stream")

		thirdStream := newLoopTestStream()
		source.streams <- thirdStream
		failLoopTestStream(secondStream, errors.New("second stream failure"))
		receiveLoopValue(t, source.streamCalls, "second reconnected stream")
		synctest.Wait()
		assertActiveConcerns(t, tracker, health.ConcernDockerEvents)

		thirdStream.events <- Event{Action: "connect", Network: "backend"}
		synctest.Wait()
		assertActiveConcerns(t, tracker)

		fourthStream := newLoopTestStream()
		source.streams <- fourthStream
		failedAt := time.Now()
		failLoopTestStream(thirdStream, errors.New("failure after valid event"))
		nextCall := receiveLoopValue(t, source.streamCalls, "event-reset reconnected stream")
		if delay := nextCall.at.Sub(failedAt); delay != retryInitialDelay {
			t.Fatalf("reconnect delay after valid event = %v, want reset %v", delay, retryInitialDelay)
		}
	})
}

func TestLoopStableStreamResetsReconnectBackoffAndRecoversHealth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		tracker := health.NewTracker()
		harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), tracker, nil)
		defer harness.stop(t)
		harness.start(t)

		secondStream := newLoopTestStream()
		source.streams <- secondStream
		failLoopTestStream(harness.stream, errors.New("first stream failure"))
		receiveLoopValue(t, source.streamCalls, "first reconnected stream")

		thirdStream := newLoopTestStream()
		source.streams <- thirdStream
		failLoopTestStream(secondStream, errors.New("second stream failure"))
		receiveLoopValue(t, source.streamCalls, "second reconnected stream")
		synctest.Wait()
		assertActiveConcerns(t, tracker, health.ConcernDockerEvents)

		advanceLoopTime(t, streamStabilityDelay)
		synctest.Wait()
		assertActiveConcerns(t, tracker)

		fourthStream := newLoopTestStream()
		source.streams <- fourthStream
		failedAt := time.Now()
		failLoopTestStream(thirdStream, errors.New("failure after stable stream"))
		nextCall := receiveLoopValue(t, source.streamCalls, "stability-reset reconnected stream")
		if delay := nextCall.at.Sub(failedAt); delay != retryInitialDelay {
			t.Fatalf("reconnect delay after stable stream = %v, want reset %v", delay, retryInitialDelay)
		}
	})
}

func TestLoopStreamFailureOutranksExpiredStabilityAfterBlockedReconcile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for range 100 {
			cfg := loopTestConfig()
			cfg.PeriodicInterval = 4 * time.Second
			source := newLoopTestSource()
			store := newLoopTestStore()
			releaseApply := make(chan struct{})
			store.onApply = func(ctx context.Context, call int) error {
				if call != 2 {
					return nil
				}
				select {
				case <-releaseApply:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			tracker := health.NewTracker()
			harness := newLoopTestHarnessWith(t, cfg, source, store, tracker, nil)
			defer harness.stop(t)
			harness.start(t)
			if call := receiveLoopValue(t, store.applyCalls, "initial apply"); call != 1 {
				t.Fatalf("initial Apply() call = %d, want 1", call)
			}

			secondStream := newLoopTestStream()
			source.streams <- secondStream
			failLoopTestStream(harness.stream, errors.New("first stream failure"))
			receiveLoopValue(t, source.streamCalls, "first reconnected stream")

			thirdStream := newLoopTestStream()
			source.streams <- thirdStream
			failLoopTestStream(secondStream, errors.New("second stream failure"))
			thirdCall := receiveLoopValue(t, source.streamCalls, "second reconnected stream")

			receiveLoopValue(t, source.snapshots, "periodic reconciliation before stability race")
			if call := receiveLoopValue(t, store.applyCalls, "blocked apply before stability race"); call != 2 {
				t.Fatalf("blocked Apply() call = %d, want 2", call)
			}

			failureAt := thirdCall.at.Add(streamStabilityDelay - time.Second)
			advanceLoopTime(t, failureAt.Sub(time.Now()))
			streamErr := errors.New("stream failure before stability threshold")
			failLoopTestStream(thirdStream, streamErr)
			advanceLoopTime(t, 2*time.Second)

			fourthStream := newLoopTestStream()
			source.streams <- fourthStream
			releasedAt := time.Now()
			close(releaseApply)
			nextCall := receiveLoopValue(t, source.streamCalls, "reconnect after expired stability race")
			if delay := nextCall.at.Sub(releasedAt); delay != 4*time.Second {
				t.Fatalf("reconnect delay after expired stability race = %v, want 4s", delay)
			}
			synctest.Wait()
			assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
			assertNewestHealthMessage(t, tracker, streamErr.Error())
			harness.stop(t)
		}
	})
}

func TestLoopClosedStreamChannelReconnectsAndDegradesHealth(t *testing.T) {
	tests := []struct {
		name       string
		disconnect func(loopTestStream)
	}{
		{name: "event channel", disconnect: func(stream loopTestStream) { close(stream.events) }},
		{name: "error channel", disconnect: func(stream loopTestStream) { close(stream.errors) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := loopTestConfig()
				source := newLoopTestSource()
				tracker := health.NewTracker()
				harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), tracker, nil)
				defer harness.stop(t)
				_, firstCall := harness.start(t)

				nextStream := newLoopTestStream()
				source.streams <- nextStream
				closedAt := time.Now()
				tt.disconnect(harness.stream)
				nextCall := receiveLoopValue(t, source.streamCalls, "reconnect after channel closure")
				if delay := nextCall.at.Sub(closedAt); delay != retryInitialDelay {
					t.Fatalf("reconnect delay = %v, want %v", delay, retryInitialDelay)
				}
				if firstCall.ctx.Err() != context.Canceled {
					t.Fatalf("closed stream context error = %v, want canceled", firstCall.ctx.Err())
				}
				assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
			})
		})
	}
}

func TestLoopReadyStopOutranksPendingPeriodicReconciliation(t *testing.T) {
	tests := []struct {
		name string
		stop func(*loopTestHarness, error)
	}{
		{name: "caller cancellation", stop: func(harness *loopTestHarness, _ error) { harness.cancel() }},
		{name: "health server completion", stop: func(harness *loopTestHarness, serveErr error) { harness.server.finish(serveErr) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				for range 100 {
					cfg := loopTestConfig()
					cfg.PeriodicInterval = time.Second
					source := newLoopTestSource()
					streamBlocked := make(chan struct{}, 1)
					releaseStream := make(chan struct{})
					source.onEvents = func(_ context.Context, call int) {
						if call != 1 {
							return
						}
						streamBlocked <- struct{}{}
						<-releaseStream
					}
					harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), health.NewTracker(), nil)
					defer func() { _ = harness.shutdown(t) }()
					harness.start(t)
					receiveLoopValue(t, streamBlocked, "blocked initial event stream")
					serveErr := errors.New("serve failure")

					advanceLoopTime(t, cfg.PeriodicInterval)
					tt.stop(harness, serveErr)
					close(releaseStream)

					err := harness.wait(t)
					if tt.name == "caller cancellation" {
						if err != nil {
							t.Fatalf("Run() error = %v after caller cancellation, want nil", err)
						}
					} else if !errors.Is(err, serveErr) {
						t.Fatalf("Run() error = %v, want wrapped %v", err, serveErr)
					}
					if calls := harness.source.snapshotCount.Load(); calls != 1 {
						t.Fatalf("Snapshot() calls = %d, want only initial reconciliation", calls)
					}
				}
			})
		})
	}
}

func TestLoopHealthServerTerminationCancelsStreamAndPreservesClassification(t *testing.T) {
	tests := []struct {
		name     string
		serveErr error
	}{
		{name: "terminal serve error", serveErr: errors.New("serve failure")},
		{name: "nil completion remains unexpected"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				harness := newLoopTestHarness(t, loopTestConfig())
				defer func() { _ = harness.shutdown(t) }()
				_, streamCall := harness.start(t)

				harness.server.finish(tt.serveErr)
				err := harness.shutdown(t)
				if tt.serveErr != nil {
					if !errors.Is(err, tt.serveErr) {
						t.Fatalf("Run() error = %v, want wrapped %v", err, tt.serveErr)
					}
				} else if !errors.Is(err, errHealthServerStopped) {
					t.Fatalf("Run() error = %v, want unexpected nil-completion error", err)
				}
				if streamCall.ctx.Err() != context.Canceled {
					t.Fatalf("stream context error after health stop = %v, want canceled", streamCall.ctx.Err())
				}
				if allCanceled := receiveLoopValue(t, harness.source.allCanceled, "source close stream check"); !allCanceled {
					t.Fatal("NetworkSource.Close() observed an uncanceled stream after health stop")
				}
			})
		})
	}
}

func TestRunHealthWatcherCancelsBlockedApplyAndJoinsBeforeReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		source := newLoopTestSource()
		store := newLoopTestStore()
		store.onApply = func(ctx context.Context, call int) error {
			if call != 1 {
				return nil
			}
			<-ctx.Done()
			return ctx.Err()
		}
		tracker := health.NewTracker()
		harness := newLoopTestHarnessWith(t, cfg, source, store, tracker, nil)
		defer func() { _ = harness.shutdown(t) }()
		receiveLoopValue(t, source.snapshots, "initial reconciliation before blocked apply")
		if call := receiveLoopValue(t, store.applyCalls, "blocked initial apply"); call != 1 {
			t.Fatalf("blocked Apply() call = %d, want 1", call)
		}

		serveErr := errors.New("serve failure during apply")
		harness.server.finish(serveErr)
		err := harness.wait(t)
		if !errors.Is(err, serveErr) {
			t.Fatalf("Run() error = %v, want wrapped %v", err, serveErr)
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, must not expose watcher cancellation", err)
		}
		if calls := source.snapshotCount.Load(); calls != 1 {
			t.Fatalf("Snapshot() calls = %d, want only blocked initial reconciliation", calls)
		}
		assertActiveConcerns(t, tracker)
	})
}

func loopTestConfig() Config {
	return Config{
		Networks:         []string{"saltbox", "backend"},
		DefaultNetwork:   "saltbox",
		PeriodicInterval: time.Minute,
		DebounceDelay:    2 * time.Second,
		MaxDebounceDelay: 5 * time.Second,
	}
}

func receiveLoopValue[T any](t *testing.T, values <-chan T, operation string) T {
	t.Helper()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	select {
	case value, ok := <-values:
		if !ok {
			t.Fatalf("%s channel closed before yielding a value", operation)
		}
		return value
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", operation)
		var zero T
		return zero
	}
}

func advanceLoopTime(t *testing.T, duration time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), duration)
	defer cancel()
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("logical time advance error = %v, want deadline exceeded", ctx.Err())
	}
}

func assertNoLoopSnapshot(t *testing.T, snapshots <-chan loopTestSnapshot) {
	t.Helper()
	synctest.Wait()
	select {
	case snapshot := <-snapshots:
		t.Fatalf("unexpected reconciliation at %v", snapshot.at)
	default:
	}
}
