package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/saltyorg/sdhm/health"
)

func newNotifyingLogRecorder() *logRecorder {
	return &logRecorder{notify: make(chan struct{}, 1)}
}

func waitForInitialReconciliation(t *testing.T, recorder *logRecorder) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		for _, record := range recorder.Records() {
			if record.Message == "SDHM ready" {
				return
			}
			if record.Message == "reconciliation failed" {
				if phase, ok := logAttrIfPresent(record, "phase"); ok && phase.String() == "initial" {
					return
				}
			}
		}
		select {
		case <-recorder.notify:
		case <-deadline.C:
			t.Fatal("initial reconciliation did not complete")
		}
	}
}

func TestRunSubscribesBeforeInitialSnapshot(t *testing.T) {
	log := &operationLog{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	subscribed := false
	source := &orderedSource{
		log:         log,
		events:      make(chan Event),
		eventErrors: make(chan error),
		onEvents:    func(context.Context, []string) { subscribed = true },
		onSnapshot: func(context.Context) {
			if !subscribed {
				t.Error("initial snapshot ran before event subscription")
			}
		},
	}
	store := &orderedStore{log: log, onApply: func(context.Context) { cancel() }}
	daemon := mustNewDaemon(t, validConfig(), source, store, health.NewTracker(), newOrderedHealthServer(log))
	if err := daemon.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLoopReconnectReconcilesBeforeStreamRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		cfg.PeriodicInterval = 24 * time.Hour
		source := newLoopTestSource()
		tracker := health.NewTracker()
		harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), tracker, nil)
		defer harness.stop(t)
		harness.start(t)
		next := newLoopTestStream()
		source.streams <- next
		failLoopTestStream(harness.stream, errors.New("connection lost"))
		reconnected := receiveLoopValue(t, source.streamCalls, "reconnected stream")
		synctest.Wait()
		if calls := source.snapshotCount.Load(); calls != 2 {
			t.Fatalf("snapshot calls after reconnect = %d, want initial and immediate reconnect reconciliation", calls)
		}
		resync := receiveLoopValue(t, source.snapshots, "reconnect reconciliation")
		if !resync.at.Equal(reconnected.at) {
			t.Fatalf("reconnect reconciliation at %v, want immediate at %v", resync.at, reconnected.at)
		}
		assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
		advanceLoopTime(t, streamStabilityDelay)
		synctest.Wait()
		assertActiveConcerns(t, tracker)
	})
}

func TestRunImmediateSubscriptionFailureKeepsHealthDegraded(t *testing.T) {
	log := &operationLog{}
	errs := make(chan error, 1)
	errs <- context.DeadlineExceeded
	close(errs)
	source := &orderedSource{log: log, events: make(chan Event), eventErrors: errs}
	tracker := health.NewTracker()
	recorder := newNotifyingLogRecorder()
	daemon := mustNewDaemonWithLogger(t, validConfig(), source, &orderedStore{log: log}, tracker, newOrderedHealthServer(log), slog.New(recorder))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- daemon.Run(ctx) }()
	waitForInitialReconciliation(t, recorder)
	assertReadiness(t, tracker, true)
	assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
	assertNewestHealthMessage(t, tracker, context.DeadlineExceeded.Error())
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

type blockingReadyWriter struct {
	ready   chan struct{}
	release chan struct{}
}

func (w *blockingReadyWriter) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("SDHM ready")) {
		close(w.ready)
		<-w.release
	}
	return len(data), nil
}

func TestRunStreamFailureDuringInitialSnapshotIsUnhealthyBeforeReadyLogCompletes(t *testing.T) {
	log := &operationLog{}
	tracker := health.NewTracker()
	eventErrors := make(chan error, 1)
	source := &orderedSource{
		log: log, events: make(chan Event), eventErrors: eventErrors,
		onSnapshot: func(context.Context) { eventErrors <- errors.New("stream failed during initial snapshot") },
	}
	writer := &blockingReadyWriter{ready: make(chan struct{}), release: make(chan struct{})}
	daemon := mustNewDaemonWithLogger(t, validConfig(), source, &orderedStore{log: log}, tracker,
		newOrderedHealthServer(log), slog.New(slog.NewTextHandler(writer, nil)))
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- daemon.Run(ctx) }()
	<-writer.ready
	response := httptest.NewRecorder()
	health.NewHandler(tracker).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	cancel()
	close(writer.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("health after stream failure = %d, want 503; body %s", response.Code, response.Body)
	}
}

func TestRunSubscriptionFailureDoesNotStopPeriodicReconciliation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		cfg.PeriodicInterval = 500 * time.Millisecond
		source := newLoopTestSource()
		failed := newLoopTestStream()
		failLoopTestStream(failed, context.DeadlineExceeded)
		source.streams <- failed
		source.onEvents = func(_ context.Context, call int) {
			if call == 1 {
				time.Sleep(dockerOperationTimeout)
			}
		}
		tracker := health.NewTracker()
		harness := newLoopTestHarnessWith(t, cfg, source, newLoopTestStore(), tracker, nil)
		defer harness.stop(t)
		initial, _ := harness.start(t)
		assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
		periodic := receiveLoopValue(t, source.snapshots, "periodic reconciliation after failed subscription")
		if delay := periodic.at.Sub(initial.at); delay != cfg.PeriodicInterval {
			t.Fatalf("periodic delay after subscription failure = %v, want %v", delay, cfg.PeriodicInterval)
		}
		assertActiveConcerns(t, tracker, health.ConcernDockerEvents)
	})
}
