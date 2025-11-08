package debounce

import (
	"sync"
	"testing"
	"time"
)

func TestDebouncer_SingleTrigger(t *testing.T) {
	var callCount int
	var mu sync.Mutex

	callback := func() {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	debouncer := NewDebouncer(50*time.Millisecond, 200*time.Millisecond, callback)

	// Trigger once
	debouncer.Trigger()

	// Wait for callback to execute
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	count := callCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("Expected 1 callback execution, got %d", count)
	}
}

func TestDebouncer_MultipleTriggers(t *testing.T) {
	var callCount int
	var mu sync.Mutex

	callback := func() {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	debouncer := NewDebouncer(50*time.Millisecond, 500*time.Millisecond, callback)

	// Trigger multiple times rapidly
	for range 5 {
		debouncer.Trigger()
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for callback to execute
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	count := callCount
	mu.Unlock()

	// Should only execute once due to debouncing
	if count != 1 {
		t.Errorf("Expected 1 callback execution after rapid triggers, got %d", count)
	}
}

func TestDebouncer_MaxDelay(t *testing.T) {
	var callCount int
	var mu sync.Mutex

	callback := func() {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	// Short max delay to test forcing
	debouncer := NewDebouncer(50*time.Millisecond, 150*time.Millisecond, callback)

	// Start time
	start := time.Now()

	// Continuously trigger to prevent normal debounce from executing
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				debouncer.Trigger()
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	// Wait for max delay to force execution
	time.Sleep(200 * time.Millisecond)
	close(done)

	elapsed := time.Since(start)

	mu.Lock()
	count := callCount
	mu.Unlock()

	// Should have executed at least once due to max delay
	if count < 1 {
		t.Errorf("Expected at least 1 callback execution due to max delay, got %d", count)
	}

	// Should have executed around the max delay time
	if elapsed < 150*time.Millisecond {
		t.Errorf("Callback executed too early: %v (expected ~150ms)", elapsed)
	}
}

func TestDebouncer_Stop(t *testing.T) {
	var callCount int
	var mu sync.Mutex

	callback := func() {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	debouncer := NewDebouncer(50*time.Millisecond, 200*time.Millisecond, callback)

	// Trigger then immediately stop
	debouncer.Trigger()
	debouncer.Stop()

	// Wait to see if callback executes (it shouldn't)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	count := callCount
	mu.Unlock()

	if count != 0 {
		t.Errorf("Expected 0 callback executions after Stop(), got %d", count)
	}
}

func TestDebouncer_ResetWindow(t *testing.T) {
	var executionTimes []time.Time
	var mu sync.Mutex

	callback := func() {
		mu.Lock()
		executionTimes = append(executionTimes, time.Now())
		mu.Unlock()
	}

	debouncer := NewDebouncer(100*time.Millisecond, 500*time.Millisecond, callback)

	// First trigger - should execute after delay
	debouncer.Trigger()
	time.Sleep(150 * time.Millisecond)

	// Second trigger - should start new window
	debouncer.Trigger()
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	count := len(executionTimes)
	mu.Unlock()

	// Should have executed twice, once for each window
	if count != 2 {
		t.Errorf("Expected 2 callback executions in separate windows, got %d", count)
	}
}
