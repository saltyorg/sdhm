package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type response struct {
	Healthy          bool          `json:"healthy"`
	Status           string        `json:"status"`
	Message          string        `json:"message"`
	ErrorCount       int           `json:"error_count"`
	Errors           []errorRecord `json:"errors"`
	Reason           string        `json:"reason"`
	TimeUntilHealthy string        `json:"time_until_healthy"`
}

type errorRecord struct {
	Timestamp          time.Time `json:"timestamp"`
	Message            string    `json:"message"`
	ErrorType          string    `json:"error_type"`
	Severity           Severity  `json:"severity"`
	RecoveryPeriod     string    `json:"recovery_period"`
	TimeUntilRecovered string    `json:"time_until_recovered"`
}

func TestHandlerEmptyTracker(t *testing.T) {
	result, recorder := serve(t, NewTracker())
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got, want := recorder.Header().Get("Content-Type"), "application/json"; got != want {
		t.Fatalf("content type = %q, want %q", got, want)
	}
	if !result.Healthy || result.Status != "ok" || result.Message != "No errors recorded" {
		t.Fatalf("response = %#v, want a healthy empty response", result)
	}
	if result.ErrorCount != 0 || len(result.Errors) != 0 {
		t.Fatalf("history = %d records, want 0", result.ErrorCount)
	}
}

func TestHandlerInitializing(t *testing.T) {
	tracker := NewTracker()
	tracker.BeginInitialization()

	result, recorder := serve(t, tracker)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if result.Healthy || result.Status != "degraded" {
		t.Fatalf("response health = %#v, want degraded", result)
	}
	if got, want := result.Message, "System initializing"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if got, want := result.Reason, "initialization has not completed"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	if got, want := result.TimeUntilHealthy, "unknown"; got != want {
		t.Fatalf("time until healthy = %q, want %q", got, want)
	}
	if result.ErrorCount != 0 || len(result.Errors) != 0 {
		t.Fatalf("initialization history = %d records, want 0", result.ErrorCount)
	}

	tracker.CompleteInitialization()
	result, recorder = serve(t, tracker)
	if recorder.Code != http.StatusOK || !result.Healthy {
		t.Fatalf("response after initialization = (%d, %#v), want healthy", recorder.Code, result)
	}
}

func TestHandlerActiveFailure(t *testing.T) {
	tracker := newTracker(10, func() time.Time { return time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC) })
	tracker.Fail(ConcernDockerSnapshot, "list failed")

	result, recorder := serve(t, tracker)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if result.Healthy || result.Status != "degraded" {
		t.Fatalf("response health = %#v, want degraded", result)
	}
	if got, want := result.Message, "System degraded: 1 active condition(s)"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if got, want := result.Reason, "list failed"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	if got, want := result.TimeUntilHealthy, "unknown"; got != want {
		t.Fatalf("time until healthy = %q, want %q", got, want)
	}
	if got := result.Errors; len(got) != 1 || got[0].RecoveryPeriod != "unknown" || got[0].TimeUntilRecovered != "unknown" {
		t.Fatalf("errors = %#v, want one active unknown record", got)
	}
}

func TestHandlerRepeatedFailureSupersedesHistory(t *testing.T) {
	tracker := NewTracker()
	tracker.Fail(ConcernDockerSnapshot, "first failure")
	tracker.Fail(ConcernDockerSnapshot, "second failure")

	result, recorder := serve(t, tracker)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got, want := result.ErrorCount, 2; got != want {
		t.Fatalf("error count = %d, want %d", got, want)
	}
	if got, want := result.Reason, "second failure"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	if got := result.Errors; len(got) != 2 || got[0].RecoveryPeriod != "0s" || got[0].TimeUntilRecovered != "0s" || got[1].RecoveryPeriod != "unknown" || got[1].TimeUntilRecovered != "unknown" {
		t.Fatalf("errors = %#v, want superseded then active record", got)
	}
}

func TestHandlerRecoveredFailure(t *testing.T) {
	tracker := NewTracker()
	tracker.Fail(ConcernDockerSnapshot, "list failed")
	tracker.Recover(ConcernDockerSnapshot)

	result, recorder := serve(t, tracker)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !result.Healthy || result.Status != "ok" {
		t.Fatalf("response health = %#v, want recovered", result)
	}
	if got, want := result.Message, "All active conditions recovered"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if got := result.Errors; len(got) != 1 || got[0].RecoveryPeriod != "0s" || got[0].TimeUntilRecovered != "0s" {
		t.Fatalf("errors = %#v, want one recovered record", got)
	}
}

func serve(t *testing.T, tracker *Tracker) (response, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	NewHandler(tracker).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	var result response
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return result, recorder
}
