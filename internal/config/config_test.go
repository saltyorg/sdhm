package config

import (
	"testing"
	"time"
)

func TestNewConfig(t *testing.T) {
	interval := 5 * time.Minute

	cfg := NewConfig(interval)

	// Test default values
	if cfg.HostsFile != "/etc/hosts" {
		t.Errorf("HostsFile = %q, want %q", cfg.HostsFile, "/etc/hosts")
	}

	if cfg.BackupFile != "/etc/hosts.backup" {
		t.Errorf("BackupFile = %q, want %q", cfg.BackupFile, "/etc/hosts.backup")
	}

	if len(cfg.DockerNetworks) != 1 || cfg.DockerNetworks[0] != "saltbox" {
		t.Errorf("DockerNetworks = %v, want %v", cfg.DockerNetworks, []string{"saltbox"})
	}

	if cfg.DebounceDelay != 1*time.Second {
		t.Errorf("DebounceDelay = %v, want %v", cfg.DebounceDelay, 1*time.Second)
	}

	if cfg.MaxDebounceDelay != 5*time.Second {
		t.Errorf("MaxDebounceDelay = %v, want %v", cfg.MaxDebounceDelay, 5*time.Second)
	}

	if cfg.PeriodicInterval != interval {
		t.Errorf("PeriodicInterval = %v, want %v", cfg.PeriodicInterval, interval)
	}

	if cfg.HealthCheckPort != 8080 {
		t.Errorf("HealthCheckPort = %d, want %d", cfg.HealthCheckPort, 8080)
	}

	if cfg.HealthCheckAddr != "127.0.0.1" {
		t.Errorf("HealthCheckAddr = %q, want %q", cfg.HealthCheckAddr, "127.0.0.1")
	}

	if cfg.ManagedSectionName != "DOCKER CONTAINERS" {
		t.Errorf("ManagedSectionName = %q, want %q", cfg.ManagedSectionName, "DOCKER CONTAINERS")
	}
}

func TestNewConfig_DifferentIntervals(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{"30 seconds", 30 * time.Second},
		{"1 minute", 1 * time.Minute},
		{"10 minutes", 10 * time.Minute},
		{"1 hour", 1 * time.Hour},
		{"1 day", 24 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewConfig(tt.interval)
			if cfg.PeriodicInterval != tt.interval {
				t.Errorf("PeriodicInterval = %v, want %v", cfg.PeriodicInterval, tt.interval)
			}
		})
	}
}
