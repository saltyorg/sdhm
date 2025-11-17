package docker

import (
	"context"
	"fmt"
	"net"
	"slices"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

const (
	// DockerShortIDLength is the standard length for short container IDs
	DockerShortIDLength = 12
)

// ContainerInfo holds information about a Docker container
type ContainerInfo struct {
	ID        string
	Name      string
	IP        net.IP
	Hostnames []string
}

// DockerClient wraps the Docker SDK client
type DockerClient struct {
	cli      *client.Client
	networks []string
	logFunc  func(string, ...any) // Logger function for warnings
}

// NewDockerClient creates a new Docker client
// logFunc is optional (can be nil) and is used for logging warnings
func NewDockerClient(networks []string, logFunc func(string, ...any)) (*DockerClient, error) {
	cli, err := client.New(
		client.WithHost(client.DefaultDockerHost),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &DockerClient{
		cli:      cli,
		networks: networks,
		logFunc:  logFunc,
	}, nil
}

// Close closes the Docker client
func (d *DockerClient) Close() error {
	return d.cli.Close()
}

// Ping checks if the Docker daemon is accessible
func (d *DockerClient) Ping(ctx context.Context) error {
	_, err := d.cli.Ping(ctx, client.PingOptions{})
	if err != nil {
		return fmt.Errorf("failed to ping Docker daemon: %w", err)
	}
	return nil
}

// ListContainersOnNetwork lists all containers on the configured networks
func (d *DockerClient) ListContainersOnNetwork(ctx context.Context) ([]ContainerInfo, error) {
	// List all running containers
	listResult, err := d.cli.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	var result []ContainerInfo

	for _, c := range listResult.Items {
		// Inspect container to get network details
		inspectResult, err := d.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			// Log error but continue with other containers
			if d.logFunc != nil {
				shortID := c.ID
				if len(shortID) > DockerShortIDLength {
					shortID = shortID[:DockerShortIDLength]
				}
				d.logFunc("WARN: Failed to inspect container %s: %v", shortID, err)
			}
			continue
		}

		inspect := inspectResult.Container

		// Check each configured network
		for _, network := range d.networks {
			// Check if container is on this network
			networkSettings, ok := inspect.NetworkSettings.Networks[network]
			if !ok {
				continue
			}

			// Skip if no IP address
			if !networkSettings.IPAddress.IsValid() {
				continue
			}

			// Convert netip.Addr to net.IP
			ip := net.IP(networkSettings.IPAddress.AsSlice())

			// Get aliases (hostnames)
			aliases := networkSettings.Aliases
			if len(aliases) == 0 {
				continue
			}

			// Filter out empty aliases and create hostnames with domain suffix
			var hostnames []string
			for _, alias := range aliases {
				if alias != "" {
					hostnames = append(hostnames, alias)
					hostnames = append(hostnames, alias+"."+network)
				}
			}

			if len(hostnames) == 0 {
				continue
			}

			// Get container name (remove leading /)
			name := inspect.Name
			if len(name) > 0 && name[0] == '/' {
				name = name[1:]
			}

			// Use short ID for display
			shortID := c.ID
			if len(shortID) > DockerShortIDLength {
				shortID = shortID[:DockerShortIDLength]
			}

			result = append(result, ContainerInfo{
				ID:        shortID,
				Name:      name,
				IP:        ip,
				Hostnames: hostnames,
			})

			// Only add each container once (even if on multiple monitored networks)
			break
		}
	}

	return result, nil
}

// GetContainerName looks up a container name by ID
func (d *DockerClient) GetContainerName(ctx context.Context, containerID string) string {
	inspectResult, err := d.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		// Return short ID if inspection fails
		if len(containerID) > DockerShortIDLength {
			return containerID[:DockerShortIDLength]
		}
		return containerID
	}

	// Get container name (remove leading /)
	name := inspectResult.Container.Name
	if len(name) > 0 && name[0] == '/' {
		name = name[1:]
	}

	if name == "" {
		// Fall back to short ID
		if len(containerID) > DockerShortIDLength {
			return containerID[:DockerShortIDLength]
		}
		return containerID
	}

	return name
}

// IsMonitoredNetwork checks if a network name is in the list of monitored networks
func (d *DockerClient) IsMonitoredNetwork(networkName string) bool {
	return slices.Contains(d.networks, networkName)
}

// MonitorEvents monitors Docker network events and sends them to the channel
func (d *DockerClient) MonitorEvents(ctx context.Context) (<-chan events.Message, <-chan error) {
	// Create filters for network events
	// We only monitor network events because they always fire when network connectivity changes.
	// Container lifecycle events (start/stop/die/destroy) are redundant - network events
	// fire first and provide complete coverage of all network connectivity changes.
	eventFilters := client.Filters{}.
		Add("event", "connect").   // Container joins a network
		Add("event", "disconnect") // Container leaves a network

	result := d.cli.Events(ctx, client.EventsListOptions{
		Filters: eventFilters,
	})

	return result.Messages, result.Err
}
