package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/saltyorg/sdhm/health"
)

const eventStreamClosedMessage = "Docker event stream closed"

func (d *Daemon) loop(
	ctx context.Context,
	initialReconcileErr error,
) (error, bool) {
	periodicTimer := time.NewTimer(d.config.PeriodicInterval)
	var debounceTimer *time.Timer
	var maximumTimer *time.Timer
	var retryTimer *time.Timer
	var reconnectTimer *time.Timer
	var stabilityTimer *time.Timer
	retryDelay := d.timing.retryInitialDelay
	reconnectDelay := d.timing.retryInitialDelay
	pending := false
	lastReconcileFailure := ""
	if initialReconcileErr != nil {
		lastReconcileFailure = initialReconcileErr.Error()
		retryTimer = resetLoopTimer(retryTimer, retryDelay)
		retryDelay = nextLoopBackoff(retryDelay, d.timing.retryMaxDelay)
	}

	var events <-chan Event
	var eventErrors <-chan error
	var streamCancel context.CancelFunc
	cancelStream := func() {
		if streamCancel == nil {
			return
		}
		streamCancel()
		streamCancel = nil
	}
	startStream := func() {
		cancelStream()
		streamCtx, childCancel := context.WithCancel(ctx)
		streamCancel = childCancel
		events, eventErrors = d.source.Events(streamCtx, slices.Clone(d.config.Networks))
		stabilityTimer = resetLoopTimer(stabilityTimer, d.timing.streamStabilityDelay)
	}
	disconnectStream := func(err error) {
		cancelStream()
		events = nil
		eventErrors = nil
		stabilityTimer = stopLoopTimer(stabilityTimer)
		message := eventStreamClosedMessage
		if err != nil {
			message = err.Error()
		}
		d.tracker.Fail(health.ConcernDockerEvents, message)
		reconnectTimer = resetLoopTimer(reconnectTimer, reconnectDelay)
		reconnectDelay = nextLoopBackoff(reconnectDelay, d.timing.retryMaxDelay)
	}
	streamErrorReady := func() (error, bool) {
		select {
		case err, ok := <-eventErrors:
			if !ok {
				return nil, true
			}
			return err, true
		default:
			return nil, false
		}
	}
	observeEvent := func() {
		reconnectDelay = d.timing.retryInitialDelay
		stabilityTimer = stopLoopTimer(stabilityTimer)
		d.tracker.Recover(health.ConcernDockerEvents)
		if debounceTimer == nil {
			maximumTimer = resetLoopTimer(maximumTimer, d.config.MaxDebounceDelay)
		}
		debounceTimer = resetLoopTimer(debounceTimer, d.config.DebounceDelay)
	}
	startStream()
	if events == nil && eventErrors == nil {
		disconnectStream(nil)
	}

	defer func() {
		cancelStream()
		periodicTimer = stopLoopTimer(periodicTimer)
		debounceTimer = stopLoopTimer(debounceTimer)
		maximumTimer = stopLoopTimer(maximumTimer)
		retryTimer = stopLoopTimer(retryTimer)
		reconnectTimer = stopLoopTimer(reconnectTimer)
		stabilityTimer = stopLoopTimer(stabilityTimer)
	}()

	for {
		if runErr, captured, stop := d.loopStop(ctx); stop {
			return runErr, captured
		}
		if pending {
			pending = false
			periodicTimer = stopLoopTimer(periodicTimer)
			debounceTimer = stopLoopTimer(debounceTimer)
			maximumTimer = stopLoopTimer(maximumTimer)
			retryTimer = stopLoopTimer(retryTimer)
			reconcileErr := d.reconcile(ctx)
			if ctx.Err() == nil {
				periodicTimer = resetLoopTimer(periodicTimer, d.config.PeriodicInterval)
			}
			if reconcileErr != nil {
				if ctx.Err() == nil {
					if reconcileErr.Error() != lastReconcileFailure {
						d.logger.Warn("reconciliation failed", "err", reconcileErr, "retry_in", retryDelay)
						lastReconcileFailure = reconcileErr.Error()
					}
					retryTimer = resetLoopTimer(retryTimer, retryDelay)
					retryDelay = nextLoopBackoff(retryDelay, d.timing.retryMaxDelay)
				}
				continue
			}
			if ctx.Err() == nil {
				retryDelay = d.timing.retryInitialDelay
				if lastReconcileFailure != "" {
					d.logger.Info("reconciliation recovered")
					lastReconcileFailure = ""
				}
			}
			continue
		}

		select {
		case <-ctx.Done():
			return nil, false
		case <-d.server.Done():
			serveErr := d.server.Err()
			if serveErr != nil {
				return fmt.Errorf("health server stopped: %w", serveErr), true
			}
			return errHealthServerStopped, true
		case _, ok := <-events:
			if !ok {
				streamErr, _ := streamErrorReady()
				disconnectStream(streamErr)
				continue
			}
			observeEvent()
		case err, ok := <-eventErrors:
			if !ok {
				disconnectStream(nil)
				continue
			}
			disconnectStream(err)
		case <-loopTimerChannel(periodicTimer):
			periodicTimer = stopLoopTimer(periodicTimer)
			debounceTimer = stopLoopTimer(debounceTimer)
			maximumTimer = stopLoopTimer(maximumTimer)
			pending = true
		case <-loopTimerChannel(debounceTimer):
			debounceTimer = stopLoopTimer(debounceTimer)
			maximumTimer = stopLoopTimer(maximumTimer)
			pending = true
		case <-loopTimerChannel(maximumTimer):
			debounceTimer = stopLoopTimer(debounceTimer)
			maximumTimer = stopLoopTimer(maximumTimer)
			pending = true
		case <-loopTimerChannel(retryTimer):
			retryTimer = stopLoopTimer(retryTimer)
			pending = true
		case <-loopTimerChannel(reconnectTimer):
			reconnectTimer = stopLoopTimer(reconnectTimer)
			startStream()
			if events == nil && eventErrors == nil {
				disconnectStream(nil)
			}
		case <-loopTimerChannel(stabilityTimer):
			stabilityTimer = stopLoopTimer(stabilityTimer)
			if streamErr, ready := streamErrorReady(); ready {
				disconnectStream(streamErr)
				continue
			}
			select {
			case _, ok := <-events:
				if !ok {
					streamErr, _ := streamErrorReady()
					disconnectStream(streamErr)
					continue
				}
				observeEvent()
				continue
			default:
			}
			reconnectDelay = d.timing.retryInitialDelay
			d.tracker.Recover(health.ConcernDockerEvents)
		}
	}
}

func (d *Daemon) loopStop(ctx context.Context) (error, bool, bool) {
	select {
	case <-d.server.Done():
		serveErr := d.server.Err()
		if serveErr != nil {
			return fmt.Errorf("health server stopped: %w", serveErr), true, true
		}
		return errHealthServerStopped, true, true
	default:
	}

	select {
	case <-ctx.Done():
		return nil, false, true
	default:
		return nil, false, false
	}
}

func nextLoopBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum-current {
		return maximum
	}
	return current * 2
}

func loopTimerChannel(timer *time.Timer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.C
}

func resetLoopTimer(timer *time.Timer, delay time.Duration) *time.Timer {
	if timer == nil {
		return time.NewTimer(delay)
	}
	stopAndDrainLoopTimer(timer)
	timer.Reset(delay)
	return timer
}

func stopLoopTimer(timer *time.Timer) *time.Timer {
	if timer != nil {
		stopAndDrainLoopTimer(timer)
	}
	return nil
}

func stopAndDrainLoopTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}
