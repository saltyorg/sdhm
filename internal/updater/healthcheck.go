package updater

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	// MaxHealthCheckErrors is the maximum number of errors to keep in the health check history
	MaxHealthCheckErrors = 10
)

// Severity levels for errors
type Severity string

const (
	SeverityCritical Severity = "critical" // 24h recovery
	SeverityMajor    Severity = "major"    // 1h recovery
	SeverityMinor    Severity = "minor"    // 5m recovery
)

// Recovery periods for each severity level
const (
	RecoveryCritical = 24 * time.Hour
	RecoveryMajor    = 1 * time.Hour
	RecoveryMinor    = 5 * time.Minute
)

// severityMap maps error types to their severity levels
var severityMap = map[string]Severity{
	// Critical errors (24h recovery)
	"validation": SeverityCritical,
	"backup":     SeverityCritical,
	"filesystem": SeverityCritical,
	// Major errors (1h recovery)
	"docker": SeverityMajor,
	"update": SeverityMajor,
	// Minor errors (5m recovery)
	"docker_events": SeverityMinor,
	"network":       SeverityMinor,
	"healthcheck":   SeverityMinor,
	"sync_check":    SeverityMinor,
}

// ErrorRecord represents a single error event
type ErrorRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
	ErrorType string    `json:"error_type"`
	Severity  Severity  `json:"severity"`
}

// getSeverity returns the severity level for a given error type
func getSeverity(errorType string) Severity {
	if severity, ok := severityMap[errorType]; ok {
		return severity
	}
	// Unknown error types default to major
	return SeverityMajor
}

// getRecoveryPeriod returns the recovery period for a severity level
func getRecoveryPeriod(severity Severity) time.Duration {
	switch severity {
	case SeverityCritical:
		return RecoveryCritical
	case SeverityMajor:
		return RecoveryMajor
	case SeverityMinor:
		return RecoveryMinor
	default:
		return RecoveryMajor
	}
}

// HealthCheck tracks errors and provides health check endpoint
type HealthCheck struct {
	errors    []ErrorRecord
	mu        sync.RWMutex
	maxErrors int
}

// NewHealthCheck creates a new HealthCheck instance
func NewHealthCheck() *HealthCheck {
	return &HealthCheck{
		errors:    make([]ErrorRecord, 0),
		maxErrors: MaxHealthCheckErrors,
	}
}

// RecordError records an error event with auto-assigned severity
func (h *HealthCheck) RecordError(errType, message string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	record := ErrorRecord{
		Timestamp: time.Now(),
		Message:   message,
		ErrorType: errType,
		Severity:  getSeverity(errType),
	}

	h.errors = append(h.errors, record)

	// Keep only the last N errors (circular buffer)
	if len(h.errors) > h.maxErrors {
		h.errors = h.errors[len(h.errors)-h.maxErrors:]
	}
}

// GetAllErrors returns all stored errors
func (h *HealthCheck) GetAllErrors() []ErrorRecord {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Return a copy to avoid race conditions
	result := make([]ErrorRecord, len(h.errors))
	copy(result, h.errors)
	return result
}

// IsHealthy returns true if all errors have exceeded their recovery period
func (h *HealthCheck) IsHealthy() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.errors) == 0 {
		return true
	}

	now := time.Now()
	for _, err := range h.errors {
		recoveryPeriod := getRecoveryPeriod(err.Severity)
		if now.Sub(err.Timestamp) < recoveryPeriod {
			// This error is still within its recovery period
			return false
		}
	}

	return true
}

// ErrorWithRecovery adds recovery information to an error for the response
type ErrorWithRecovery struct {
	ErrorRecord
	RecoveryPeriod     string `json:"recovery_period"`
	TimeUntilRecovered string `json:"time_until_recovered"`
}

// ServeHTTP implements http.Handler interface
func (h *HealthCheck) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")

	now := time.Now()

	// Build error list with recovery information
	errorsWithRecovery := make([]ErrorWithRecovery, 0, len(h.errors))
	var maxTimeUntilHealthy time.Duration
	var criticalError *ErrorRecord

	for i := range h.errors {
		err := &h.errors[i]
		recoveryPeriod := getRecoveryPeriod(err.Severity)
		timeSinceError := now.Sub(err.Timestamp)
		timeUntilRecovered := max(recoveryPeriod-timeSinceError, 0)

		errorsWithRecovery = append(errorsWithRecovery, ErrorWithRecovery{
			ErrorRecord:        *err,
			RecoveryPeriod:     recoveryPeriod.String(),
			TimeUntilRecovered: timeUntilRecovered.String(),
		})

		// Track the longest recovery time remaining
		if timeUntilRecovered > maxTimeUntilHealthy {
			maxTimeUntilHealthy = timeUntilRecovered
			criticalError = err
		}
	}

	healthy := maxTimeUntilHealthy == 0

	response := map[string]any{
		"healthy":     healthy,
		"error_count": len(h.errors),
		"errors":      errorsWithRecovery,
	}

	if healthy {
		w.WriteHeader(http.StatusOK)
		response["status"] = "ok"
		if len(h.errors) == 0 {
			response["message"] = "No errors recorded"
		} else {
			response["message"] = "All errors have exceeded their recovery period"
		}
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		response["status"] = "degraded"
		response["time_until_healthy"] = maxTimeUntilHealthy.String()
		response["reason"] = fmt.Sprintf("%s error still within %s recovery period",
			criticalError.Severity, getRecoveryPeriod(criticalError.Severity))
		response["message"] = fmt.Sprintf("System degraded: %d error(s), healthy in %s",
			len(h.errors), maxTimeUntilHealthy.String())
	}

	json.NewEncoder(w).Encode(response)
}
