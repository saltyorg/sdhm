package updater

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/saltyorg/sdhm/internal/config"
	"github.com/saltyorg/sdhm/internal/debounce"
	"github.com/saltyorg/sdhm/internal/docker"
	"github.com/saltyorg/sdhm/internal/hosts"
)

const (
	// DockerOperationTimeout is the timeout for Docker API operations
	DockerOperationTimeout = 10 * time.Second
	// EventStreamReconnectDelay is the delay before reconnecting to Docker event stream after an error
	EventStreamReconnectDelay = 5 * time.Second
)

// Updater coordinates the hosts file updates
type Updater struct {
	dockerClient *docker.DockerClient
	hostsManager *hosts.HostsFileManager
	debouncer    *debounce.Debouncer
	config       *config.Config
	healthCheck  *HealthCheck
	shutdownCh   chan struct{}
	wg           sync.WaitGroup
	logger       *log.Logger
	updateMutex  sync.Mutex    // Serializes all update operations
	resetTimerCh chan struct{} // Signals periodic timer to reset
}

// NewUpdater creates a new Updater instance
func NewUpdater(cfg *config.Config, logger *log.Logger) (*Updater, error) {
	dockerClient, err := docker.NewDockerClient(cfg.DockerNetworks, logger.Printf)
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
		logger:       logger,
		resetTimerCh: make(chan struct{}, 1), // Buffered to prevent blocking
	}

	// Create debouncer with callback
	updater.debouncer = debounce.NewDebouncer(
		cfg.DebounceDelay,
		cfg.MaxDebounceDelay,
		func() {
			// Lock to prevent concurrent updates
			updater.updateMutex.Lock()
			err := updater.updateHostsFile(context.Background())
			updater.updateMutex.Unlock()

			if err != nil {
				logger.Printf("ERROR: Failed to update hosts file: %v", err)
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
		u.logger.Printf("WARN: Failed to fix non-breaking spaces: %v", err)
	}

	// Validate and recover hosts file if corrupted
	if err := u.hostsManager.ValidateHostsFile(ctx, u.config.HostsFile); err != nil {
		u.logger.Printf("WARN: Hosts file validation failed: %v", err)
		if err := u.hostsManager.RecoverHostsFile(ctx, u.logger.Printf); err != nil {
			return fmt.Errorf("failed to recover hosts file: %w", err)
		}
	}

	// Ensure managed section exists
	if err := u.hostsManager.EnsureManagedSectionExists(ctx); err != nil {
		return fmt.Errorf("failed to ensure managed section: %w", err)
	}

	// Do initial update
	if err := u.updateHostsFile(ctx); err != nil {
		u.logger.Printf("WARN: Initial update failed: %v", err)
	}

	// Create cancellable context
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Start periodic updater goroutine
	u.wg.Add(1)
	go u.periodicUpdater(ctx)

	// Start Docker event monitor goroutine
	u.wg.Add(1)
	go u.eventMonitor(ctx)

	// Start health check HTTP server goroutine
	u.wg.Add(1)
	go u.healthCheckServer(ctx)

	u.logger.Printf("INFO: Updater started (periodic validation: %s)", u.config.PeriodicInterval)

	// Wait for shutdown signal
	<-u.shutdownCh

	// Graceful shutdown
	u.logger.Println("INFO: Shutting down...")
	cancel()
	u.debouncer.Stop()
	u.wg.Wait()

	return nil
}

// Shutdown signals the updater to stop
func (u *Updater) Shutdown() {
	close(u.shutdownCh)
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
		// Use first hostname as primary identifier
		if len(c.Hostnames) > 0 {
			dockerHostnameToIP[c.Hostnames[0]] = ip
		}
	}

	hostsMap := make(map[string][]string)
	hostsHostnameToIP := make(map[string]string)
	for _, entry := range currentEntries {
		ip := entry.IP.String()
		hostsMap[ip] = entry.Hostnames
		if len(entry.Hostnames) > 0 {
			hostsHostnameToIP[entry.Hostnames[0]] = ip
		}
	}

	// Check for missing containers (in Docker but not in hosts file)
	for ip, dockerHostnames := range dockerMap {
		hostsHostnames, exists := hostsMap[ip]
		if !exists {
			containerName := dockerHostnames[0]
			// Check if this container exists with a different IP
			if oldIP, hasOldIP := hostsHostnameToIP[containerName]; hasOldIP {
				return false, fmt.Sprintf("container '%s' IP changed from %s to %s", containerName, oldIP, ip)
			}
			return false, fmt.Sprintf("container '%s' with IP %s missing from hosts file", containerName, ip)
		}
		if !hostnamesMatch(dockerHostnames, hostsHostnames) {
			containerName := dockerHostnames[0]
			return false, fmt.Sprintf("hostname mismatch for container '%s' (IP %s): expected %v, found %v",
				containerName, ip, dockerHostnames, hostsHostnames)
		}
	}

	// Check for stale entries (in hosts file but not in Docker)
	for ip, hostsHostnames := range hostsMap {
		if _, exists := dockerMap[ip]; !exists {
			staleHostname := hostsHostnames[0]
			// Check if this hostname exists with a different IP in Docker
			if newIP, hasNewIP := dockerHostnameToIP[staleHostname]; hasNewIP {
				// This case is already covered by the IP change check above
				// But handle it here for completeness
				return false, fmt.Sprintf("container '%s' IP changed from %s to %s", staleHostname, ip, newIP)
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
		u.logger.Printf("INFO: Hosts file updated successfully (%d container entries)", len(entries))
	} else {
		u.logger.Println("INFO: Hosts file updated successfully (no containers on network)")
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
			u.logger.Println("INFO: Periodic updater stopped")
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
				u.logger.Printf("WARN: Hosts file out of sync: %s", details)
				u.healthCheck.RecordError("sync_check", details)
				if err := u.updateHostsFile(ctx); err != nil {
					u.logger.Printf("ERROR: Periodic update failed: %v", err)
				}
			}
			// Silent when in sync - no log message needed

			u.updateMutex.Unlock()

			// Reset timer for next cycle
			timer.Reset(u.config.PeriodicInterval)
		}
	}
}

// eventMonitor monitors Docker events
func (u *Updater) eventMonitor(ctx context.Context) {
	defer u.wg.Done()

	u.logger.Println("INFO: Monitoring Docker network events (connect, disconnect)")

	eventCh, errCh := u.dockerClient.MonitorEvents(ctx)

	for {
		select {
		case <-ctx.Done():
			u.logger.Println("INFO: Event monitor stopped")
			return
		case err := <-errCh:
			if err != nil {
				u.healthCheck.RecordError("docker_events", fmt.Sprintf("event stream error: %v", err))
				u.logger.Printf("ERROR: Docker event stream error: %v", err)
				// Brief delay before reconnecting
				time.Sleep(EventStreamReconnectDelay)
			}
		case event := <-eventCh:
			// All events are network events (connect/disconnect)
			// The "container" attribute contains the container ID
			// The "name" attribute contains the network name
			containerID := event.Actor.Attributes["container"]
			networkName := event.Actor.Attributes["name"]

			// Check if this is one of our monitored networks
			if !u.dockerClient.IsMonitoredNetwork(networkName) {
				// Log and ignore events from other networks
				containerName := u.dockerClient.GetContainerName(ctx, containerID)
				u.logger.Printf("INFO: Docker event: network %s on '%s' (container: %s) - not monitoring this network",
					event.Action, networkName, containerName)
				continue
			}

			// Look up the container name by ID
			containerName := u.dockerClient.GetContainerName(ctx, containerID)

			// Log event with container and network information
			u.logger.Printf("INFO: Docker event: network %s on '%s' (container: %s)",
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
		u.logger.Printf("INFO: Health check server started on %s:%d", u.config.HealthCheckAddr, u.config.HealthCheckPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			u.logger.Printf("ERROR: Health check server error: %v", err)
			u.healthCheck.RecordError("healthcheck", fmt.Sprintf("server error: %v", err))
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		u.logger.Printf("ERROR: Health check server shutdown error: %v", err)
	}
}
