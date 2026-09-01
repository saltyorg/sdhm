package health

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTrackerTransitions(t *testing.T) {
	fixed := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	tracker := newTracker(10, func() time.Time { return fixed })

	tracker.Fail(ConcernDockerSnapshot, "list failed")
	first := tracker.Snapshot()
	if len(first.History) != 1 {
		t.Fatalf("first history length = %d, want 1", len(first.History))
	}
	if got, want := first.Active[ConcernDockerSnapshot], first.History[0].id; got != want {
		t.Fatalf("first active ID = %d, want %d", got, want)
	}
	assertRecord(t, first.History[0], fixed, "list failed", "docker", SeverityMajor)

	tracker.Fail(ConcernDockerSnapshot, "list failed again")
	second := tracker.Snapshot()
	if len(second.History) != 2 {
		t.Fatalf("second history length = %d, want 2", len(second.History))
	}
	if got, want := second.Active[ConcernDockerSnapshot], second.History[1].id; got != want {
		t.Fatalf("second active ID = %d, want %d", got, want)
	}
	if second.History[0].id == second.Active[ConcernDockerSnapshot] {
		t.Fatal("superseded record remained active")
	}

	tracker.Recover(ConcernDockerSnapshot)
	recovered := tracker.Snapshot()
	if len(recovered.Active) != 0 {
		t.Fatalf("recovered active conditions = %v, want none", recovered.Active)
	}
	if len(recovered.History) != 2 {
		t.Fatalf("recovered history length = %d, want 2", len(recovered.History))
	}
}

func TestTrackerConcernMappings(t *testing.T) {
	tests := []struct {
		concern   Concern
		errorType string
		severity  Severity
	}{
		{ConcernDockerSnapshot, "docker", SeverityMajor},
		{ConcernDockerEvents, "docker_events", SeverityMinor},
		{ConcernHostsApply, "update", SeverityMajor},
		{ConcernRecovery, "validation", SeverityCritical},
	}

	for _, tt := range tests {
		t.Run(string(tt.concern), func(t *testing.T) {
			tracker := newTracker(10, time.Now)
			tracker.Fail(tt.concern, "failed")

			snapshot := tracker.Snapshot()
			if len(snapshot.History) != 1 {
				t.Fatalf("history length = %d, want 1", len(snapshot.History))
			}
			assertRecord(t, snapshot.History[0], snapshot.History[0].Timestamp, "failed", tt.errorType, tt.severity)
		})
	}
}

func TestTrackerEvictsOldestInactiveRecordWhilePreservingActiveRecords(t *testing.T) {
	tracker := newTracker(10, time.Now)
	tracker.Fail(ConcernDockerSnapshot, "snapshot failure")
	for i := range 10 {
		tracker.Fail(ConcernDockerEvents, fmt.Sprintf("event failure %d", i))
	}

	snapshot := tracker.Snapshot()
	if len(snapshot.History) != 10 {
		t.Fatalf("history length = %d, want 10", len(snapshot.History))
	}
	if got := snapshot.History[0].id; got != 1 {
		t.Fatalf("oldest retained ID = %d, want active ID 1", got)
	}
	if got, want := snapshot.Active[ConcernDockerSnapshot], uint64(1); got != want {
		t.Fatalf("snapshot active ID = %d, want %d", got, want)
	}
	if got, want := snapshot.Active[ConcernDockerEvents], uint64(11); got != want {
		t.Fatalf("events active ID = %d, want %d", got, want)
	}
	for _, record := range snapshot.History {
		if record.id == 2 {
			t.Fatal("oldest inactive record ID 2 was not evicted")
		}
	}
}

func TestTrackerSnapshotIsIndependent(t *testing.T) {
	tracker := newTracker(10, time.Now)
	tracker.Fail(ConcernDockerSnapshot, "list failed")

	snapshot := tracker.Snapshot()
	snapshot.Active[ConcernDockerSnapshot] = 0
	snapshot.History[0].Message = "mutated"

	fresh := tracker.Snapshot()
	if got, want := fresh.Active[ConcernDockerSnapshot], uint64(1); got != want {
		t.Fatalf("active ID after snapshot mutation = %d, want %d", got, want)
	}
	if got, want := fresh.History[0].Message, "list failed"; got != want {
		t.Fatalf("history message after snapshot mutation = %q, want %q", got, want)
	}
}

func TestTrackerAndHandlerAreConcurrentSafe(t *testing.T) {
	tracker := NewTracker()
	handler := NewHandler(tracker)

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for worker := range 3 {
		wg.Go(func() {
			concern := []Concern{ConcernDockerSnapshot, ConcernDockerEvents, ConcernHostsApply}[worker]
			for i := range 1_000 {
				tracker.Fail(concern, fmt.Sprintf("failure %d", i))
				tracker.Recover(concern)
			}
		})
	}
	for range 3 {
		wg.Go(func() {
			for range 1_000 {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
				if recorder.Code != http.StatusOK && recorder.Code != http.StatusServiceUnavailable {
					errs <- fmt.Errorf("health status = %d", recorder.Code)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func assertRecord(t *testing.T, got Record, timestamp time.Time, message, errorType string, severity Severity) {
	t.Helper()
	if got.Timestamp != timestamp {
		t.Errorf("timestamp = %s, want %s", got.Timestamp, timestamp)
	}
	if got.Message != message {
		t.Errorf("message = %q, want %q", got.Message, message)
	}
	if got.ErrorType != errorType {
		t.Errorf("error type = %q, want %q", got.ErrorType, errorType)
	}
	if got.Severity != severity {
		t.Errorf("severity = %q, want %q", got.Severity, severity)
	}
}
