// Package health tracks the daemon's current health concerns and diagnostic history.
package health

import (
	"maps"
	"sync"
	"time"
)

const maxHistoryRecords = 10

// Concern identifies a daemon operation whose health is tracked independently.
type Concern string

const (
	ConcernDockerSnapshot Concern = "docker_snapshot"
	ConcernDockerEvents   Concern = "docker_events"
	ConcernHostsApply     Concern = "hosts_apply"
	ConcernRecovery       Concern = "recovery"
)

// Severity describes the impact of a health concern.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
)

type concernDetails struct {
	errorType string
	severity  Severity
}

var concernDetailsByConcern = map[Concern]concernDetails{
	ConcernDockerSnapshot: {errorType: "docker", severity: SeverityMajor},
	ConcernDockerEvents:   {errorType: "docker_events", severity: SeverityMinor},
	ConcernHostsApply:     {errorType: "update", severity: SeverityMajor},
	ConcernRecovery:       {errorType: "validation", severity: SeverityCritical},
}

// Record is one failed health observation retained for diagnosis.
type Record struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
	ErrorType string    `json:"error_type"`
	Severity  Severity  `json:"severity"`

	id      uint64
	concern Concern
}

// Snapshot is an immutable copy of the tracker's current state.
type Snapshot struct {
	Active  map[Concern]uint64
	History []Record
}

// Tracker stores current concerns and a bounded diagnostic history.
type Tracker struct {
	mu      sync.RWMutex
	active  map[Concern]uint64
	history []Record
	nextID  uint64
	max     int
	now     func() time.Time
}

// NewTracker creates a tracker with the standard history limit.
func NewTracker() *Tracker {
	return newTracker(maxHistoryRecords, time.Now)
}

func newTracker(max int, now func() time.Time) *Tracker {
	return &Tracker{
		active: make(map[Concern]uint64),
		max:    max,
		now:    now,
	}
}

// Fail replaces concern's active record and retains the failed observation.
func (t *Tracker) Fail(concern Concern, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	details := concernDetailsByConcern[concern]
	t.nextID++
	record := Record{
		Timestamp: t.now(),
		Message:   message,
		ErrorType: details.errorType,
		Severity:  details.severity,
		id:        t.nextID,
		concern:   concern,
	}
	t.history = append(t.history, record)
	t.active[concern] = record.id
	t.evictHistory()
}

// Recover removes concern from the current active conditions while keeping history.
func (t *Tracker) Recover(concern Concern) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.active, concern)
}

// Snapshot returns a stable copy of the current health state.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return Snapshot{
		Active:  maps.Clone(t.active),
		History: append([]Record(nil), t.history...),
	}
}

func (t *Tracker) evictHistory() {
	for len(t.history) > t.max {
		index := -1
		for i, record := range t.history {
			if !t.isActive(record.id) {
				index = i
				break
			}
		}
		if index == -1 {
			return
		}
		t.history = append(t.history[:index], t.history[index+1:]...)
	}
}

func (t *Tracker) isActive(id uint64) bool {
	for _, activeID := range t.active {
		if activeID == id {
			return true
		}
	}
	return false
}
