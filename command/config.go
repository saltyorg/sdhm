package command

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Config contains normalized, validated runtime configuration.
type Config struct {
	Networks         []string
	DefaultNetwork   string
	HostsFile        string
	BackupFile       string
	SectionName      string
	PeriodicInterval time.Duration
	DebounceDelay    time.Duration
	MaxDebounceDelay time.Duration
	HealthAddr       string
	HealthPort       int
}

// RegenerateConfig contains normalized, validated hosts regeneration settings.
type RegenerateConfig struct {
	HostsFile   string
	BackupFile  string
	SectionName string
}

// RunFunc runs the daemon using validated command configuration.
type RunFunc func(context.Context, Config) error

// RegenerateFunc regenerates the hosts file using validated command configuration.
type RegenerateFunc func(context.Context, RegenerateConfig) error

// ParseNetworks normalizes the comma-separated network option and resolves its
// default network.
func ParseNetworks(raw, requestedDefault string) ([]string, string, error) {
	seen := make(map[string]struct{})
	networks := make([]string, 0)
	for network := range strings.SplitSeq(raw, ",") {
		network = strings.TrimSpace(network)
		if network == "" {
			continue
		}
		if strings.IndexFunc(network, invalidNetworkRune) >= 0 {
			return nil, "", fmt.Errorf("network %q contains whitespace, control characters, or #", network)
		}
		if _, exists := seen[network]; exists {
			continue
		}
		seen[network] = struct{}{}
		networks = append(networks, network)
	}
	if len(networks) == 0 {
		return nil, "", errors.New("at least one network must be specified")
	}

	requestedDefault = strings.TrimSpace(requestedDefault)
	if requestedDefault != "" {
		if _, exists := seen[requestedDefault]; !exists {
			return nil, "", fmt.Errorf("default network %q is not monitored", requestedDefault)
		}
		return networks, requestedDefault, nil
	}
	if _, exists := seen["saltbox"]; exists {
		return networks, "saltbox", nil
	}
	return networks, networks[0], nil
}

// Validate verifies that Config is normalized and safe to pass to runtime
// modules.
func (c Config) Validate() error {
	if err := validateNetworks(c.Networks, c.DefaultNetwork); err != nil {
		return err
	}
	if err := validateFileConfig(c.HostsFile, c.BackupFile, c.SectionName); err != nil {
		return err
	}
	if c.PeriodicInterval <= 0 {
		return errors.New("periodic interval must be positive")
	}
	if c.DebounceDelay <= 0 {
		return errors.New("debounce delay must be positive")
	}
	if c.MaxDebounceDelay <= 0 {
		return errors.New("maximum debounce delay must be positive")
	}
	if c.MaxDebounceDelay < c.DebounceDelay {
		return errors.New("maximum debounce delay must not be less than debounce delay")
	}
	if c.HealthAddr == "" || strings.TrimSpace(c.HealthAddr) != c.HealthAddr {
		return errors.New("health address must be non-empty and normalized")
	}
	if _, err := netip.ParseAddr(c.HealthAddr); err != nil {
		return fmt.Errorf("health address %q must be a literal IPv4 or IPv6 address: %w", c.HealthAddr, err)
	}
	if c.HealthPort < 1 || c.HealthPort > 65535 {
		return fmt.Errorf("health port %d is outside 1..65535", c.HealthPort)
	}
	return nil
}

func validateNetworks(networks []string, defaultNetwork string) error {
	if len(networks) == 0 {
		return errors.New("at least one network must be specified")
	}
	seen := make(map[string]struct{}, len(networks))
	for _, network := range networks {
		if network == "" || strings.TrimSpace(network) != network || strings.IndexFunc(network, invalidNetworkRune) >= 0 {
			return fmt.Errorf("network %q is not normalized", network)
		}
		if _, exists := seen[network]; exists {
			return fmt.Errorf("network %q is duplicated", network)
		}
		seen[network] = struct{}{}
	}
	if _, exists := seen[defaultNetwork]; !exists {
		return fmt.Errorf("default network %q is not monitored", defaultNetwork)
	}
	return nil
}

func validateFileConfig(hostsFile, backupFile, sectionName string) error {
	if err := validateCleanPath("hosts file", hostsFile); err != nil {
		return err
	}
	if err := validateCleanPath("backup file", backupFile); err != nil {
		return err
	}
	if hostsFile == backupFile {
		return errors.New("hosts file and backup file must be different paths")
	}
	if strings.TrimSpace(sectionName) == "" {
		return errors.New("section name must be non-empty")
	}
	if strings.ContainsAny(sectionName, "\r\n") {
		return errors.New("section name must be a single line")
	}
	return nil
}

func validateCleanPath(name, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s must be non-empty", name)
	}
	if filepath.Clean(path) != path {
		return fmt.Errorf("%s %q is not cleaned", name, path)
	}
	return nil
}

func invalidNetworkRune(r rune) bool {
	return r == '#' || unicode.IsSpace(r) || unicode.IsControl(r)
}
