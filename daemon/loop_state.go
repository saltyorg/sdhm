package daemon

import (
	"context"
	"slices"
	"time"

	"github.com/saltyorg/sdhm/health"
)

type loopState struct {
	daemon *Daemon
	ctx    context.Context

	periodicTimer  *time.Timer
	debounceTimer  *time.Timer
	maximumTimer   *time.Timer
	retryTimer     *time.Timer
	reconnectTimer *time.Timer
	stabilityTimer *time.Timer

	retryDelay           time.Duration
	reconnectDelay       time.Duration
	pending              bool
	lastReconcileFailure string
	lastStreamFailure    string
	events               <-chan Event
	eventErrors          <-chan error
	streamCancel         context.CancelFunc
}

func newLoopState(daemon *Daemon, ctx context.Context, initialReconcileErr error) *loopState {
	state := &loopState{
		daemon:         daemon,
		ctx:            ctx,
		periodicTimer:  time.NewTimer(daemon.config.PeriodicInterval),
		retryDelay:     daemon.timing.retryInitialDelay,
		reconnectDelay: daemon.timing.retryInitialDelay,
	}
	if initialReconcileErr != nil {
		state.lastReconcileFailure = initialReconcileErr.Error()
		state.retryTimer = resetLoopTimer(state.retryTimer, state.retryDelay)
		state.retryDelay = nextLoopBackoff(state.retryDelay, daemon.timing.retryMaxDelay)
	}
	return state
}

func (s *loopState) close() {
	s.cancelStream()
	s.periodicTimer = stopLoopTimer(s.periodicTimer)
	s.debounceTimer = stopLoopTimer(s.debounceTimer)
	s.maximumTimer = stopLoopTimer(s.maximumTimer)
	s.retryTimer = stopLoopTimer(s.retryTimer)
	s.reconnectTimer = stopLoopTimer(s.reconnectTimer)
	s.stabilityTimer = stopLoopTimer(s.stabilityTimer)
}

func (s *loopState) stopReady() bool {
	return loopStreamStopReady(s.ctx, s.daemon.server.Done())
}

func (s *loopState) cancelStream() {
	if s.streamCancel == nil {
		return
	}
	s.streamCancel()
	s.streamCancel = nil
}

func (s *loopState) startStream() {
	if s.stopReady() {
		return
	}
	s.cancelStream()
	streamCtx, cancel := context.WithCancel(s.ctx)
	s.streamCancel = cancel
	s.events, s.eventErrors = s.daemon.source.Events(streamCtx, slices.Clone(s.daemon.config.Networks))
	if s.stopReady() {
		s.cancelStream()
		s.events = nil
		s.eventErrors = nil
		return
	}
	s.stabilityTimer = resetLoopTimer(s.stabilityTimer, s.daemon.timing.streamStabilityDelay)
}

func (s *loopState) recoverStream(evidence string) {
	if s.stopReady() {
		return
	}
	s.reconnectDelay = s.daemon.timing.retryInitialDelay
	s.stabilityTimer = stopLoopTimer(s.stabilityTimer)
	s.daemon.tracker.Recover(health.ConcernDockerEvents)
	if s.lastStreamFailure != "" {
		s.daemon.logger.Info("Docker event stream recovered", "evidence", evidence)
		s.lastStreamFailure = ""
	}
}

func (s *loopState) disconnectStream(err error) {
	s.cancelStream()
	s.events = nil
	s.eventErrors = nil
	s.stabilityTimer = stopLoopTimer(s.stabilityTimer)
	if s.stopReady() {
		return
	}
	message := eventStreamClosedMessage
	if err != nil {
		message = err.Error()
	}
	if message != s.lastStreamFailure {
		s.daemon.logger.Warn("Docker event stream unavailable", "err", message, "retry_in", s.reconnectDelay)
		s.lastStreamFailure = message
	}
	s.daemon.tracker.Fail(health.ConcernDockerEvents, message)
	s.reconnectTimer = resetLoopTimer(s.reconnectTimer, s.reconnectDelay)
	s.reconnectDelay = nextLoopBackoff(s.reconnectDelay, s.daemon.timing.retryMaxDelay)
}

func (s *loopState) streamErrorReady() (error, bool) {
	select {
	case err, ok := <-s.eventErrors:
		if !ok {
			return nil, true
		}
		return err, true
	default:
		return nil, false
	}
}

func (s *loopState) observeEvent() {
	if s.stopReady() {
		return
	}
	s.recoverStream("event")
	if s.debounceTimer == nil {
		s.maximumTimer = resetLoopTimer(s.maximumTimer, s.daemon.config.MaxDebounceDelay)
	}
	s.debounceTimer = resetLoopTimer(s.debounceTimer, s.daemon.config.DebounceDelay)
}

func (s *loopState) handleEvent(open bool) {
	if !open {
		streamErr, _ := s.streamErrorReady()
		s.disconnectStream(streamErr)
		return
	}
	s.observeEvent()
}

func (s *loopState) handleStreamError(err error, open bool) {
	if !open {
		s.disconnectStream(nil)
		return
	}
	s.disconnectStream(err)
}

func (s *loopState) reconcilePending() {
	s.pending = false
	s.periodicTimer = stopLoopTimer(s.periodicTimer)
	s.debounceTimer = stopLoopTimer(s.debounceTimer)
	s.maximumTimer = stopLoopTimer(s.maximumTimer)
	s.retryTimer = stopLoopTimer(s.retryTimer)
	reconcileErr := s.daemon.reconcile(s.ctx)
	if s.stopReady() {
		return
	}
	if s.ctx.Err() == nil {
		s.periodicTimer = resetLoopTimer(s.periodicTimer, s.daemon.config.PeriodicInterval)
	}
	if reconcileErr != nil {
		if s.ctx.Err() == nil {
			if reconcileErr.Error() != s.lastReconcileFailure {
				s.daemon.logger.Warn("reconciliation failed", "err", reconcileErr, "retry_in", s.retryDelay)
				s.lastReconcileFailure = reconcileErr.Error()
			}
			s.retryTimer = resetLoopTimer(s.retryTimer, s.retryDelay)
			s.retryDelay = nextLoopBackoff(s.retryDelay, s.daemon.timing.retryMaxDelay)
		}
		return
	}
	if s.ctx.Err() == nil {
		s.retryDelay = s.daemon.timing.retryInitialDelay
		if s.lastReconcileFailure != "" {
			s.daemon.logger.Info("reconciliation recovered")
			s.lastReconcileFailure = ""
		}
	}
}

func (s *loopState) handleStability() {
	s.stabilityTimer = stopLoopTimer(s.stabilityTimer)
	if s.stopReady() {
		return
	}
	if streamErr, ready := s.streamErrorReady(); ready {
		s.disconnectStream(streamErr)
		return
	}
	select {
	case _, ok := <-s.events:
		if !ok {
			streamErr, _ := s.streamErrorReady()
			s.disconnectStream(streamErr)
			return
		}
		s.observeEvent()
		return
	default:
	}
	s.recoverStream("stable")
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
