package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/saltyorg/sdhm/health"
)

var errHealthServerStopped = errors.New("health server stopped unexpectedly")

// Daemon owns startup, reconciliation, and shutdown of the SDHM runtime.
type Daemon struct {
	config  Config
	source  NetworkSource
	store   HostStore
	tracker *health.Tracker
	server  HealthServer
	logger  *slog.Logger
	timing  timingConfig
}

// New validates the daemon's typed configuration and dependencies.
func New(
	cfg Config,
	source NetworkSource,
	store HostStore,
	tracker *health.Tracker,
	server HealthServer,
	logger *slog.Logger,
) (*Daemon, error) {
	if isNil(source) {
		return nil, errors.New("network source is required")
	}
	if isNil(store) {
		return nil, errors.New("host store is required")
	}
	if tracker == nil {
		return nil, errors.New("health tracker is required")
	}
	if isNil(server) {
		return nil, errors.New("health server is required")
	}
	if logger == nil {
		return nil, errors.New("logger is required")
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	cfg.Networks = slices.Clone(cfg.Networks)
	return &Daemon{
		config:  cfg,
		source:  source,
		store:   store,
		tracker: tracker,
		server:  server,
		logger:  logger,
		timing:  defaultTiming(),
	}, nil
}

// Run starts owned resources, waits for a stop condition, and cleans up.
func (d *Daemon) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	serverStarted := false

	if parent.Err() != nil {
		cancel()
		return d.cleanup(serverStarted, nil, false)
	}

	pingCtx, pingCancel := context.WithTimeout(ctx, d.timing.dockerOperationTimeout)
	err := d.source.Ping(pingCtx)
	pingCancel()
	if err != nil {
		var runErr error
		if parent.Err() == nil {
			runErr = fmt.Errorf("ping Docker: %w", err)
		}
		cancel()
		return d.cleanup(serverStarted, runErr, false)
	}
	if parent.Err() != nil {
		cancel()
		return d.cleanup(serverStarted, nil, false)
	}

	if err := d.server.Start(); err != nil {
		var runErr error
		if parent.Err() == nil {
			runErr = fmt.Errorf("start health server: %w", err)
		}
		cancel()
		return d.cleanup(serverStarted, runErr, false)
	}
	serverStarted = true
	if runErr, captured, stop := d.startupStop(parent); stop {
		cancel()
		return d.cleanup(serverStarted, runErr, captured)
	}

	if err := d.store.Prepare(ctx); err != nil {
		var runErr error
		if parent.Err() == nil {
			d.tracker.Fail(health.ConcernRecovery, err.Error())
			runErr = fmt.Errorf("prepare hosts store: %w", err)
		}
		cancel()
		return d.cleanup(serverStarted, runErr, false)
	}
	d.tracker.Recover(health.ConcernRecovery)
	if runErr, captured, stop := d.startupStop(parent); stop {
		cancel()
		return d.cleanup(serverStarted, runErr, captured)
	}

	initialReconcileErr := d.reconcile(ctx)
	if initialReconcileErr != nil && parent.Err() == nil {
		d.logger.Warn("initial reconciliation failed", "err", initialReconcileErr)
	}
	if runErr, captured, stop := d.startupStop(parent); stop {
		cancel()
		return d.cleanup(serverStarted, runErr, captured)
	}

	runErr, serveErrCaptured := d.loop(ctx, cancel, initialReconcileErr != nil)
	cancel()
	return d.cleanup(serverStarted, runErr, serveErrCaptured)
}

func (d *Daemon) reconcile(ctx context.Context) error {
	snapshotCtx, cancel := context.WithTimeout(ctx, d.timing.dockerOperationTimeout)
	endpoints, err := d.source.Snapshot(snapshotCtx, slices.Clone(d.config.Networks))
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			d.tracker.Fail(health.ConcernDockerSnapshot, err.Error())
		}
		return fmt.Errorf("snapshot Docker networks: %w", err)
	}
	d.tracker.Recover(health.ConcernDockerSnapshot)

	if err := d.store.Apply(ctx, endpoints); err != nil {
		if ctx.Err() == nil {
			d.tracker.Fail(health.ConcernHostsApply, err.Error())
		}
		return fmt.Errorf("apply hosts snapshot: %w", err)
	}
	d.tracker.Recover(health.ConcernHostsApply)
	return nil
}

func (d *Daemon) startupStop(parent context.Context) (error, bool, bool) {
	if parent.Err() != nil {
		return nil, false, true
	}
	select {
	case <-d.server.Done():
		serveErr := d.server.Err()
		if serveErr != nil {
			return fmt.Errorf("health server stopped: %w", serveErr), true, true
		}
		return errHealthServerStopped, true, true
	default:
		return nil, false, false
	}
}

func (d *Daemon) cleanup(serverStarted bool, runErr error, serveErrCaptured bool) error {
	var shutdownErr error
	var terminalServeErr error
	if serverStarted {
		completedBeforeShutdown := false
		select {
		case <-d.server.Done():
			completedBeforeShutdown = true
		default:
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), d.timing.shutdownTimeout)
		if err := d.server.Shutdown(shutdownCtx); err != nil {
			shutdownErr = fmt.Errorf("shut down health server: %w", err)
		}
		cancel()
		<-d.server.Done()

		if !serveErrCaptured {
			if err := d.server.Err(); err != nil {
				terminalServeErr = fmt.Errorf("health server stopped: %w", err)
			} else if completedBeforeShutdown {
				terminalServeErr = errHealthServerStopped
			}
		}
	}

	var closeErr error
	if err := d.source.Close(); err != nil {
		closeErr = fmt.Errorf("close Docker source: %w", err)
	}
	return errors.Join(runErr, terminalServeErr, shutdownErr, closeErr)
}

func validateConfig(cfg Config) error {
	if len(cfg.Networks) == 0 {
		return errors.New("at least one network is required")
	}
	seen := make(map[string]struct{}, len(cfg.Networks))
	defaultFound := false
	for _, network := range cfg.Networks {
		if network == "" || strings.TrimSpace(network) != network || strings.IndexFunc(network, invalidNetworkRune) >= 0 {
			return fmt.Errorf("network %q is not normalized", network)
		}
		if _, exists := seen[network]; exists {
			return fmt.Errorf("network %q is duplicated", network)
		}
		seen[network] = struct{}{}
		if network == cfg.DefaultNetwork {
			defaultFound = true
		}
	}
	if !defaultFound {
		return fmt.Errorf("default network %q is not configured", cfg.DefaultNetwork)
	}
	if cfg.PeriodicInterval <= 0 {
		return errors.New("periodic interval must be positive")
	}
	if cfg.DebounceDelay <= 0 {
		return errors.New("debounce delay must be positive")
	}
	if cfg.MaxDebounceDelay <= 0 {
		return errors.New("maximum debounce delay must be positive")
	}
	if cfg.MaxDebounceDelay < cfg.DebounceDelay {
		return errors.New("maximum debounce delay must not be less than debounce delay")
	}
	return nil
}

func invalidNetworkRune(r rune) bool {
	return r == '#' || unicode.IsSpace(r) || unicode.IsControl(r)
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
