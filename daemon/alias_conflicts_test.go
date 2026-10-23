package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/saltyorg/sdhm/health"
)

func TestRunAliasConflictsAllowUpdatesAndLogOnlyTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loopTestConfig()
		cfg.PeriodicInterval = time.Hour
		conflict := AliasConflict{
			Hostname: "web.backend",
			Endpoints: []AliasConflictEndpoint{
				{Network: "backend", IP: netip.MustParseAddr("172.20.0.2")},
				{Network: "saltbox", IP: netip.MustParseAddr("172.18.0.2")},
			},
		}
		store := &aliasTestStore{}
		store.set(ApplyResult{Conflicts: []AliasConflict{conflict}}, nil)
		tracker := health.NewTracker()
		harness := newLoopTestHarnessWith(t, cfg, newLoopTestSource(), store, tracker, nil)
		defer harness.stop(t)
		harness.start(t)
		synctest.Wait()
		assertAliasHealth(t, tracker, http.StatusServiceUnavailable, "web.backend", "backend", "172.20.0.2", "saltbox", "172.18.0.2")
		initialHistory := len(tracker.Snapshot().History)
		if records := logRecords(harness.recorder.Records(), slog.LevelError, "conflicting hostnames omitted"); len(records) != 1 {
			t.Fatalf("conflict log records = %d, want 1", len(records))
		}

		// A partial successful update must not enter the failed-write retry loop.
		advanceLoopTime(t, retryMaxDelay+time.Second)
		if got := harness.source.snapshotCount.Load(); got != 1 {
			t.Fatalf("snapshot attempts = %d, want only initial update", got)
		}
		advanceLoopTime(t, cfg.PeriodicInterval-retryMaxDelay-time.Second)
		synctest.Wait()
		if got := harness.source.snapshotCount.Load(); got != 2 {
			t.Fatalf("snapshot attempts = %d, want periodic updates to continue", got)
		}
		if got := len(tracker.Snapshot().History); got != initialHistory {
			t.Fatalf("unchanged conflict history = %d, want %d", got, initialHistory)
		}
		if records := logRecords(harness.recorder.Records(), slog.LevelError, "conflicting hostnames omitted"); len(records) != 1 {
			t.Fatalf("unchanged conflict logs = %d, want 1", len(records))
		}

		conflict.Endpoints[0].IP = netip.MustParseAddr("172.20.0.3")
		store.set(ApplyResult{Conflicts: []AliasConflict{conflict}}, nil)
		advanceLoopTime(t, cfg.PeriodicInterval)
		synctest.Wait()
		assertAliasHealth(t, tracker, http.StatusServiceUnavailable, "172.20.0.3")
		if records := logRecords(harness.recorder.Records(), slog.LevelError, "conflicting hostnames omitted"); len(records) != 2 {
			t.Fatalf("changed conflict logs = %d, want 2", len(records))
		}

		// Failed writes cannot claim that a previously published conflict cleared.
		store.set(ApplyResult{}, errors.New("disk unavailable"))
		advanceLoopTime(t, cfg.PeriodicInterval)
		synctest.Wait()
		assertAliasHealth(t, tracker, http.StatusServiceUnavailable)
		if records := logRecords(harness.recorder.Records(), slog.LevelInfo, "hosts alias conflicts resolved"); len(records) != 0 {
			t.Fatalf("recovery logs after failed write = %d, want 0", len(records))
		}

		store.set(ApplyResult{}, nil)
		advanceLoopTime(t, retryInitialDelay)
		synctest.Wait()
		assertAliasHealth(t, tracker, http.StatusOK)
		if records := logRecords(harness.recorder.Records(), slog.LevelInfo, "hosts alias conflicts resolved"); len(records) != 1 {
			t.Fatalf("recovery logs = %d, want 1", len(records))
		}
		advanceLoopTime(t, cfg.PeriodicInterval)
		synctest.Wait()
		if records := logRecords(harness.recorder.Records(), slog.LevelInfo, "hosts alias conflicts resolved"); len(records) != 1 {
			t.Fatalf("repeated recovery logs = %d, want 1", len(records))
		}
	})
}

type aliasTestStore struct {
	mu     sync.Mutex
	result ApplyResult
	err    error
}

func (*aliasTestStore) Prepare(context.Context) (PrepareResult, error) {
	return PrepareResult{}, nil
}

func (s *aliasTestStore) Apply(context.Context, []Endpoint) (ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}

func (s *aliasTestStore) set(result ApplyResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result = ApplyResult{Conflicts: make([]AliasConflict, len(result.Conflicts))}
	for i, conflict := range result.Conflicts {
		s.result.Conflicts[i] = AliasConflict{
			Hostname:  conflict.Hostname,
			Endpoints: append([]AliasConflictEndpoint(nil), conflict.Endpoints...),
		}
	}
	s.err = err
}

func assertAliasHealth(t *testing.T, tracker *health.Tracker, status int, details ...string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	health.NewHandler(tracker).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != status {
		t.Fatalf("health status = %d, want %d: %s", recorder.Code, status, recorder.Body.String())
	}
	for _, detail := range details {
		if !strings.Contains(recorder.Body.String(), detail) {
			t.Errorf("health response %s missing %q", recorder.Body.String(), detail)
		}
	}
}
