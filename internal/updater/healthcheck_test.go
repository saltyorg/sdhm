package updater

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthCheck_RecordError(t *testing.T) {
	hc := NewHealthCheck()

	// Record an error
	hc.RecordError("docker", "test error message")

	errors := hc.GetAllErrors()
	if len(errors) != 1 {
		t.Fatalf("Expected 1 error, got %d", len(errors))
	}

	if errors[0].ErrorType != "docker" {
		t.Errorf("Expected error type 'docker', got '%s'", errors[0].ErrorType)
	}

	if errors[0].Message != "test error message" {
		t.Errorf("Expected message 'test error message', got '%s'", errors[0].Message)
	}

	// Check that severity was auto-assigned
	if errors[0].Severity != SeverityMajor {
		t.Errorf("Expected severity 'major' for docker error, got '%s'", errors[0].Severity)
	}
}

func TestHealthCheck_SeverityAssignment(t *testing.T) {
	tests := []struct {
		errorType        string
		expectedSeverity Severity
	}{
		{"validation", SeverityCritical},
		{"backup", SeverityCritical},
		{"filesystem", SeverityCritical},
		{"docker", SeverityMajor},
		{"update", SeverityMajor},
		{"docker_events", SeverityMinor},
		{"network", SeverityMinor},
		{"healthcheck", SeverityMinor},
		{"unknown", SeverityMajor}, // Default
	}

	for _, tt := range tests {
		t.Run(tt.errorType, func(t *testing.T) {
			hc := NewHealthCheck()
			hc.RecordError(tt.errorType, "test")

			errors := hc.GetAllErrors()
			if len(errors) != 1 {
				t.Fatalf("Expected 1 error, got %d", len(errors))
			}

			if errors[0].Severity != tt.expectedSeverity {
				t.Errorf("Expected severity '%s', got '%s'", tt.expectedSeverity, errors[0].Severity)
			}
		})
	}
}

func TestHealthCheck_MaxErrors(t *testing.T) {
	hc := NewHealthCheck()

	// Record more errors than max (10)
	for range 15 {
		hc.RecordError("docker_events", "error message")
	}

	errors := hc.GetAllErrors()
	if len(errors) != 10 {
		t.Errorf("Expected max 10 stored errors, got %d", len(errors))
	}
}

func TestHealthCheck_IsHealthy(t *testing.T) {
	hc := NewHealthCheck()

	// Should be healthy initially
	if !hc.IsHealthy() {
		t.Error("Expected healthy state initially")
	}

	// Record a minor error (5m recovery)
	hc.RecordError("docker_events", "event stream error")

	// Should be unhealthy immediately
	if hc.IsHealthy() {
		t.Error("Expected unhealthy state after error")
	}
}

func TestHealthCheck_TieredRecovery(t *testing.T) {
	// Test minor error recovery (5m)
	t.Run("minor error recovery", func(t *testing.T) {
		hc := NewHealthCheck()
		hc.RecordError("docker_events", "minor error")

		// Manually set timestamp to 6 minutes ago
		hc.mu.Lock()
		hc.errors[0].Timestamp = time.Now().Add(-6 * time.Minute)
		hc.mu.Unlock()

		if !hc.IsHealthy() {
			t.Error("Expected healthy after 6 minutes for minor error")
		}
	})

	// Test major error recovery (1h)
	t.Run("major error recovery", func(t *testing.T) {
		hc := NewHealthCheck()
		hc.RecordError("docker", "major error")

		// Set timestamp to 30 minutes ago - should still be unhealthy
		hc.mu.Lock()
		hc.errors[0].Timestamp = time.Now().Add(-30 * time.Minute)
		hc.mu.Unlock()

		if hc.IsHealthy() {
			t.Error("Expected unhealthy after 30 minutes for major error")
		}

		// Set timestamp to 65 minutes ago - should be healthy
		hc.mu.Lock()
		hc.errors[0].Timestamp = time.Now().Add(-65 * time.Minute)
		hc.mu.Unlock()

		if !hc.IsHealthy() {
			t.Error("Expected healthy after 65 minutes for major error")
		}
	})

	// Test critical error recovery (24h)
	t.Run("critical error recovery", func(t *testing.T) {
		hc := NewHealthCheck()
		hc.RecordError("validation", "critical error")

		// Set timestamp to 12 hours ago - should still be unhealthy
		hc.mu.Lock()
		hc.errors[0].Timestamp = time.Now().Add(-12 * time.Hour)
		hc.mu.Unlock()

		if hc.IsHealthy() {
			t.Error("Expected unhealthy after 12 hours for critical error")
		}

		// Set timestamp to 25 hours ago - should be healthy
		hc.mu.Lock()
		hc.errors[0].Timestamp = time.Now().Add(-25 * time.Hour)
		hc.mu.Unlock()

		if !hc.IsHealthy() {
			t.Error("Expected healthy after 25 hours for critical error")
		}
	})
}

func TestHealthCheck_ServeHTTP_Healthy(t *testing.T) {
	hc := NewHealthCheck()

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()

	hc.ServeHTTP(rec, req)

	// Check status code
	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	// Check content type
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Expected Content-Type application/json, got %s", ct)
	}

	// Parse JSON response
	var response map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	// Check healthy field
	healthy, ok := response["healthy"].(bool)
	if !ok {
		t.Fatal("healthy field not found or not boolean")
	}
	if !healthy {
		t.Error("Expected healthy=true")
	}

	// Check status field
	status, ok := response["status"].(string)
	if !ok {
		t.Fatal("status field not found or not string")
	}
	if status != "ok" {
		t.Errorf("Expected status='ok', got '%s'", status)
	}

	// Check error_count
	errorCount, ok := response["error_count"].(float64)
	if !ok {
		t.Fatal("error_count field not found or not number")
	}
	if errorCount != 0 {
		t.Errorf("Expected error_count=0, got %v", errorCount)
	}
}

func TestHealthCheck_ServeHTTP_Unhealthy(t *testing.T) {
	hc := NewHealthCheck()

	// Record some errors with different severities
	hc.RecordError("docker", "connection failed")   // major
	hc.RecordError("validation", "file invalid")    // critical
	hc.RecordError("docker_events", "stream error") // minor

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()

	hc.ServeHTTP(rec, req)

	// Check status code
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", rec.Code)
	}

	// Parse JSON response
	var response map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	// Check healthy field
	healthy, ok := response["healthy"].(bool)
	if !ok {
		t.Fatal("healthy field not found or not boolean")
	}
	if healthy {
		t.Error("Expected healthy=false")
	}

	// Check status field
	status, ok := response["status"].(string)
	if !ok {
		t.Fatal("status field not found or not string")
	}
	if status != "degraded" {
		t.Errorf("Expected status='degraded', got '%s'", status)
	}

	// Check error_count
	errorCount, ok := response["error_count"].(float64)
	if !ok {
		t.Fatal("error_count field not found or not number")
	}
	if errorCount != 3 {
		t.Errorf("Expected error_count=3, got %v", errorCount)
	}

	// Check time_until_healthy field exists
	if _, ok := response["time_until_healthy"].(string); !ok {
		t.Error("Expected time_until_healthy field in degraded response")
	}

	// Check reason field exists
	if _, ok := response["reason"].(string); !ok {
		t.Error("Expected reason field in degraded response")
	}

	// Check errors array
	errors, ok := response["errors"].([]any)
	if !ok {
		t.Fatal("errors field not found or not array")
	}
	if len(errors) != 3 {
		t.Errorf("Expected 3 errors in array, got %d", len(errors))
	}

	// Check first error has recovery information
	firstError, ok := errors[0].(map[string]any)
	if !ok {
		t.Fatal("First error is not an object")
	}

	if _, ok := firstError["recovery_period"].(string); !ok {
		t.Error("Expected recovery_period in error object")
	}

	if _, ok := firstError["time_until_recovered"].(string); !ok {
		t.Error("Expected time_until_recovered in error object")
	}

	if _, ok := firstError["severity"].(string); !ok {
		t.Error("Expected severity in error object")
	}
}

func TestHealthCheck_ServeHTTP_RecoveredErrors(t *testing.T) {
	hc := NewHealthCheck()

	// Record an error and set its timestamp to way in the past
	hc.RecordError("docker_events", "old error")
	hc.mu.Lock()
	hc.errors[0].Timestamp = time.Now().Add(-10 * time.Minute) // 10 minutes ago (past 5m recovery)
	hc.mu.Unlock()

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()

	hc.ServeHTTP(rec, req)

	// Should be healthy now
	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200 for recovered errors, got %d", rec.Code)
	}

	var response map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	healthy, ok := response["healthy"].(bool)
	if !ok || !healthy {
		t.Error("Expected healthy=true for recovered errors")
	}

	// Should still show the error in the list, but with 0 time_until_recovered
	errorCount, _ := response["error_count"].(float64)
	if errorCount != 1 {
		t.Errorf("Expected 1 error in history, got %v", errorCount)
	}
}
