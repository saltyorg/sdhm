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
	hostnames []string
}

// renderEndpoints converts a complete endpoint snapshot into a deterministic
// managed hosts body. The default network receives bare and qualified aliases;
// all other networks receive qualified aliases only.
func renderEndpoints(endpoints []daemon.Endpoint, defaultNetwork string) ([]byte, error) {
	if err := validateNetwork(defaultNetwork); err != nil {
		return nil, fmt.Errorf("default network: %w", err)
	}

	cloned := make([]daemon.Endpoint, len(endpoints))
	for i, endpoint := range endpoints {
		cloned[i] = endpoint
		cloned[i].Aliases = slices.Clone(endpoint.Aliases)
		if err := validateEndpoint(cloned[i]); err != nil {
			return nil, fmt.Errorf("endpoint %d: %w", i, err)
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
			hostnames: slices.Sorted(maps.Keys(hostnames)),
		}
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
		fmt.Fprintf(&body, "%s %s\n", endpoint.ip, strings.Join(endpoint.hostnames, " "))
	}
	return []byte(body.String()), nil
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
