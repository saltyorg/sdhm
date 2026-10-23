package hosts

import (
	"cmp"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"unicode"

	"github.com/saltyorg/sdhm/daemon"
)

type renderedEndpoint struct {
	network   string
	ip        netip.Addr
	ipText    string
	hostnames []string
}

// renderEndpoints converts a complete endpoint snapshot into a deterministic
// managed hosts body. The default network receives bare and qualified aliases;
// all other networks receive qualified aliases only. Hostnames claimed by
// different addresses across networks are omitted and reported as conflicts.
func renderEndpoints(endpoints []daemon.Endpoint, defaultNetwork string) ([]byte, []daemon.AliasConflict, error) {
	if err := validateNetwork(defaultNetwork); err != nil {
		return nil, nil, fmt.Errorf("default network: %w", err)
	}

	cloned := make([]daemon.Endpoint, len(endpoints))
	for i, endpoint := range endpoints {
		cloned[i] = endpoint
		cloned[i].Aliases = slices.Clone(endpoint.Aliases)
		if err := validateEndpoint(cloned[i]); err != nil {
			return nil, nil, fmt.Errorf("endpoint %d: %w", i, err)
		}
	}

	rendered := make([]renderedEndpoint, len(cloned))
	for i, endpoint := range cloned {
		hostnames := make(map[string]struct{}, len(endpoint.Aliases)*2)
		for _, alias := range endpoint.Aliases {
			if endpoint.Network == defaultNetwork {
				hostnames[alias] = struct{}{}
			}
			hostnames[alias+"."+endpoint.Network] = struct{}{}
		}

		rendered[i] = renderedEndpoint{
			network:   endpoint.Network,
			ip:        endpoint.IP,
			ipText:    endpoint.IP.String(),
			hostnames: slices.Sorted(maps.Keys(hostnames)),
		}
	}
	conflicts := omitConflictingHostnames(rendered)
	rendered = slices.DeleteFunc(rendered, func(endpoint renderedEndpoint) bool {
		return len(endpoint.hostnames) == 0
	})
	maxIPWidth := 0
	for _, endpoint := range rendered {
		maxIPWidth = max(maxIPWidth, len(endpoint.ipText))
	}

	slices.SortFunc(rendered, func(left, right renderedEndpoint) int {
		return cmp.Or(
			cmp.Compare(left.hostnames[0], right.hostnames[0]),
			cmp.Compare(left.network, right.network),
			left.ip.Compare(right.ip),
		)
	})

	var body strings.Builder
	for _, endpoint := range rendered {
		fmt.Fprintf(&body, "%-*s %s\n", maxIPWidth, endpoint.ipText, strings.Join(endpoint.hostnames, " "))
	}
	return []byte(body.String()), conflicts, nil
}

func omitConflictingHostnames(endpoints []renderedEndpoint) []daemon.AliasConflict {
	claims := make(map[string]map[daemon.AliasConflictEndpoint]struct{})
	for _, endpoint := range endpoints {
		claim := daemon.AliasConflictEndpoint{Network: endpoint.network, IP: endpoint.ip}
		for _, hostname := range endpoint.hostnames {
			key := strings.ToLower(hostname)
			if claims[key] == nil {
				claims[key] = make(map[daemon.AliasConflictEndpoint]struct{})
			}
			claims[key][claim] = struct{}{}
		}
	}

	var conflicts []daemon.AliasConflict
	for _, hostname := range slices.Sorted(maps.Keys(claims)) {
		owners := claims[hostname]
		if !hasCrossNetworkConflict(owners) {
			continue
		}
		conflictingEndpoints := slices.Collect(maps.Keys(owners))
		slices.SortFunc(conflictingEndpoints, func(left, right daemon.AliasConflictEndpoint) int {
			return cmp.Or(cmp.Compare(left.Network, right.Network), left.IP.Compare(right.IP))
		})
		conflicts = append(conflicts, daemon.AliasConflict{Hostname: hostname, Endpoints: conflictingEndpoints})
	}
	if len(conflicts) == 0 {
		return nil
	}
	omitted := make(map[string]struct{}, len(conflicts))
	for _, conflict := range conflicts {
		omitted[conflict.Hostname] = struct{}{}
	}
	for i := range endpoints {
		endpoints[i].hostnames = slices.DeleteFunc(endpoints[i].hostnames, func(hostname string) bool {
			_, conflict := omitted[strings.ToLower(hostname)]
			return conflict
		})
	}
	return conflicts
}

func hasCrossNetworkConflict(owners map[daemon.AliasConflictEndpoint]struct{}) bool {
	var first daemon.AliasConflictEndpoint
	differentNetwork := false
	differentAddress := false
	for owner := range owners {
		if !first.IP.IsValid() {
			first = owner
			continue
		}
		differentNetwork = differentNetwork || owner.Network != first.Network
		differentAddress = differentAddress || owner.IP != first.IP
		if differentNetwork && differentAddress {
			return true
		}
	}
	return false
}

func validateEndpoint(endpoint daemon.Endpoint) error {
	if !endpoint.IP.IsValid() {
		return fmt.Errorf("invalid IP address")
	}
	if err := validateNetwork(endpoint.Network); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	if len(endpoint.Aliases) == 0 {
		return fmt.Errorf("empty aliases")
	}
	for _, alias := range endpoint.Aliases {
		if err := validateHostnamePart(alias); err != nil {
			return fmt.Errorf("alias %q: %w", alias, err)
		}
	}
	return nil
}

func validateNetwork(network string) error {
	if network == "" {
		return fmt.Errorf("empty network name")
	}
	return validateHostnamePart(network)
}

func validateHostnamePart(value string) error {
	if value == "" {
		return fmt.Errorf("empty value")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '#' {
			return fmt.Errorf("contains a hosts-file delimiter")
		}
	}
	return nil
}
