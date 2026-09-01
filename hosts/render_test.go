package hosts

import (
	"bytes"
	"net/netip"
	"slices"
	"testing"

	"github.com/saltyorg/sdhm/daemon"
)

func TestRenderEndpointsUsesDefaultNetworkForBareAliases(t *testing.T) {
	endpoints := []daemon.Endpoint{
		{
			Network: "backend",
			IP:      netip.MustParseAddr("172.20.0.2"),
			Aliases: []string{"radarr", "radarr"},
		},
		{
			Network: "saltbox",
			IP:      netip.MustParseAddr("2001:db8::2"),
			Aliases: []string{"bazarr"},
		},
		{
			Network: "saltbox",
			IP:      netip.MustParseAddr("172.18.0.2"),
			Aliases: []string{"radarr", "radarr"},
		},
	}

	got, err := renderEndpoints(endpoints, "saltbox")
	if err != nil {
		t.Fatalf("renderEndpoints() error = %v", err)
	}
	want := []byte("2001:db8::2 bazarr bazarr.saltbox\n172.18.0.2  radarr radarr.saltbox\n172.20.0.2  radarr.backend\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("renderEndpoints() = %q, want %q", got, want)
	}
}

func TestRenderEndpointsAlignsMixedIPColumns(t *testing.T) {
	endpoints := []daemon.Endpoint{
		{
			Network: "saltbox",
			IP:      netip.MustParseAddr("172.18.0.2"),
			Aliases: []string{"radarr"},
		},
		{
			Network: "saltbox",
			IP:      netip.MustParseAddr("2001:db8::2"),
			Aliases: []string{"bazarr"},
		},
	}

	got, err := renderEndpoints(endpoints, "saltbox")
	if err != nil {
		t.Fatalf("renderEndpoints() error = %v", err)
	}
	want := []byte("2001:db8::2 bazarr bazarr.saltbox\n172.18.0.2  radarr radarr.saltbox\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("renderEndpoints() = %q, want %q", got, want)
	}
}

func TestRenderEndpointsMovesBareAliasWithDefaultNetwork(t *testing.T) {
	endpoints := []daemon.Endpoint{
		{
			Network: "saltbox",
			IP:      netip.MustParseAddr("172.18.0.2"),
			Aliases: []string{"radarr"},
		},
		{
			Network: "backend",
			IP:      netip.MustParseAddr("172.20.0.2"),
			Aliases: []string{"radarr"},
		},
	}

	got, err := renderEndpoints(endpoints, "backend")
	if err != nil {
		t.Fatalf("renderEndpoints() error = %v", err)
	}
	want := []byte("172.20.0.2 radarr radarr.backend\n172.18.0.2 radarr.saltbox\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("renderEndpoints() = %q, want %q", got, want)
	}
}

func TestRenderEndpointsRejectsInvalidEndpointData(t *testing.T) {
	tests := []struct {
		name           string
		endpoints      []daemon.Endpoint
		defaultNetwork string
	}{
		{
			name: "invalid IP address",
			endpoints: []daemon.Endpoint{{
				Network: "saltbox",
				Aliases: []string{"radarr"},
			}},
			defaultNetwork: "saltbox",
		},
		{
			name: "empty aliases",
			endpoints: []daemon.Endpoint{{
				Network: "saltbox",
				IP:      netip.MustParseAddr("172.18.0.2"),
			}},
			defaultNetwork: "saltbox",
		},
		{
			name: "empty alias",
			endpoints: []daemon.Endpoint{{
				Network: "saltbox",
				IP:      netip.MustParseAddr("172.18.0.2"),
				Aliases: []string{""},
			}},
			defaultNetwork: "saltbox",
		},
		{
			name: "alias containing spaces",
			endpoints: []daemon.Endpoint{{
				Network: "saltbox",
				IP:      netip.MustParseAddr("172.18.0.2"),
				Aliases: []string{"rad arr"},
			}},
			defaultNetwork: "saltbox",
		},
		{
			name: "alias containing tabs",
			endpoints: []daemon.Endpoint{{
				Network: "saltbox",
				IP:      netip.MustParseAddr("172.18.0.2"),
				Aliases: []string{"rad\tarr"},
			}},
			defaultNetwork: "saltbox",
		},
		{
			name: "alias containing newlines",
			endpoints: []daemon.Endpoint{{
				Network: "saltbox",
				IP:      netip.MustParseAddr("172.18.0.2"),
				Aliases: []string{"rad\narr"},
			}},
			defaultNetwork: "saltbox",
		},
		{
			name: "alias containing a control byte",
			endpoints: []daemon.Endpoint{{
				Network: "saltbox",
				IP:      netip.MustParseAddr("172.18.0.2"),
				Aliases: []string{"rad\x00arr"},
			}},
			defaultNetwork: "saltbox",
		},
		{
			name: "alias containing a comment delimiter",
			endpoints: []daemon.Endpoint{{
				Network: "saltbox",
				IP:      netip.MustParseAddr("172.18.0.2"),
				Aliases: []string{"rad#arr"},
			}},
			defaultNetwork: "saltbox",
		},
		{
			name: "empty network name",
			endpoints: []daemon.Endpoint{{
				IP:      netip.MustParseAddr("172.18.0.2"),
				Aliases: []string{"radarr"},
			}},
			defaultNetwork: "saltbox",
		},
		{
			name: "empty default network",
			endpoints: []daemon.Endpoint{{
				Network: "saltbox",
				IP:      netip.MustParseAddr("172.18.0.2"),
				Aliases: []string{"radarr"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := renderEndpoints(tt.endpoints, tt.defaultNetwork); err == nil {
				t.Fatal("renderEndpoints() error = nil, want validation error")
			}
		})
	}
}

func TestRenderEndpointsDoesNotMutateInput(t *testing.T) {
	endpoints := []daemon.Endpoint{
		{
			Network: "saltbox",
			IP:      netip.MustParseAddr("172.18.0.2"),
			Aliases: []string{"sonarr", "radarr"},
		},
		{
			Network: "backend",
			IP:      netip.MustParseAddr("172.20.0.2"),
			Aliases: []string{"bazarr"},
		},
	}
	want := []daemon.Endpoint{
		{
			Network: "saltbox",
			IP:      netip.MustParseAddr("172.18.0.2"),
			Aliases: []string{"sonarr", "radarr"},
		},
		{
			Network: "backend",
			IP:      netip.MustParseAddr("172.20.0.2"),
			Aliases: []string{"bazarr"},
		},
	}

	if _, err := renderEndpoints(endpoints, "saltbox"); err != nil {
		t.Fatalf("renderEndpoints() error = %v", err)
	}
	if len(endpoints) != len(want) {
		t.Fatalf("endpoint count = %d, want %d", len(endpoints), len(want))
	}
	for i := range endpoints {
		if endpoints[i].Network != want[i].Network || endpoints[i].IP != want[i].IP || !slices.Equal(endpoints[i].Aliases, want[i].Aliases) {
			t.Fatalf("endpoints[%d] = %#v, want %#v", i, endpoints[i], want[i])
		}
	}
}
