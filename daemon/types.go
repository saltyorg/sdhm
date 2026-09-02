package daemon

import (
	"context"
	"net/netip"
	"time"
)

const (
	dockerOperationTimeout = 10 * time.Second
	retryInitialDelay      = 1 * time.Second
	retryMaxDelay          = 30 * time.Second
	streamStabilityDelay   = 30 * time.Second
	shutdownTimeout        = 5 * time.Second
)

// Endpoint is a container address and its aliases on one Docker network.
type Endpoint struct {
	Network string
	IP      netip.Addr
	Aliases []string
}

// Event describes a container's network connection or disconnection.
type Event struct {
	Action      string
	ContainerID string
	Network     string
}

// NetworkSource provides Docker network state and change notifications.
type NetworkSource interface {
	Ping(context.Context) error
	Snapshot(context.Context, []string) ([]Endpoint, error)
	Events(context.Context, []string) (<-chan Event, <-chan error)
	Close() error
}

// PrepareResult describes an observed outcome while preparing the hosts file.
type PrepareResult struct {
	RecoveredFromBackup bool
}

// HostStore prepares and updates the managed hosts file.
type HostStore interface {
	Prepare(context.Context) (PrepareResult, error)
	Apply(context.Context, []Endpoint) error
}

// HealthServer serves the daemon health endpoint and owns its listener.
type HealthServer interface {
	Start() error
	Done() <-chan struct{}
	Err() error
	Shutdown(context.Context) error
}

// Config contains the validated daemon runtime configuration.
type Config struct {
	Networks         []string
	DefaultNetwork   string
	PeriodicInterval time.Duration
	DebounceDelay    time.Duration
	MaxDebounceDelay time.Duration
}

// timingConfig contains internal timing controls used by daemon tasks.
// Keeping these values private lets package tests use short durations without
// expanding the public configuration surface.
type timingConfig struct {
	dockerOperationTimeout time.Duration
	retryInitialDelay      time.Duration
	retryMaxDelay          time.Duration
	streamStabilityDelay   time.Duration
	shutdownTimeout        time.Duration
}

func defaultTiming() timingConfig {
	return timingConfig{
		dockerOperationTimeout: dockerOperationTimeout,
		retryInitialDelay:      retryInitialDelay,
		retryMaxDelay:          retryMaxDelay,
		streamStabilityDelay:   streamStabilityDelay,
		shutdownTimeout:        shutdownTimeout,
	}
}
