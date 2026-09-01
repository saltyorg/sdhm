package health

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

const unknownRecovery = "unknown"

type responseRecord struct {
	Timestamp          time.Time `json:"timestamp"`
	Message            string    `json:"message"`
	ErrorType          string    `json:"error_type"`
	Severity           Severity  `json:"severity"`
	RecoveryPeriod     string    `json:"recovery_period"`
	TimeUntilRecovered string    `json:"time_until_recovered"`
}

type healthResponse struct {
	Healthy          bool             `json:"healthy"`
	Status           string           `json:"status"`
	Message          string           `json:"message"`
	ErrorCount       int              `json:"error_count"`
	Errors           []responseRecord `json:"errors"`
	Reason           string           `json:"reason,omitempty"`
	TimeUntilHealthy string           `json:"time_until_healthy,omitempty"`
}

// NewHandler returns an HTTP handler for the tracker's current health snapshot.
func NewHandler(tracker *Tracker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		snapshot := tracker.Snapshot()
		response := renderSnapshot(snapshot)

		w.Header().Set("Content-Type", "application/json")
		if !response.Healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			// Headers may already be committed, so no fallback response is safe.
			return
		}
	})
}

func renderSnapshot(snapshot Snapshot) healthResponse {
	activeIDs := make(map[uint64]struct{}, len(snapshot.Active))
	for _, id := range snapshot.Active {
		activeIDs[id] = struct{}{}
	}

	response := healthResponse{
		Healthy:    snapshot.Ready && len(activeIDs) == 0,
		ErrorCount: len(snapshot.History),
		Errors:     make([]responseRecord, 0, len(snapshot.History)),
	}
	var newestActive *Record
	for i := range snapshot.History {
		record := snapshot.History[i]
		_, active := activeIDs[record.id]
		recovery := "0s"
		if active {
			recovery = unknownRecovery
			if newestActive == nil || record.id > newestActive.id {
				newestActive = &record
			}
		}
		response.Errors = append(response.Errors, responseRecord{
			Timestamp:          record.Timestamp,
			Message:            record.Message,
			ErrorType:          record.ErrorType,
			Severity:           record.Severity,
			RecoveryPeriod:     recovery,
			TimeUntilRecovered: recovery,
		})
	}

	if !snapshot.Ready {
		response.Status = "degraded"
		response.Message = "System initializing"
		response.Reason = "initialization has not completed"
		response.TimeUntilHealthy = unknownRecovery
		return response
	}

	if response.Healthy {
		response.Status = "ok"
		response.Message = "No errors recorded"
		if response.ErrorCount > 0 {
			response.Message = "All active conditions recovered"
		}
		return response
	}

	response.Status = "degraded"
	response.Message = "System degraded: " + strconv.Itoa(len(activeIDs)) + " active condition(s)"
	response.Reason = newestActive.Message
	response.TimeUntilHealthy = unknownRecovery
	return response
}
