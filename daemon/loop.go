package daemon

import (
	"context"
	"fmt"
)

const eventStreamClosedMessage = "Docker event stream closed"

func (d *Daemon) loop(
	ctx context.Context,
	initialReconcileErr error,
) (error, bool) {
	state := newLoopState(d, ctx, initialReconcileErr)
	state.startStream()
	if state.events == nil && state.eventErrors == nil {
		state.disconnectStream(nil)
	}
	defer state.close()

	for {
		if stop, captured, runErr := d.loopStop(ctx); stop {
			return runErr, captured
		}
		if state.pending {
			state.reconcilePending()
			continue
		}

		select {
		case <-ctx.Done():
			return nil, false
		case <-d.server.Done():
			return d.loopServerResult()
		case _, ok := <-state.events:
			state.handleEvent(ok)
		case err, ok := <-state.eventErrors:
			state.handleStreamError(err, ok)
		case <-loopTimerChannel(state.periodicTimer):
			state.periodicTimer = stopLoopTimer(state.periodicTimer)
			state.debounceTimer = stopLoopTimer(state.debounceTimer)
			state.maximumTimer = stopLoopTimer(state.maximumTimer)
			state.pending = true
		case <-loopTimerChannel(state.debounceTimer):
			state.debounceTimer = stopLoopTimer(state.debounceTimer)
			state.maximumTimer = stopLoopTimer(state.maximumTimer)
			state.pending = true
		case <-loopTimerChannel(state.maximumTimer):
			state.debounceTimer = stopLoopTimer(state.debounceTimer)
			state.maximumTimer = stopLoopTimer(state.maximumTimer)
			state.pending = true
		case <-loopTimerChannel(state.retryTimer):
			state.retryTimer = stopLoopTimer(state.retryTimer)
			state.pending = true
		case <-loopTimerChannel(state.reconnectTimer):
			state.reconnectTimer = stopLoopTimer(state.reconnectTimer)
			if state.stopReady() {
				continue
			}
			state.startStream()
			if state.events == nil && state.eventErrors == nil {
				state.disconnectStream(nil)
			}
		case <-loopTimerChannel(state.stabilityTimer):
			state.handleStability()
		}
	}
}

func (d *Daemon) loopServerResult() (error, bool) {
	if serveErr := d.server.Err(); serveErr != nil {
		return fmt.Errorf("health server stopped: %w", serveErr), true
	}
	return errHealthServerStopped, true
}

func loopStreamStopReady(ctx context.Context, serverDone <-chan struct{}) bool {
	select {
	case <-serverDone:
		return true
	default:
	}

	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func (d *Daemon) loopStop(ctx context.Context) (bool, bool, error) {
	select {
	case <-d.server.Done():
		serveErr := d.server.Err()
		if serveErr != nil {
			return true, true, fmt.Errorf("health server stopped: %w", serveErr)
		}
		return true, true, errHealthServerStopped
	default:
	}

	select {
	case <-ctx.Done():
		return true, false, nil
	default:
		return false, false, nil
	}
}
