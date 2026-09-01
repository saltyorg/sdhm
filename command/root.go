package command

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type rootOptions struct {
	interval         string
	healthPort       int
	healthAddr       string
	hostsFile        string
	backupFile       string
	networks         string
	defaultNetwork   string
	sectionName      string
	debounceDelay    string
	maxDebounceDelay string
}

type regenerateOptions struct {
	hostsFile   string
	backupFile  string
	sectionName string
}

// NewRoot constructs a fresh, isolated SDHM command tree.
func NewRoot(version string, run RunFunc, regenerate RegenerateFunc) *cobra.Command {
	options := rootOptions{}
	root := &cobra.Command{
		Use:   "sdhm",
		Short: "Automatically updates /etc/hosts with Docker container hostnames",
		Long: `A daemon that monitors Docker network events and automatically updates
/etc/hosts with container hostnames on a specified network.

Features:
  - Monitors Docker network events (connect/disconnect)
  - Updates /etc/hosts with debounced event handling
  - Periodic validation to ensure sync
  - Health check endpoint for monitoring

Use 'sdhm regenerate' to reset a corrupted hosts file.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := options.config(cmd.Flags().Changed("default-network"))
			if err != nil {
				return err
			}
			if run == nil {
				return errors.New("run function is required")
			}
			return run(cmd.Context(), cfg)
		},
	}
	root.SetVersionTemplate("sdhm version {{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true

	flags := root.Flags()
	flags.StringVarP(&options.interval, "interval", "i", "5m", "Periodic validation interval (e.g., 30s, 5m, 1h, 1d)")
	flags.IntVarP(&options.healthPort, "health-port", "p", 8080, "Health check HTTP server port")
	flags.StringVar(&options.healthAddr, "health-addr", "127.0.0.1", "IP address to bind health check server (e.g., 127.0.0.1, 0.0.0.0)")
	flags.StringVar(&options.hostsFile, "hosts-file", "/etc/hosts", "Path to hosts file (useful for testing)")
	flags.StringVar(&options.backupFile, "backup-file", "/etc/hosts.backup", "Path to backup file")
	flags.StringVarP(&options.networks, "networks", "n", "saltbox", "Comma-separated list of Docker networks to monitor (e.g., 'saltbox,bridge')")
	flags.StringVar(&options.defaultNetwork, "default-network", "", "Monitored network that receives bare host aliases")
	flags.StringVar(&options.sectionName, "section-name", "DOCKER CONTAINERS", "Name for managed section in hosts file (markers auto-generated as '# BEGIN/END <name>')")
	flags.StringVar(&options.debounceDelay, "debounce-delay", "1s", "Debounce delay (e.g., 500ms, 1s, 2s)")
	flags.StringVar(&options.maxDebounceDelay, "debounce-max-delay", "5s", "Maximum debounce delay (e.g., 3s, 5s, 10s)")

	root.AddCommand(newRegenerateCommand(regenerate))
	return root
}

func newRegenerateCommand(regenerate RegenerateFunc) *cobra.Command {
	options := regenerateOptions{}
	cmd := &cobra.Command{
		Use:   "regenerate",
		Short: "Regenerate the hosts file with fresh content",
		Long: `Regenerates the hosts file with Ubuntu Server defaults and an empty managed section.
This is useful for resetting a corrupted hosts file.

The generated file includes:
  - Standard localhost entries (127.0.0.1, 127.0.1.1)
  - IPv6 entries (ip6-localhost, ip6-loopback, etc.)
  - Empty managed section markers for Docker containers`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := options.config()
			if err != nil {
				return err
			}
			if regenerate == nil {
				return errors.New("regenerate function is required")
			}
			return regenerate(cmd.Context(), cfg)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&options.hostsFile, "hosts-file", "/etc/hosts", "Path to hosts file")
	flags.StringVar(&options.backupFile, "backup-file", "/etc/hosts.backup", "Path to backup file")
	flags.StringVar(&options.sectionName, "section-name", "DOCKER CONTAINERS", "Name for managed section in hosts file")
	return cmd
}

func (o rootOptions) config(defaultNetworkSet bool) (Config, error) {
	if defaultNetworkSet && strings.TrimSpace(o.defaultNetwork) == "" {
		return Config{}, errors.New("default network must be non-empty when explicitly set")
	}
	networks, defaultNetwork, err := ParseNetworks(o.networks, o.defaultNetwork)
	if err != nil {
		return Config{}, err
	}
	interval, err := ParseDuration(o.interval)
	if err != nil {
		return Config{}, fmt.Errorf("invalid interval %q: %w", o.interval, err)
	}
	debounceDelay, err := ParseDuration(o.debounceDelay)
	if err != nil {
		return Config{}, fmt.Errorf("invalid debounce-delay %q: %w", o.debounceDelay, err)
	}
	maxDebounceDelay, err := ParseDuration(o.maxDebounceDelay)
	if err != nil {
		return Config{}, fmt.Errorf("invalid debounce-max-delay %q: %w", o.maxDebounceDelay, err)
	}

	cfg := Config{
		Networks:         networks,
		DefaultNetwork:   defaultNetwork,
		HostsFile:        cleanPath(o.hostsFile),
		BackupFile:       cleanPath(o.backupFile),
		SectionName:      o.sectionName,
		PeriodicInterval: interval,
		DebounceDelay:    debounceDelay,
		MaxDebounceDelay: maxDebounceDelay,
		HealthAddr:       strings.TrimSpace(o.healthAddr),
		HealthPort:       o.healthPort,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (o regenerateOptions) config() (RegenerateConfig, error) {
	cfg := RegenerateConfig{
		HostsFile:   cleanPath(o.hostsFile),
		BackupFile:  cleanPath(o.backupFile),
		SectionName: o.sectionName,
	}
	if err := validateFileConfig(cfg.HostsFile, cfg.BackupFile, cfg.SectionName); err != nil {
		return RegenerateConfig{}, err
	}
	return cfg, nil
}

func cleanPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return filepath.Clean(path)
}
