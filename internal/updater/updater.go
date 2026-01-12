package updater

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/saltyorg/sdhm/internal/config"
	"github.com/saltyorg/sdhm/internal/debounce"
	"github.com/saltyorg/sdhm/internal/docker"
	"github.com/saltyorg/sdhm/internal/hosts"
	"github.com/saltyorg/sdhm/internal/logger"
)

const (
	// DockerOperationTimeout is the timeout for Docker API operations
	DockerOperationTimeout = 10 * time.Second
	// EventStreamInitialBackoff is the initial delay before reconnecting to Docker event stream
	EventStreamInitialBackoff = 1 * time.Second
	// EventStreamMaxBackoff is the maximum delay before reconnecting to Docker event stream
	EventStreamMaxBackoff = 30 * time.Second
	// EventStreamBackoffMultiplier is the multiplier for exponential backoff
	EventStreamBackoffMultiplier = 2
)

// Updater coordinates the hosts file updates
type Updater struct {
	dockerClient *docker.DockerClient
	hostsManager *hosts.HostsFileManager
	debouncer    *debounce.Debouncer
	config       *config.Config
	healthCheck  *HealthCheck
	shutdownCh   chan struct{}
	shutdownOnce sync.Once
	wg           sync.WaitGroup
	logger       *logger.Logger
	updateMutex  sync.Mutex // Serializes all update operations
	updateCtxMu  sync.RWMutex
	updateCtx    context.Context
	resetTimerCh chan struct{} // Signals periodic timer to reset
}

// NewUpdater creates a new Updater instance
func NewUpdater(cfg *config.Config, log *logger.Logger) (*Updater, error) {
	dockerClient, err := docker.NewDockerClient(cfg.DockerNetworks, log.LogFunc())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	hostsManager := hosts.NewHostsFileManager(
		cfg.HostsFile,
		cfg.BackupFile,
		cfg.ManagedSectionName,
	)

	healthCheck := NewHealthCheck()

	updater := &Updater{
		dockerClient: dockerClient,
		hostsManager: hostsManager,
		config:       cfg,
		healthCheck:  healthCheck,
		shutdownCh:   make(chan struct{}),
		logger:       log,
		updateCtx:    context.Background(),
		resetTimerCh: make(chan struct{}, 1), // Buffered to prevent blocking
	}

	// Create debouncer with callback
	updater.debouncer = debounce.NewDebouncer(
		cfg.DebounceDelay,
		cfg.MaxDebounceDelay,
		func() {
			// Lock to prevent concurrent updates
			updater.updateMutex.Lock()
			updateCtx := updater.getUpdateContext()
			err := updater.updateHostsFile(updateCtx)
			updater.updateMutex.Unlock()

			if err != nil {
				log.Error("Failed to update hosts file: %v", err)
			} else {
				// Reset periodic timer after successful event-driven update
				select {
				case updater.resetTimerCh <- struct{}{}:
				default: // Don't block if channel is full
				}
			}
		},
	)

	return updater, nil
}

// Start starts the updater
func (u *Updater) Start(ctx context.Context) error {
	// Check Docker connectivity
	if err := u.dockerClient.Ping(ctx); err != nil {
		return fmt.Errorf("docker daemon not accessible: %w", err)
	}

	// Fix non-breaking spaces
	if err := u.hostsManager.FixNonBreakingSpaces(ctx); err != nil {
		u.logger.Warn("Failed to fix non-breaking spaces: %v", err)
	}

	// Ensure managed section markers are valid (adds them if missing, errors if corrupt)
	if err := u.hostsManager.EnsureMarkersValid(ctx); err != nil {
		return fmt.Errorf("hosts file markers invalid: %w (run 'sdhm regenerate' to fix)", err)
	}

	// Do initial update
	if err := u.updateHostsFile(ctx); err != nil {
		u.logger.Warn("Initial update failed: %v", err)
	}

	// Create cancellable context
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	u.setUpdateContext(ctx)

	// Start periodic updater goroutine
	u.wg.Add(1)
	go u.periodicUpdater(ctx)

	// Start Docker event monitor goroutine
	u.wg.Add(1)
	go u.eventMonitor(ctx)

	// Start health check HTTP server goroutine
	u.wg.Add(1)
	go u.healthCheckServer(ctx)

	u.logger.Info("Updater started (periodic validation: %s)", u.config.PeriodicInterval)

	// Wait for shutdown signal or context cancellation
	select {
	case <-u.shutdownCh:
	case <-ctx.Done():
	}

	// Graceful shutdown
	u.logger.Info("Shutting down...")
	cancel()
	u.debouncer.Stop()
	u.wg.Wait()

	return nil
}

// Shutdown signals the updater to stop
func (u *Updater) Shutdown() {
	u.shutdownOnce.Do(func() {
		close(u.shutdownCh)
	})
}

func (u *Updater) getUpdateContext() context.Context {
	u.updateCtxMu.RLock()
	defer u.updateCtxMu.RUnlock()
	if u.updateCtx == nil {
		return context.Background()
	}
	return u.updateCtx
}

func (u *Updater) setUpdateContext(ctx context.Context) {
	u.updateCtxMu.Lock()
	u.updateCtx = ctx
	u.updateCtxMu.Unlock()
}

// Close closes resources
func (u *Updater) Close() error {
	return u.dockerClient.Close()
}

// hostnamesMatch checks if two hostname slices are equivalent (order-independent)
func hostnamesMatch(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	aMap := make(map[string]bool)
	for _, hostname := range a {
		aMap[hostname] = true
	}

	for _, hostname := range b {
		if !aMap[hostname] {
			return false
		}
	}

	return true
}

func findHostnameIPChange(hostnames []string, lookup map[string]string, currentIP string) (string, string, bool) {
	for _, hostname := range hostnames {
		if hostname == "" {
			continue
		}
		if ip, ok := lookup[hostname]; ok && ip != currentIP {
			return ip, hostname, true
		}
	}
	return "", "", false
}

// validateHostsFile checks if hosts file matches current Docker state
func (u *Updater) validateHostsFile(ctx context.Context) (bool, string) {
	// Create a context with timeout for Docker operations
	dockerCtx, cancel := context.WithTimeout(ctx, DockerOperationTimeout)
	defer cancel()

	// Get current Docker state
	containers, err := u.dockerClient.ListContainersOnNetwork(dockerCtx)
	if err != nil {
		return false, fmt.Sprintf("failed to list containers: %v", err)
	}

	// Get current hosts file state
	currentEntries, err := u.hostsManager.GetManagedSectionEntries(ctx)
	if err != nil {
		return false, fmt.Sprintf("failed to read hosts file: %v", err)
	}

	// Build maps keyed by IP address for comparison
	dockerMap := make(map[string][]string)        // IP -> hostnames
	dockerHostnameToIP := make(map[string]string) // hostname -> IP (for detecting IP changes)
	for _, c := range containers {
		ip := c.IP.String()
		dockerMap[ip] = c.Hostnames
		for _, hostname := range c.Hostnames {
			if hostname != "" {
				dockerHostnameToIP[hostname] = ip
			}
		}
	}

	hostsMap := make(map[string][]string)
	hostsHostnameToIP := make(map[string]string)
	for _, entry := range currentEntries {
		ip := entry.IP.String()
		hostsMap[ip] = entry.Hostnames
		for _, hostname := range entry.Hostnames {
			if hostname != "" {
				hostsHostnameToIP[hostname] = ip
			}
		}
	}

	// Check for missing containers (in Docker but not in hosts file)
	for ip, dockerHostnames := range dockerMap {
		hostsHostnames, exists := hostsMap[ip]
		if !exists {
			containerName := ip
			if len(dockerHostnames) > 0 && dockerHostnames[0] != "" {
				containerName = dockerHostnames[0]
			}
			// Check if this container exists with a different IP
			if oldIP, hostname, hasOldIP := findHostnameIPChange(dockerHostnames, hostsHostnameToIP, ip); hasOldIP {
				if hostname == "" {
					hostname = containerName
				}
				return false, fmt.Sprintf("container '%s' IP changed from %s to %s", hostname, oldIP, ip)
			}
			return false, fmt.Sprintf("container '%s' with IP %s missing from hosts file", containerName, ip)
		}
		if !hostnamesMatch(dockerHostnames, hostsHostnames) {
			containerName := ip
			if len(dockerHostnames) > 0 && dockerHostnames[0] != "" {
				containerName = dockerHostnames[0]
			}
			return false, fmt.Sprintf("hostname mismatch for container '%s' (IP %s): expected %v, found %v",
				containerName, ip, dockerHostnames, hostsHostnames)
		}
	}

	// Check for stale entries (in hosts file but not in Docker)
	for ip, hostsHostnames := range hostsMap {
		if _, exists := dockerMap[ip]; !exists {
			staleHostname := ip
			if len(hostsHostnames) > 0 && hostsHostnames[0] != "" {
				staleHostname = hostsHostnames[0]
			}
			// Check if this hostname exists with a different IP in Docker
			if newIP, hostname, hasNewIP := findHostnameIPChange(hostsHostnames, dockerHostnameToIP, ip); hasNewIP {
				if hostname == "" {
					hostname = staleHostname
				}
				// This case is already covered by the IP change check above
				// But handle it here for completeness
				return false, fmt.Sprintf("container '%s' IP changed from %s to %s", hostname, ip, newIP)
			}
			return false, fmt.Sprintf("stale entry '%s' with IP %s in hosts file (container no longer exists)", staleHostname, ip)
		}
	}

	return true, ""
}

// updateHostsFile performs the actual hosts file update
func (u *Updater) updateHostsFile(ctx context.Context) error {
	// Create a context with timeout for Docker operations
	dockerCtx, cancel := context.WithTimeout(ctx, DockerOperationTimeout)
	defer cancel()

	// Get containers on network
	containers, err := u.dockerClient.ListContainersOnNetwork(dockerCtx)
	if err != nil {
		u.healthCheck.RecordError("docker", fmt.Sprintf("failed to list containers: %v", err))
		return fmt.Errorf("failed to list containers: %w", err)
	}

	// Convert to host entries
	var entries []hosts.HostEntry
	for _, c := range containers {
		entries = append(entries, hosts.HostEntry{
			IP:        c.IP,
			Hostnames: c.Hostnames,
		})
	}

	// Update managed section
	if err := u.hostsManager.UpdateManagedSection(ctx, entries); err != nil {
		u.healthCheck.RecordError("update", fmt.Sprintf("failed to update hosts: %v", err))
		return fmt.Errorf("failed to update managed section: %w", err)
	}

	if len(entries) > 0 {
		u.logger.Info("Hosts file updated successfully (%d container entries)", len(entries))
	} else {
		u.logger.Info("Hosts file updated successfully (no containers on network)")
	}

	return nil
}

// periodicUpdater runs periodic validation with timer reset support
func (u *Updater) periodicUpdater(ctx context.Context) {
	defer u.wg.Done()

	timer := time.NewTimer(u.config.PeriodicInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			u.logger.Info("Periodic updater stopped")
			return

		case <-u.resetTimerCh:
			// Reset timer after event-driven update
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(u.config.PeriodicInterval)

		case <-timer.C:
			// Lock to prevent concurrent updates
			u.updateMutex.Lock()

			// Validate current state instead of unconditionally updating
			inSync, details := u.validateHostsFile(ctx)
			if !inSync {
				u.logger.Warn("Hosts file out of sync: %s", details)
				u.healthCheck.RecordError("sync_check", details)
				if err := u.updateHostsFile(ctx); err != nil {
					u.logger.Error("Periodic update failed: %v", err)
				}
			}
			// Silent when in sync - no log message needed

			u.updateMutex.Unlock()

			// Reset timer for next cycle
			timer.Reset(u.config.PeriodicInterval)
		}
	}
}

// eventMonitor monitors Docker events with automatic reconnection
func (u *Updater) eventMonitor(ctx context.Context) {
	defer u.wg.Done()

	u.logger.Info("Monitoring Docker network events (connect, disconnect)")

	backoff := EventStreamInitialBackoff
	var eventCh <-chan events.Message
	var errCh <-chan error
	reconnect := func() bool {
		u.logger.Info("Reconnecting to Docker event stream in %v", backoff)

		select {
		case <-time.After(backoff):
			// Increase backoff for next potential failure
			backoff *= EventStreamBackoffMultiplier
			if backoff > EventStreamMaxBackoff {
				backoff = EventStreamMaxBackoff
			}
			return true
		case <-ctx.Done():
			u.logger.Info("Event monitor stopped during reconnection backoff")
			return false
		}
	}

	for {
		// Establish or re-establish connection to Docker event stream
		if eventCh == nil {
			u.logger.Info("Connecting to Docker event stream")
			eventCh, errCh = u.dockerClient.MonitorEvents(ctx)
			// Reset backoff on successful connection
			backoff = EventStreamInitialBackoff
		}

		select {
		case <-ctx.Done():
			u.logger.Info("Event monitor stopped")
			return

		case err, ok := <-errCh:
			if !ok {
				u.logger.Warn("Docker event stream closed")
				eventCh = nil
				errCh = nil
				if !reconnect() {
					return
				}
				continue
			}
			if err != nil {
				u.healthCheck.RecordError("docker_events", fmt.Sprintf("event stream error: %v", err))
				u.logger.Error("Docker event stream error: %v", err)

				// Mark channels as dead so we reconnect on next iteration
				eventCh = nil
				errCh = nil

				// Exponential backoff before reconnecting
				if !reconnect() {
					return
				}
			}

		case event, ok := <-eventCh:
			if !ok {
				u.logger.Warn("Docker event stream closed")
				eventCh = nil
				errCh = nil
				if !reconnect() {
					return
				}
				continue
			}
			// All events are network events (connect/disconnect)
			// The "container" attribute contains the container ID
			// The "name" attribute contains the network name
			containerID := event.Actor.Attributes["container"]
			networkName := event.Actor.Attributes["name"]

			// Check if this is one of our monitored networks
			if !u.dockerClient.IsMonitoredNetwork(networkName) {
				// Log and ignore events from other networks
				containerName := u.dockerClient.GetContainerName(ctx, containerID)
				u.logger.Info("Docker event: network %s on '%s' (container: %s) - not monitoring this network",
					event.Action, networkName, containerName)
				continue
			}

			// Look up the container name by ID
			containerName := u.dockerClient.GetContainerName(ctx, containerID)

			// Log event with container and network information
			u.logger.Info("Docker event: network %s on '%s' (container: %s)",
				event.Action, networkName, containerName)
			u.debouncer.Trigger()
		}
	}
}

// healthCheckServer runs the health check HTTP server
func (u *Updater) healthCheckServer(ctx context.Context) {
	defer u.wg.Done()

	mux := http.NewServeMux()
	mux.Handle("/health", u.healthCheck)

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", u.config.HealthCheckAddr, u.config.HealthCheckPort),
		Handler: mux,
	}

	go func() {
		u.logger.Info("Health check server started on %s:%d", u.config.HealthCheckAddr, u.config.HealthCheckPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			u.logger.Error("Health check server error: %v", err)
			u.healthCheck.RecordError("healthcheck", fmt.Sprintf("server error: %v", err))
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		u.logger.Error("Health check server shutdown error: %v", err)
	}
}
