package docker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"

	"github.com/saltyorg/sdhm/daemon"
)

type fakeAPI struct {
	pingFn    func(context.Context, mobyclient.PingOptions) (mobyclient.PingResult, error)
	listFn    func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error)
	inspectFn func(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error)
	eventsFn  func(context.Context, mobyclient.EventsListOptions) mobyclient.EventsResult
	closeFn   func() error
}

func (f *fakeAPI) Ping(ctx context.Context, opts mobyclient.PingOptions) (mobyclient.PingResult, error) {
	return f.pingFn(ctx, opts)
}

func (f *fakeAPI) ContainerList(ctx context.Context, opts mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
	return f.listFn(ctx, opts)
}

func (f *fakeAPI) ContainerInspect(ctx context.Context, id string, opts mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
	return f.inspectFn(ctx, id, opts)
}

func (f *fakeAPI) Events(ctx context.Context, opts mobyclient.EventsListOptions) mobyclient.EventsResult {
	return f.eventsFn(ctx, opts)
}

func (f *fakeAPI) Close() error {
	return f.closeFn()
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		pingFn: func(context.Context, mobyclient.PingOptions) (mobyclient.PingResult, error) {
			return mobyclient.PingResult{}, nil
		},
		listFn: func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
			return mobyclient.ContainerListResult{}, nil
		},
		inspectFn: func(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
			return mobyclient.ContainerInspectResult{}, nil
		},
		eventsFn: func(context.Context, mobyclient.EventsListOptions) mobyclient.EventsResult {
			return mobyclient.EventsResult{}
		},
		closeFn: func() error { return nil },
	}
}

func TestPing(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		api := newFakeAPI()
		called := false
		api.pingFn = func(ctx context.Context, opts mobyclient.PingOptions) (mobyclient.PingResult, error) {
			called = true
			if ctx != t.Context() {
				t.Fatal("Ping() did not pass its context to Docker")
			}
			if opts != (mobyclient.PingOptions{}) {
				t.Fatalf("Ping() options = %+v, want zero options", opts)
			}
			return mobyclient.PingResult{}, nil
		}

		if err := newClient(api).Ping(t.Context()); err != nil {
			t.Fatalf("Ping() error = %v", err)
		}
		if !called {
			t.Fatal("Ping() did not call Docker")
		}
	})

	t.Run("error", func(t *testing.T) {
		api := newFakeAPI()
		wantErr := errors.New("ping failed")
		api.pingFn = func(context.Context, mobyclient.PingOptions) (mobyclient.PingResult, error) {
			return mobyclient.PingResult{}, wantErr
		}

		err := newClient(api).Ping(t.Context())
		if !errors.Is(err, wantErr) {
			t.Fatalf("Ping() error = %v, want wrapped %v", err, wantErr)
		}
	})
}

func TestSnapshotCombinesConfiguredNetworks(t *testing.T) {
	api := newFakeAPI()
	listCalls := 0
	inspectIDs := make([]string, 0, 1)
	api.listFn = func(ctx context.Context, opts mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
		listCalls++
		if ctx != t.Context() {
			t.Fatal("Snapshot() did not pass its context to ContainerList")
		}
		if opts.All {
			t.Fatal("ContainerListOptions.All = true, want false")
		}
		assertFilterValue(t, opts.Filters, "network", "saltbox")
		assertFilterValue(t, opts.Filters, "network", "backend")
		assertNoFilterValue(t, opts.Filters, "network", "saltbox,backend")
		return listResult("container-1"), nil
	}
	api.inspectFn = func(ctx context.Context, id string, opts mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
		if ctx != t.Context() {
			t.Fatal("Snapshot() did not pass its context to ContainerInspect")
		}
		if opts != (mobyclient.ContainerInspectOptions{}) {
			t.Fatalf("ContainerInspect() options = %+v, want zero options", opts)
		}
		inspectIDs = append(inspectIDs, id)
		return inspectResult(map[string]*network.EndpointSettings{
			"saltbox": endpoint("172.19.0.2", "", []string{"radarr"}, nil),
			"backend": endpoint("172.20.0.2", "", []string{"radarr"}, nil),
		}), nil
	}

	got, err := newClient(api).Snapshot(t.Context(), []string{"saltbox", "backend"})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	want := []daemon.Endpoint{
		{Network: "saltbox", IP: netip.MustParseAddr("172.19.0.2"), Aliases: []string{"radarr"}},
		{Network: "backend", IP: netip.MustParseAddr("172.20.0.2"), Aliases: []string{"radarr"}},
	}
	assertEndpointsEqual(t, got, want)
	if listCalls != 1 {
		t.Fatalf("ContainerList() calls = %d, want 1", listCalls)
	}
	if !slices.Equal(inspectIDs, []string{"container-1"}) {
		t.Fatalf("ContainerInspect() IDs = %v, want [container-1]", inspectIDs)
	}
}

func TestSnapshotRejectsEmptyNetworksBeforeDockerAccess(t *testing.T) {
	api := newFakeAPI()
	listCalls := 0
	inspectCalls := 0
	api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
		listCalls++
		return mobyclient.ContainerListResult{}, nil
	}
	api.inspectFn = func(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
		inspectCalls++
		return mobyclient.ContainerInspectResult{}, nil
	}

	got, err := newClient(api).Snapshot(t.Context(), nil)
	if err == nil {
		t.Fatal("Snapshot() error = nil, want empty-network error")
	}
	if got != nil {
		t.Fatalf("Snapshot() endpoints = %v, want nil", got)
	}
	if listCalls != 0 || inspectCalls != 0 {
		t.Fatalf("Docker calls = list %d, inspect %d; want none", listCalls, inspectCalls)
	}
}

func TestSnapshotReturnsListError(t *testing.T) {
	api := newFakeAPI()
	wantErr := errors.New("list failed")
	api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
		return mobyclient.ContainerListResult{}, wantErr
	}
	inspectCalls := 0
	api.inspectFn = func(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
		inspectCalls++
		return mobyclient.ContainerInspectResult{}, nil
	}

	got, err := newClient(api).Snapshot(t.Context(), []string{"saltbox"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Snapshot() error = %v, want wrapped %v", err, wantErr)
	}
	if got != nil {
		t.Fatalf("Snapshot() endpoints = %v, want nil", got)
	}
	if inspectCalls != 0 {
		t.Fatalf("ContainerInspect() calls = %d, want 0", inspectCalls)
	}
}

func TestSnapshotSkipsContainerRemovedAfterList(t *testing.T) {
	api := newFakeAPI()
	api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
		return listResult("removed-container"), nil
	}
	api.inspectFn = func(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
		return mobyclient.ContainerInspectResult{}, errdefs.ErrNotFound
	}

	got, err := newClient(api).Snapshot(t.Context(), []string{"saltbox"})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Snapshot() endpoints = %v, want empty", got)
	}
}

func TestSnapshotAbortsOnOtherInspectError(t *testing.T) {
	api := newFakeAPI()
	wantErr := errors.New("inspect failed")
	api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
		return listResult("valid-container", "broken-container"), nil
	}
	api.inspectFn = func(_ context.Context, id string, _ mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
		if id == "broken-container" {
			return mobyclient.ContainerInspectResult{}, wantErr
		}
		return inspectResult(map[string]*network.EndpointSettings{
			"saltbox": endpoint("172.19.0.2", "", []string{"radarr"}, nil),
		}), nil
	}

	got, err := newClient(api).Snapshot(t.Context(), []string{"saltbox"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Snapshot() error = %v, want wrapped %v", err, wantErr)
	}
	if got != nil {
		t.Fatalf("Snapshot() endpoints = %v, want discarded result", got)
	}
}

func TestSnapshotBoundsConcurrentInspects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newFakeAPI()
		ids := make([]string, maxConcurrentInspects*2+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("container-%02d", i)
		}
		api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
			return listResult(ids...), nil
		}

		var active atomic.Int32
		var maximum atomic.Int32
		started := make(chan struct{}, len(ids))
		release := make(chan struct{})
		api.inspectFn = func(ctx context.Context, _ string, _ mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}
			started <- struct{}{}
			select {
			case <-release:
				return mobyclient.ContainerInspectResult{}, nil
			case <-ctx.Done():
				return mobyclient.ContainerInspectResult{}, ctx.Err()
			}
		}

		type snapshotResult struct {
			endpoints []daemon.Endpoint
			err       error
		}
		result := make(chan snapshotResult, 1)
		go func() {
			endpoints, err := newClient(api).Snapshot(t.Context(), []string{"saltbox"})
			result <- snapshotResult{endpoints: endpoints, err: err}
		}()

		synctest.Wait()
		if got := len(started); got != maxConcurrentInspects {
			close(release)
			t.Fatalf("concurrent inspections started = %d, want %d", got, maxConcurrentInspects)
		}
		close(release)
		synctest.Wait()
		got := <-result
		if got.err != nil {
			t.Fatalf("Snapshot() error = %v", got.err)
		}
		if len(got.endpoints) != 0 {
			t.Fatalf("Snapshot() endpoints = %v, want empty", got.endpoints)
		}
		if got := maximum.Load(); got != maxConcurrentInspects {
			t.Fatalf("maximum concurrent inspections = %d, want %d", got, maxConcurrentInspects)
		}
	})
}

func TestSnapshotSelectsFatalInspectErrorByListOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newFakeAPI()
		api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
			return listResult("first", "second"), nil
		}
		firstErr := errors.New("first inspect failed")
		secondErr := errors.New("second inspect failed")
		releaseFirst := make(chan struct{})
		var secondStarted atomic.Bool
		api.inspectFn = func(ctx context.Context, id string, _ mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
			switch id {
			case "first":
				select {
				case <-releaseFirst:
					return mobyclient.ContainerInspectResult{}, firstErr
				case <-ctx.Done():
					return mobyclient.ContainerInspectResult{}, ctx.Err()
				}
			case "second":
				secondStarted.Store(true)
				return mobyclient.ContainerInspectResult{}, secondErr
			default:
				t.Fatalf("unexpected container ID %q", id)
				return mobyclient.ContainerInspectResult{}, nil
			}
		}

		result := make(chan error, 1)
		go func() {
			_, err := newClient(api).Snapshot(t.Context(), []string{"saltbox"})
			result <- err
		}()
		synctest.Wait()
		if !secondStarted.Load() {
			close(releaseFirst)
			t.Fatal("second inspection did not start while the first was blocked")
		}
		close(releaseFirst)
		synctest.Wait()

		err := <-result
		if !errors.Is(err, firstErr) {
			t.Fatalf("Snapshot() error = %v, want first list-order error %v", err, firstErr)
		}
		if errors.Is(err, secondErr) {
			t.Fatalf("Snapshot() error = %v, do not want later list-order error %v", err, secondErr)
		}
	})
}

func TestSnapshotSkipsDisconnectedConfiguredNetwork(t *testing.T) {
	api := newFakeAPI()
	api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
		return listResult("container-1"), nil
	}
	api.inspectFn = func(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
		return inspectResult(map[string]*network.EndpointSettings{
			"saltbox": endpoint("172.19.0.2", "", []string{"radarr"}, nil),
		}), nil
	}

	got, err := newClient(api).Snapshot(t.Context(), []string{"saltbox", "backend"})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	want := []daemon.Endpoint{{
		Network: "saltbox",
		IP:      netip.MustParseAddr("172.19.0.2"),
		Aliases: []string{"radarr"},
	}}
	assertEndpointsEqual(t, got, want)
}

func TestSnapshotSkipsUnpublishableEndpointAndKeepsHealthyContainer(t *testing.T) {
	tests := []struct {
		name     string
		endpoint *network.EndpointSettings
	}{
		{
			name:     "nil endpoint",
			endpoint: nil,
		},
		{
			name:     "missing address",
			endpoint: endpoint("", "", []string{"radarr"}, nil),
		},
		{
			name:     "IPv4 in global IPv6 field",
			endpoint: endpoint("", "172.19.0.2", []string{"radarr"}, nil),
		},
		{
			name:     "missing aliases",
			endpoint: endpoint("172.19.0.2", "", nil, []string{"radarr", "container-id"}),
		},
		{
			name:     "empty aliases",
			endpoint: endpoint("172.19.0.2", "", []string{"", ""}, []string{"radarr"}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI()
			api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
				return listResult("unpublishable-container", "healthy-container"), nil
			}
			api.inspectFn = func(_ context.Context, id string, _ mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
				switch id {
				case "healthy-container":
					return inspectResult(map[string]*network.EndpointSettings{
						"saltbox": endpoint("172.19.0.2", "", []string{"radarr"}, nil),
					}), nil
				case "unpublishable-container":
					return inspectResult(map[string]*network.EndpointSettings{"saltbox": tt.endpoint}), nil
				default:
					t.Fatalf("unexpected container ID %q", id)
					return mobyclient.ContainerInspectResult{}, nil
				}
			}

			got, err := newClient(api).Snapshot(t.Context(), []string{"saltbox"})
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			want := []daemon.Endpoint{{
				Network: "saltbox",
				IP:      netip.MustParseAddr("172.19.0.2"),
				Aliases: []string{"radarr"},
			}}
			assertEndpointsEqual(t, got, want)
		})
	}
}

func TestSnapshotSkipsUnpublishableNetworkAndKeepsOtherNetwork(t *testing.T) {
	tests := []struct {
		name     string
		endpoint *network.EndpointSettings
	}{
		{
			name:     "nil endpoint",
			endpoint: nil,
		},
		{
			name:     "missing address",
			endpoint: endpoint("", "", []string{"radarr"}, nil),
		},
		{
			name:     "empty aliases",
			endpoint: endpoint("172.19.0.2", "", []string{"", ""}, []string{"radarr", "container-id"}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI()
			api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
				return listResult("container-1"), nil
			}
			api.inspectFn = func(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
				return inspectResult(map[string]*network.EndpointSettings{
					"saltbox": tt.endpoint,
					"backend": endpoint("172.20.0.2", "", []string{"radarr"}, nil),
				}), nil
			}

			got, err := newClient(api).Snapshot(t.Context(), []string{"saltbox", "backend"})
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			want := []daemon.Endpoint{{
				Network: "backend",
				IP:      netip.MustParseAddr("172.20.0.2"),
				Aliases: []string{"radarr"},
			}}
			assertEndpointsEqual(t, got, want)
		})
	}
}

func TestSnapshotUsesIPv6FallbackAndOnlyInspectedAliases(t *testing.T) {
	api := newFakeAPI()
	api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
		return listResult("container-1"), nil
	}
	api.inspectFn = func(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
		return inspectResult(map[string]*network.EndpointSettings{
			"saltbox": endpoint(
				"",
				"2001:db8::2",
				[]string{"radarr", "", "bazarr", "radarr"},
				[]string{"radarr", "container-id", "generated-name"},
			),
		}), nil
	}

	got, err := newClient(api).Snapshot(t.Context(), []string{"saltbox"})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	want := []daemon.Endpoint{{
		Network: "saltbox",
		IP:      netip.MustParseAddr("2001:db8::2"),
		Aliases: []string{"bazarr", "radarr"},
	}}
	assertEndpointsEqual(t, got, want)
}

func TestSnapshotAllowsEmptyConfiguredNetworkContribution(t *testing.T) {
	api := newFakeAPI()
	inspectCalls := 0
	api.inspectFn = func(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
		inspectCalls++
		return mobyclient.ContainerInspectResult{}, nil
	}

	got, err := newClient(api).Snapshot(t.Context(), []string{"saltbox", "backend"})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Snapshot() endpoints = %v, want empty", got)
	}
	if inspectCalls != 0 {
		t.Fatalf("ContainerInspect() calls = %d, want 0", inspectCalls)
	}
}

func TestSnapshotSortsEndpointsWithinConfiguredNetworkOrder(t *testing.T) {
	api := newFakeAPI()
	api.listFn = func(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
		return listResult("container-z", "container-a"), nil
	}
	api.inspectFn = func(_ context.Context, id string, _ mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
		switch id {
		case "container-z":
			return inspectResult(map[string]*network.EndpointSettings{
				"saltbox": endpoint("172.19.0.20", "", []string{"zulu"}, nil),
				"backend": endpoint("172.20.0.5", "", []string{"bravo"}, nil),
			}), nil
		case "container-a":
			return inspectResult(map[string]*network.EndpointSettings{
				"saltbox": endpoint("172.19.0.3", "", []string{"alpha"}, nil),
				"backend": endpoint("172.20.0.8", "", []string{"alpha"}, nil),
			}), nil
		default:
			t.Fatalf("unexpected container ID %q", id)
			return mobyclient.ContainerInspectResult{}, nil
		}
	}

	got, err := newClient(api).Snapshot(t.Context(), []string{"saltbox", "backend"})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	want := []daemon.Endpoint{
		{Network: "saltbox", IP: netip.MustParseAddr("172.19.0.3"), Aliases: []string{"alpha"}},
		{Network: "saltbox", IP: netip.MustParseAddr("172.19.0.20"), Aliases: []string{"zulu"}},
		{Network: "backend", IP: netip.MustParseAddr("172.20.0.5"), Aliases: []string{"bravo"}},
		{Network: "backend", IP: netip.MustParseAddr("172.20.0.8"), Aliases: []string{"alpha"}},
	}
	assertEndpointsEqual(t, got, want)
}

func TestEventsScopesAndMapsNetworkEvents(t *testing.T) {
	api := newFakeAPI()
	sourceEvents := make(chan events.Message, 1)
	sourceErrors := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	eventCalls := 0
	api.eventsFn = func(gotCtx context.Context, opts mobyclient.EventsListOptions) mobyclient.EventsResult {
		eventCalls++
		if gotCtx != ctx {
			t.Fatal("Events() did not pass its context to Docker")
		}
		assertFilterValue(t, opts.Filters, "type", "network")
		assertFilterValue(t, opts.Filters, "event", "connect")
		assertFilterValue(t, opts.Filters, "event", "disconnect")
		assertFilterValue(t, opts.Filters, "network", "saltbox")
		assertFilterValue(t, opts.Filters, "network", "backend")
		assertNoFilterValue(t, opts.Filters, "network", "saltbox,backend")
		return mobyclient.EventsResult{Messages: sourceEvents, Err: sourceErrors}
	}

	mappedEvents, mappedErrors := newClient(api).Events(ctx, []string{"saltbox", "backend"})
	sourceEvents <- events.Message{
		Action: events.ActionConnect,
		Actor: events.Actor{
			ID: "network-id",
			Attributes: map[string]string{
				"container": "container-id",
				"name":      "backend",
			},
		},
	}

	want := daemon.Event{Action: "connect", ContainerID: "container-id", Network: "backend"}
	if got := receive(t, mappedEvents); got != want {
		t.Fatalf("mapped event = %+v, want %+v", got, want)
	}
	if eventCalls != 1 {
		t.Fatalf("Docker Events() calls = %d, want 1", eventCalls)
	}
	assertNoImmediateValue(t, mappedErrors)
	cancel()
	if _, ok := receiveOK(t, mappedEvents); ok {
		t.Fatal("mapped event channel is open after cancellation")
	}
	if _, ok := receiveOK(t, mappedErrors); ok {
		t.Fatal("mapped error channel is open after cancellation")
	}
}

func TestEventsRejectsEmptyNetworksBeforeDockerAccess(t *testing.T) {
	api := newFakeAPI()
	eventCalls := 0
	api.eventsFn = func(context.Context, mobyclient.EventsListOptions) mobyclient.EventsResult {
		eventCalls++
		return mobyclient.EventsResult{}
	}

	mappedEvents, mappedErrors := newClient(api).Events(t.Context(), nil)
	if eventCalls != 0 {
		t.Fatalf("Docker Events() calls = %d, want 0", eventCalls)
	}
	if _, ok := receiveOK(t, mappedEvents); ok {
		t.Fatal("mapped event channel is open, want closed")
	}
	if err := receive(t, mappedErrors); err == nil {
		t.Fatal("mapped error = nil, want empty-network error")
	}
	if _, ok := receiveOK(t, mappedErrors); ok {
		t.Fatal("mapped error channel is open after validation error")
	}
}

func TestEventsForwardsSourceErrorAndClosesMappedChannels(t *testing.T) {
	api := newFakeAPI()
	sourceEvents := make(chan events.Message)
	sourceErrors := make(chan error, 1)
	wantErr := errors.New("event stream failed")
	sourceErrors <- wantErr
	api.eventsFn = func(context.Context, mobyclient.EventsListOptions) mobyclient.EventsResult {
		return mobyclient.EventsResult{Messages: sourceEvents, Err: sourceErrors}
	}

	mappedEvents, mappedErrors := newClient(api).Events(t.Context(), []string{"saltbox"})
	if err := receive(t, mappedErrors); !errors.Is(err, wantErr) {
		t.Fatalf("mapped error = %v, want %v", err, wantErr)
	}
	if _, ok := receiveOK(t, mappedEvents); ok {
		t.Fatal("mapped event channel is open after source error")
	}
	if _, ok := receiveOK(t, mappedErrors); ok {
		t.Fatal("mapped error channel is open after source error")
	}
}

func TestEventsPrioritizesBufferedSourceErrorOverMessageClosure(t *testing.T) {
	for iteration := range 100 {
		api := newFakeAPI()
		sourceEvents := make(chan events.Message)
		sourceErrors := make(chan error, 1)
		wantErr := errors.New("buffered event stream failure")
		sourceErrors <- wantErr
		close(sourceEvents)
		close(sourceErrors)
		api.eventsFn = func(context.Context, mobyclient.EventsListOptions) mobyclient.EventsResult {
			return mobyclient.EventsResult{Messages: sourceEvents, Err: sourceErrors}
		}

		mappedEvents, mappedErrors := newClient(api).Events(t.Context(), []string{"saltbox"})
		err, ok := receiveOK(t, mappedErrors)
		if !ok || !errors.Is(err, wantErr) {
			t.Fatalf("iteration %d mapped error = (%v, %t), want buffered %v", iteration, err, ok, wantErr)
		}
		if _, ok := receiveOK(t, mappedEvents); ok {
			t.Fatalf("iteration %d mapped event channel remained open", iteration)
		}
		if _, ok := receiveOK(t, mappedErrors); ok {
			t.Fatalf("iteration %d mapped error channel remained open", iteration)
		}
	}
}

func TestEventsClosesMappedChannelsWhenSourceCloses(t *testing.T) {
	api := newFakeAPI()
	sourceEvents := make(chan events.Message)
	close(sourceEvents)
	api.eventsFn = func(context.Context, mobyclient.EventsListOptions) mobyclient.EventsResult {
		return mobyclient.EventsResult{Messages: sourceEvents, Err: make(chan error)}
	}

	mappedEvents, mappedErrors := newClient(api).Events(t.Context(), []string{"saltbox"})
	if _, ok := receiveOK(t, mappedEvents); ok {
		t.Fatal("mapped event channel is open after source event channel closed")
	}
	if _, ok := receiveOK(t, mappedErrors); ok {
		t.Fatal("mapped error channel is open after source event channel closed")
	}
}

func TestEventsCancellationClosesMappedChannels(t *testing.T) {
	api := newFakeAPI()
	api.eventsFn = func(context.Context, mobyclient.EventsListOptions) mobyclient.EventsResult {
		return mobyclient.EventsResult{
			Messages: make(chan events.Message),
			Err:      make(chan error),
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	mappedEvents, mappedErrors := newClient(api).Events(ctx, []string{"saltbox"})

	cancel()
	if _, ok := receiveOK(t, mappedEvents); ok {
		t.Fatal("mapped event channel is open after cancellation")
	}
	if _, ok := receiveOK(t, mappedErrors); ok {
		t.Fatal("mapped error channel is open after cancellation")
	}
}

func TestCloseWaitsForCanceledBlockedMapper(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newFakeAPI()
		sourceEvents := make(chan events.Message)
		api.eventsFn = func(context.Context, mobyclient.EventsListOptions) mobyclient.EventsResult {
			return mobyclient.EventsResult{
				Messages: sourceEvents,
				Err:      make(chan error),
			}
		}
		client := newClient(api)
		ctx, cancel := context.WithCancel(t.Context())
		_, mappedErrors := client.Events(ctx, []string{"saltbox"})

		go func() {
			sourceEvents <- events.Message{Action: events.ActionConnect}
			sourceEvents <- events.Message{Action: events.ActionDisconnect}
		}()
		synctest.Wait()

		closeDone := make(chan error, 1)
		go func() {
			closeDone <- client.Close()
		}()
		synctest.Wait()
		select {
		case err := <-closeDone:
			t.Fatalf("Close() returned before stream cancellation: %v", err)
		default:
		}

		cancel()
		if err := receive(t, closeDone); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		select {
		case _, ok := <-mappedErrors:
			if ok {
				t.Fatal("mapped error channel yielded a value after Close returned")
			}
		default:
			t.Fatal("mapped error channel is still open after Close returned")
		}
	})
}

func TestCloseReturnsWrappedAPIError(t *testing.T) {
	api := newFakeAPI()
	wantErr := errors.New("close failed")
	api.closeFn = func() error { return wantErr }

	err := newClient(api).Close()
	if !errors.Is(err, wantErr) {
		t.Fatalf("Close() error = %v, want wrapped %v", err, wantErr)
	}
}

func listResult(ids ...string) mobyclient.ContainerListResult {
	items := make([]container.Summary, 0, len(ids))
	for _, id := range ids {
		items = append(items, container.Summary{ID: id})
	}
	return mobyclient.ContainerListResult{Items: items}
}

func inspectResult(networks map[string]*network.EndpointSettings) mobyclient.ContainerInspectResult {
	return mobyclient.ContainerInspectResult{
		Container: container.InspectResponse{
			NetworkSettings: &container.NetworkSettings{Networks: networks},
		},
	}
}

func endpoint(ipv4, ipv6 string, aliases, dnsNames []string) *network.EndpointSettings {
	settings := &network.EndpointSettings{
		Aliases:  aliases,
		DNSNames: dnsNames,
	}
	if ipv4 != "" {
		settings.IPAddress = netip.MustParseAddr(ipv4)
	}
	if ipv6 != "" {
		settings.GlobalIPv6Address = netip.MustParseAddr(ipv6)
	}
	return settings
}

func assertFilterValue(t *testing.T, filters mobyclient.Filters, term, value string) {
	t.Helper()
	if !filters[term][value] {
		t.Fatalf("filter %q value %q is absent: %v", term, value, filters)
	}
}

func assertNoFilterValue(t *testing.T, filters mobyclient.Filters, term, value string) {
	t.Helper()
	if filters[term][value] {
		t.Fatalf("filter %q unexpectedly contains %q: %v", term, value, filters)
	}
}

func assertEndpointsEqual(t *testing.T, got, want []daemon.Endpoint) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Snapshot() endpoints = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Network != want[i].Network || got[i].IP != want[i].IP || !slices.Equal(got[i].Aliases, want[i].Aliases) {
			t.Fatalf("Snapshot() endpoint %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	value, ok := receiveOK(t, ch)
	if !ok {
		t.Fatal("channel closed before yielding a value")
	}
	return value
}

func receiveOK[T any](t *testing.T, ch <-chan T) (T, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case value, ok := <-ch:
		return value, ok
	case <-ctx.Done():
		t.Fatalf("timed out waiting for channel: %v", ctx.Err())
		var zero T
		return zero, false
	}
}

func assertNoImmediateValue[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case value, ok := <-ch:
		t.Fatalf("unexpected channel result: value %v, open %t", value, ok)
	default:
	}
}
