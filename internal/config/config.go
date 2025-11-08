package config

import "time"

// Config holds the application configuration
type Config struct {
	HostsFile          string
	BackupFile         string
	DockerNetworks     []string // Multiple networks to monitor
	DebounceDelay      time.Duration
	MaxDebounceDelay   time.Duration
	PeriodicInterval   time.Duration
	HealthCheckPort    int
	HealthCheckAddr    string // IP address to bind health check server (e.g., "127.0.0.1", "0.0.0.0")
	ManagedSectionName string // Name for the managed section (e.g., "DOCKER CONTAINERS")
}

// NewConfig creates a new configuration with default values
func NewConfig(periodicInterval time.Duration) *Config {
	return &Config{
		HostsFile:          "/etc/hosts",
		BackupFile:         "/etc/hosts.backup",
		DockerNetworks:     []string{"saltbox"},
		DebounceDelay:      1 * time.Second,
		MaxDebounceDelay:   5 * time.Second,
		PeriodicInterval:   periodicInterval,
		HealthCheckPort:    8080,
		HealthCheckAddr:    "127.0.0.1", // Default to localhost for security
		ManagedSectionName: "DOCKER CONTAINERS",
	}
}
