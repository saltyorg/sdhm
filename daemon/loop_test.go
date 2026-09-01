package daemon

import (
	"context"
	"errors"
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

type loopTestSource struct {
	streams         chan loopTestStream
	streamCalls     chan loopTestStreamCall
	snapshots       chan loopTestSnapshot
	snapshotResults chan error
	closed          chan struct{}
	allCanceled     chan bool
	mutateNetworks  func([]string)
	closeOnce       sync.Once
	streamMu        sync.Mutex
	streamContexts  []context.Context
}

type loopTestStore struct {
	applyCalls chan int
	onApply    func(context.Context, int) error
	count      atomic.Int64
}

func newLoopTestStore() *loopTestStore {
	return &loopTestStore{applyCalls: make(chan int, 64)}
}

func (*loopTestStore) Prepare(context.Context) error {
	return nil
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
	call := loopTestSnapshot{at: time.Now(), networks: slices.Clone(networks)}
	select {
	case s.snapshots <- call:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case err := <-s.snapshotResults:
		return nil, err
	default:
		return nil, nil
	}
}

func (s *loopTestSource) Events(ctx context.Context, networks []string) (<-chan Event, <-chan error) {
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
	daemon := mustNewDaemon(
		t,
		cfg,
		source,
		store,
		tracker,
		server,
	)
	if configure != nil {
		configure(daemon)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- daemon.Run(ctx) }()

	return &loopTestHarness{
		source: source,
		stream: stream,
		server: server,
		cancel: cancel,
		result: result,
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
	h.stopOnce.Do(func() {
		h.cancel()
		h.stopErr = receiveLoopValue(t, h.result, "daemon shutdown")
	})
	return h.stopErr
}

func newLoopTestStream() loopTestStream {
	return loopTestStream{events: make(chan Event, 16), errors: make(chan error, 16)}
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
			currentStream.errors <- errors.New("event stream failure")
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
		harness.stream.errors <- errors.New("first stream failure")
		receiveLoopValue(t, source.streamCalls, "first reconnected stream")

		thirdStream := newLoopTestStream()
		source.streams <- thirdStream
		secondStream.errors <- errors.New("second stream failure")
		receiveLoopValue(t, source.streamCalls, "second reconnected stream")
		synctest.Wait()
		assertActiveConcerns(t, tracker, health.ConcernDockerEvents)

		thirdStream.events <- Event{Action: "connect", Network: "backend"}
		synctest.Wait()
		assertActiveConcerns(t, tracker)

		fourthStream := newLoopTestStream()
		source.streams <- fourthStream
		failedAt := time.Now()
		thirdStream.errors <- errors.New("failure after valid event")
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
		harness.stream.errors <- errors.New("first stream failure")
		receiveLoopValue(t, source.streamCalls, "first reconnected stream")

		thirdStream := newLoopTestStream()
		source.streams <- thirdStream
		secondStream.errors <- errors.New("second stream failure")
		receiveLoopValue(t, source.streamCalls, "second reconnected stream")
		synctest.Wait()
		assertActiveConcerns(t, tracker, health.ConcernDockerEvents)

		advanceLoopTime(t, streamStabilityDelay)
		synctest.Wait()
		assertActiveConcerns(t, tracker)

		fourthStream := newLoopTestStream()
		source.streams <- fourthStream
		failedAt := time.Now()
		thirdStream.errors <- errors.New("failure after stable stream")
		nextCall := receiveLoopValue(t, source.streamCalls, "stability-reset reconnected stream")
		if delay := nextCall.at.Sub(failedAt); delay != retryInitialDelay {
			t.Fatalf("reconnect delay after stable stream = %v, want reset %v", delay, retryInitialDelay)
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
