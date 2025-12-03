package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/saltyorg/sdhm/internal/config"
	"github.com/saltyorg/sdhm/internal/hosts"
	"github.com/saltyorg/sdhm/internal/logger"
	"github.com/saltyorg/sdhm/internal/timeutil"
	"github.com/saltyorg/sdhm/internal/updater"

	"github.com/spf13/cobra"
)

var (
	intervalStr         string
	healthCheckPort     int
	healthCheckAddr     string
	hostsFilePath       string
	backupFilePath      string
	networksStr         string
	sectionName         string
	debounceDelayStr    string
	maxDebounceDelayStr string
	version             = "0.0.0.0-dev"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
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
	Version: version,
	RunE:    run,
}

var regenerateCmd = &cobra.Command{
	Use:   "regenerate",
	Short: "Regenerate the hosts file with fresh content",
	Long: `Regenerates the hosts file with Ubuntu Server defaults and an empty managed section.
This is useful for resetting a corrupted hosts file.

The generated file includes:
  - Standard localhost entries (127.0.0.1, 127.0.1.1)
  - IPv6 entries (ip6-localhost, ip6-loopback, etc.)
  - Empty managed section markers for Docker containers`,
	RunE: runRegenerate,
}

func init() {
	rootCmd.AddCommand(regenerateCmd)
	rootCmd.CompletionOptions.DisableDefaultCmd = true

	rootCmd.Flags().StringVarP(&intervalStr, "interval", "i", "5m", "Periodic validation interval (e.g., 30s, 5m, 1h, 1d)")
	rootCmd.Flags().IntVarP(&healthCheckPort, "health-port", "p", 8080, "Health check HTTP server port")
	rootCmd.Flags().StringVar(&healthCheckAddr, "health-addr", "127.0.0.1", "IP address to bind health check server (e.g., 127.0.0.1, 0.0.0.0)")
	rootCmd.Flags().StringVar(&hostsFilePath, "hosts-file", "/etc/hosts", "Path to hosts file (useful for testing)")
	rootCmd.Flags().StringVar(&backupFilePath, "backup-file", "/etc/hosts.backup", "Path to backup file")
	rootCmd.Flags().StringVarP(&networksStr, "networks", "n", "saltbox", "Comma-separated list of Docker networks to monitor (e.g., 'saltbox,bridge')")
	rootCmd.Flags().StringVar(&sectionName, "section-name", "DOCKER CONTAINERS", "Name for managed section in hosts file (markers auto-generated as '# BEGIN/END <name>')")
	rootCmd.Flags().StringVar(&debounceDelayStr, "debounce-delay", "1s", "Debounce delay (e.g., 500ms, 1s, 2s)")
	rootCmd.Flags().StringVar(&maxDebounceDelayStr, "debounce-max-delay", "5s", "Maximum debounce delay (e.g., 3s, 5s, 10s)")

	// Flags for regenerate command (shared with root)
	regenerateCmd.Flags().StringVar(&hostsFilePath, "hosts-file", "/etc/hosts", "Path to hosts file")
	regenerateCmd.Flags().StringVar(&backupFilePath, "backup-file", "/etc/hosts.backup", "Path to backup file")
	regenerateCmd.Flags().StringVar(&sectionName, "section-name", "DOCKER CONTAINERS", "Name for managed section in hosts file")
}

func run(cmd *cobra.Command, args []string) error {
	stdLogger := log.New(os.Stdout, "", log.LstdFlags)
	log := logger.New(stdLogger)

	// Check if running as root
	if os.Geteuid() != 0 {
		log.Warn("This program should be run as root to modify /etc/hosts")
	}

	// Parse interval
	interval, err := timeutil.ParseDuration(intervalStr)
	if err != nil {
		return fmt.Errorf("invalid interval '%s': %w\n\nInterval format:\n  s = seconds (e.g., 30s)\n  m = minutes (e.g., 5m)\n  h = hours   (e.g., 1h)\n  d = days    (e.g., 1d)", intervalStr, err)
	}

	// Parse debounce delay
	debounceDelay, err := timeutil.ParseDuration(debounceDelayStr)
	if err != nil {
		return fmt.Errorf("invalid debounce-delay '%s': %w", debounceDelayStr, err)
	}

	// Parse max debounce delay
	maxDebounceDelay, err := timeutil.ParseDuration(maxDebounceDelayStr)
	if err != nil {
		return fmt.Errorf("invalid debounce-max-delay '%s': %w", maxDebounceDelayStr, err)
	}

	// Parse networks (comma-separated)
	networks := strings.Split(networksStr, ",")
	for i := range networks {
		networks[i] = strings.TrimSpace(networks[i])
	}
	// Filter out empty strings
	var filteredNetworks []string
	for _, network := range networks {
		if network != "" {
			filteredNetworks = append(filteredNetworks, network)
		}
	}
	if len(filteredNetworks) == 0 {
		return fmt.Errorf("at least one network must be specified")
	}

	log.Info("Starting sdhm %s", version)
	log.Info("Monitoring networks: %v", filteredNetworks)
	log.Info("Interval: %s, Health check: %s:%d", interval, healthCheckAddr, healthCheckPort)

	// Create configuration
	cfg := config.NewConfig(interval)
	cfg.HealthCheckPort = healthCheckPort
	cfg.HealthCheckAddr = healthCheckAddr
	cfg.HostsFile = hostsFilePath
	cfg.BackupFile = backupFilePath
	cfg.DockerNetworks = filteredNetworks
	cfg.ManagedSectionName = sectionName
	cfg.DebounceDelay = debounceDelay
	cfg.MaxDebounceDelay = maxDebounceDelay

	// Create updater
	u, err := updater.NewUpdater(cfg, log)
	if err != nil {
		return fmt.Errorf("failed to create updater: %w", err)
	}
	defer u.Close()

	// Set up signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Handle signals in separate goroutine
	go func() {
		<-ctx.Done()
		log.Info("Received shutdown signal")
		u.Shutdown()
	}()

	// Start the updater (blocks until shutdown)
	if err := u.Start(ctx); err != nil {
		return fmt.Errorf("updater failed: %w", err)
	}

	log.Info("Shutdown complete")
	return nil
}

func runRegenerate(cmd *cobra.Command, args []string) error {
	stdLogger := log.New(os.Stdout, "", log.LstdFlags)
	l := logger.New(stdLogger)

	// Check if running as root
	if os.Geteuid() != 0 {
		l.Warn("This program should be run as root to modify /etc/hosts")
	}

	l.Info("Regenerating hosts file...")

	hostsManager := hosts.NewHostsFileManager(
		hostsFilePath,
		backupFilePath,
		sectionName,
	)

	if err := hostsManager.GenerateFreshHostsFile(context.Background()); err != nil {
		return fmt.Errorf("failed to regenerate hosts file: %w", err)
	}

	l.Info("Hosts file regenerated successfully at %s", hostsFilePath)
	return nil
}
