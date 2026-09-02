package daemon

import (
	"context"
	"net/netip"
	"testing"
)

type fakeNetworkSource struct{}
type fakeHostStore struct{}
type fakeHealthServer struct{}

func (*fakeNetworkSource) Ping(context.Context) error { return nil }
func (*fakeNetworkSource) Snapshot(context.Context, []string) ([]Endpoint, error) {
	return nil, nil
}
func (*fakeNetworkSource) Events(context.Context, []string) (<-chan Event, <-chan error) {
	return nil, nil
}
func (*fakeNetworkSource) Close() error { return nil }

func (*fakeHostStore) Prepare(context.Context) (PrepareResult, error) { return PrepareResult{}, nil }
func (*fakeHostStore) Apply(context.Context, []Endpoint) error        { return nil }

func (*fakeHealthServer) Start() error                   { return nil }
func (*fakeHealthServer) Done() <-chan struct{}          { return make(chan struct{}) }
func (*fakeHealthServer) Err() error                     { return nil }
func (*fakeHealthServer) Shutdown(context.Context) error { return nil }

var _ NetworkSource = (*fakeNetworkSource)(nil)
var _ HostStore = (*fakeHostStore)(nil)
var _ HealthServer = (*fakeHealthServer)(nil)

func TestEndpointCarriesNetworkIdentity(t *testing.T) {
	got := Endpoint{
		Network: "saltbox",
		IP:      netip.MustParseAddr("172.18.0.2"),
		Aliases: []string{"radarr"},
	}
	if got.Network != "saltbox" || got.Aliases[0] != "radarr" {
		t.Fatalf("endpoint = %#v", got)
	}
}
