// Package docker adapts the Moby client to the daemon's network source.
package docker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/events"
	mobyclient "github.com/moby/moby/client"

	"github.com/saltyorg/sdhm/daemon"
)

var _ daemon.NetworkSource = (*Client)(nil)

type apiClient interface {
	Ping(context.Context, mobyclient.PingOptions) (mobyclient.PingResult, error)
	ContainerList(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error)
	ContainerInspect(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error)
	Events(context.Context, mobyclient.EventsListOptions) mobyclient.EventsResult
	Close() error
}

// Client provides authoritative Docker network snapshots and change events.
type Client struct {
	api     apiClient
	streams sync.WaitGroup
}

// New constructs a client for the default Docker host with API negotiation.
func New() (*Client, error) {
	api, err := mobyclient.New(
		mobyclient.WithHost(mobyclient.DefaultDockerHost),
	)
	if err != nil {
		return nil, fmt.Errorf("creating Docker client: %w", err)
	}
	return newClient(api), nil
}

func newClient(api apiClient) *Client {
	return &Client{api: api}
}

// Ping verifies that the Docker daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.api.Ping(ctx, mobyclient.PingOptions{}); err != nil {
		return fmt.Errorf("pinging Docker daemon: %w", err)
	}
	return nil
}

// Snapshot returns all valid endpoints on the configured Docker networks.
func (c *Client) Snapshot(ctx context.Context, networks []string) ([]daemon.Endpoint, error) {
	if err := requireNetworks(networks); err != nil {
		return nil, err
	}

	filters := mobyclient.Filters{}.Add("network", networks...)
	listed, err := c.api.ContainerList(ctx, mobyclient.ContainerListOptions{
		All:     false,
		Filters: filters,
	})
	if err != nil {
		return nil, fmt.Errorf("listing Docker containers: %w", err)
	}

	endpoints := make([]daemon.Endpoint, 0, len(listed.Items))
	for _, item := range listed.Items {
		inspected, err := c.api.ContainerInspect(ctx, item.ID, mobyclient.ContainerInspectOptions{})
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("inspecting Docker container %q: %w", item.ID, err)
		}
		if inspected.Container.NetworkSettings == nil {
			continue
		}

		for _, networkName := range networks {
			settings, ok := inspected.Container.NetworkSettings.Networks[networkName]
			if !ok {
				continue
			}
			if settings == nil {
				continue
			}

			ip := settings.IPAddress
			if !ip.Is4() {
				ip = settings.GlobalIPv6Address
				if !ip.Is6() {
					continue
				}
			}

			aliases := normalizeAliases(settings.Aliases)
			if len(aliases) == 0 {
				continue
			}

			endpoints = append(endpoints, daemon.Endpoint{
				Network: networkName,
				IP:      ip,
				Aliases: aliases,
			})
		}
	}

	networkOrder := make(map[string]int, len(networks))
	for i, networkName := range networks {
		if _, exists := networkOrder[networkName]; !exists {
			networkOrder[networkName] = i
		}
	}
	slices.SortFunc(endpoints, func(a, b daemon.Endpoint) int {
		if order := networkOrder[a.Network] - networkOrder[b.Network]; order != 0 {
			return order
		}
		if address := a.IP.Compare(b.IP); address != 0 {
			return address
		}
		return slices.Compare(a.Aliases, b.Aliases)
	})

	return endpoints, nil
}

// Events returns mapped Docker network connect and disconnect events.
func (c *Client) Events(ctx context.Context, networks []string) (<-chan daemon.Event, <-chan error) {
	if err := requireNetworks(networks); err != nil {
		return failedEventStream(err)
	}

	filters := mobyclient.Filters{}.
		Add("type", "network").
		Add("event", "connect", "disconnect").
		Add("network", networks...)
	source := c.api.Events(ctx, mobyclient.EventsListOptions{Filters: filters})
	mappedEvents := make(chan daemon.Event, 1)
	mappedErrors := make(chan error, 1)

	c.streams.Go(func() {
		defer close(mappedEvents)
		defer close(mappedErrors)

		forwardError := func(err error) {
			if err == nil {
				return
			}
			select {
			case mappedErrors <- err:
			case <-ctx.Done():
			}
		}
		sourceErrorReady := func() (error, bool) {
			select {
			case err, ok := <-source.Err:
				if !ok {
					return nil, true
				}
				return err, true
			default:
				return nil, false
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if err, ready := sourceErrorReady(); ready {
				forwardError(err)
				return
			}

			select {
			case <-ctx.Done():
				return
			case event, ok := <-source.Messages:
				if !ok {
					if err, ready := sourceErrorReady(); ready {
						forwardError(err)
					}
					return
				}
				select {
				case mappedEvents <- mapEvent(event):
				case <-ctx.Done():
					return
				}
			case err, ok := <-source.Err:
				if !ok {
					return
				}
				forwardError(err)
				return
			}
		}
	})

	return mappedEvents, mappedErrors
}

// Close closes the Moby client and waits for every mapped stream to finish.
// Callers must cancel all stream contexts before calling Close.
func (c *Client) Close() error {
	err := c.api.Close()
	c.streams.Wait()
	if err != nil {
		return fmt.Errorf("closing Docker client: %w", err)
	}
	return nil
}

func requireNetworks(networks []string) error {
	if len(networks) == 0 {
		return errors.New("at least one Docker network is required")
	}
	return nil
}

func normalizeAliases(aliases []string) []string {
	seen := make(map[string]struct{}, len(aliases))
	normalized := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if alias == "" {
			continue
		}
		if _, exists := seen[alias]; exists {
			continue
		}
		seen[alias] = struct{}{}
		normalized = append(normalized, alias)
	}
	slices.Sort(normalized)
	return normalized
}

func mapEvent(event events.Message) daemon.Event {
	return daemon.Event{
		Action:      string(event.Action),
		ContainerID: event.Actor.Attributes["container"],
		Network:     event.Actor.Attributes["name"],
	}
}

func failedEventStream(err error) (<-chan daemon.Event, <-chan error) {
	events := make(chan daemon.Event)
	errs := make(chan error, 1)
	errs <- err
	close(events)
	close(errs)
	return events, errs
}
