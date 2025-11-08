package debounce

import (
	"sync"
	"time"
)

// Debouncer handles debouncing of events with a maximum delay
type Debouncer struct {
	delay          time.Duration
	maxDelay       time.Duration
	timer          *time.Timer
	firstEventTime *time.Time
	callback       func()
	mu             sync.Mutex
	stopped        bool // Prevents callbacks after Stop() is called
}

// NewDebouncer creates a new Debouncer
func NewDebouncer(delay, maxDelay time.Duration, callback func()) *Debouncer {
	return &Debouncer{
		delay:    delay,
		maxDelay: maxDelay,
		callback: callback,
	}
}

// Trigger triggers the debouncer
func (d *Debouncer) Trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Don't trigger if stopped
	if d.stopped {
		return
	}

	now := time.Now()

	// Check if we need to force execution due to max delay
	if d.firstEventTime != nil {
		timeSinceFirst := now.Sub(*d.firstEventTime)
		if timeSinceFirst >= d.maxDelay {
			// Max delay reached - execute immediately
			if d.timer != nil {
				d.timer.Stop()
				d.timer = nil
			}
			d.firstEventTime = nil

			// Execute callback in goroutine to avoid holding lock
			go d.callback()
			return
		}
	} else {
		// First event in this window
		d.firstEventTime = &now
	}

	// Reset or create timer
	if d.timer != nil {
		d.timer.Stop()
	}

	d.timer = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		// Check if stopped before executing callback
		if d.stopped {
			d.mu.Unlock()
			return
		}
		d.firstEventTime = nil
		d.timer = nil
		d.mu.Unlock()
		d.callback()
	})
}

// Stop stops the debouncer and cancels any pending callbacks
func (d *Debouncer) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.stopped = true
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.firstEventTime = nil
}
