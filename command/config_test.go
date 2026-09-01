package command

import (
	"slices"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "milliseconds", raw: "500ms", want: 500 * time.Millisecond},
		{name: "seconds", raw: "1s", want: time.Second},
		{name: "compound", raw: "1m30s", want: 90 * time.Second},
		{name: "hours", raw: "1h", want: time.Hour},
		{name: "one integer day", raw: "1d", want: 24 * time.Hour},
		{name: "seven integer days", raw: "7d", want: 7 * 24 * time.Hour},
		{name: "empty", raw: "", wantErr: true},
		{name: "zero", raw: "0s", wantErr: true},
		{name: "negative", raw: "-1s", wantErr: true},
		{name: "fractional day", raw: "1.5d", wantErr: true},
		{name: "unknown unit", raw: "1x", wantErr: true},
		{name: "integer day overflow", raw: "106752d", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseDuration(test.raw)
			if (err != nil) != test.wantErr {
				t.Fatalf("ParseDuration(%q) error = %v, wantErr %v", test.raw, err, test.wantErr)
			}
			if got != test.want {
				t.Errorf("ParseDuration(%q) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

func TestParseNetworks(t *testing.T) {
	tests := []struct {
		name             string
		raw              string
		requestedDefault string
		wantNetworks     []string
		wantDefault      string
		wantErr          bool
	}{
		{
			name:         "default Saltbox network",
			raw:          "saltbox",
			wantNetworks: []string{"saltbox"},
			wantDefault:  "saltbox",
		},
		{
			name:             "trim filter deduplicate and explicit default",
			raw:              " saltbox, ,backend,saltbox,backend ",
			requestedDefault: " backend ",
			wantNetworks:     []string{"saltbox", "backend"},
			wantDefault:      "backend",
		},
		{
			name:         "implicit Saltbox preference",
			raw:          "backend,saltbox,frontend",
			wantNetworks: []string{"backend", "saltbox", "frontend"},
			wantDefault:  "saltbox",
		},
		{
			name:         "first network fallback",
			raw:          "backend,frontend",
			wantNetworks: []string{"backend", "frontend"},
			wantDefault:  "backend",
		},
		{name: "all empty", raw: " , , ", wantErr: true},
		{name: "internal space", raw: "salt box", wantErr: true},
		{name: "internal tab", raw: "salt\tbox", wantErr: true},
		{name: "newline", raw: "salt\nbox", wantErr: true},
		{name: "control rune", raw: "salt\x00box", wantErr: true},
		{name: "comment delimiter", raw: "salt#box", wantErr: true},
		{
			name:             "explicit default not monitored",
			raw:              "saltbox,backend",
			requestedDefault: "frontend",
			wantErr:          true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			networks, defaultNetwork, err := ParseNetworks(test.raw, test.requestedDefault)
			if (err != nil) != test.wantErr {
				t.Fatalf("ParseNetworks(%q, %q) error = %v, wantErr %v", test.raw, test.requestedDefault, err, test.wantErr)
			}
			if !slices.Equal(networks, test.wantNetworks) {
				t.Errorf("ParseNetworks() networks = %v, want %v", networks, test.wantNetworks)
			}
			if defaultNetwork != test.wantDefault {
				t.Errorf("ParseNetworks() default = %q, want %q", defaultNetwork, test.wantDefault)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	valid := Config{
		Networks:         []string{"saltbox", "backend"},
		DefaultNetwork:   "saltbox",
		HostsFile:        "/etc/hosts",
		BackupFile:       "/etc/hosts.backup",
		SectionName:      "DOCKER CONTAINERS",
		PeriodicInterval: 5 * time.Minute,
		DebounceDelay:    time.Second,
		MaxDebounceDelay: 5 * time.Second,
		HealthAddr:       "127.0.0.1",
		HealthPort:       8080,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid Config.Validate() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "no networks", mutate: func(c *Config) { c.Networks = nil }},
		{name: "empty network", mutate: func(c *Config) { c.Networks = []string{""} }},
		{name: "untrimmed network", mutate: func(c *Config) { c.Networks = []string{" saltbox"} }},
		{name: "network with whitespace", mutate: func(c *Config) { c.Networks = []string{"salt box"}; c.DefaultNetwork = "salt box" }},
		{name: "network with control rune", mutate: func(c *Config) { c.Networks = []string{"salt\x00box"}; c.DefaultNetwork = "salt\x00box" }},
		{name: "network with comment delimiter", mutate: func(c *Config) { c.Networks = []string{"salt#box"}; c.DefaultNetwork = "salt#box" }},
		{name: "duplicate network", mutate: func(c *Config) { c.Networks = []string{"saltbox", "saltbox"} }},
		{name: "default network not monitored", mutate: func(c *Config) { c.DefaultNetwork = "frontend" }},
		{name: "empty hosts path", mutate: func(c *Config) { c.HostsFile = "" }},
		{name: "unclean hosts path", mutate: func(c *Config) { c.HostsFile = "/etc/../etc/hosts" }},
		{name: "empty backup path", mutate: func(c *Config) { c.BackupFile = "" }},
		{name: "unclean backup path", mutate: func(c *Config) { c.BackupFile = "/etc/hosts/../hosts.backup" }},
		{name: "same hosts and backup path", mutate: func(c *Config) { c.BackupFile = c.HostsFile }},
		{name: "empty section", mutate: func(c *Config) { c.SectionName = "" }},
		{name: "blank section", mutate: func(c *Config) { c.SectionName = " \t " }},
		{name: "section with newline", mutate: func(c *Config) { c.SectionName = "DOCKER\n# END INJECTED" }},
		{name: "section with carriage return", mutate: func(c *Config) { c.SectionName = "DOCKER\r# END INJECTED" }},
		{name: "periodic interval not positive", mutate: func(c *Config) { c.PeriodicInterval = 0 }},
		{name: "debounce delay not positive", mutate: func(c *Config) { c.DebounceDelay = -time.Second }},
		{name: "maximum debounce delay not positive", mutate: func(c *Config) { c.MaxDebounceDelay = 0 }},
		{name: "maximum debounce below debounce", mutate: func(c *Config) { c.MaxDebounceDelay = c.DebounceDelay / 2 }},
		{name: "empty health address", mutate: func(c *Config) { c.HealthAddr = "" }},
		{name: "health port below range", mutate: func(c *Config) { c.HealthPort = 0 }},
		{name: "health port above range", mutate: func(c *Config) { c.HealthPort = 65536 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			cfg.Networks = slices.Clone(valid.Networks)
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Config.Validate() error = nil, want validation error")
			}
		})
	}
}

func TestConfigValidateAcceptsEqualDebounceBounds(t *testing.T) {
	cfg := Config{
		Networks:         []string{"saltbox"},
		DefaultNetwork:   "saltbox",
		HostsFile:        "/etc/hosts",
		BackupFile:       "/etc/hosts.backup",
		SectionName:      "DOCKER CONTAINERS",
		PeriodicInterval: time.Minute,
		DebounceDelay:    time.Second,
		MaxDebounceDelay: time.Second,
		HealthAddr:       "127.0.0.1",
		HealthPort:       1,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Config.Validate() with equal debounce bounds error = %v", err)
	}
}

func TestConfigValidateHealthAddressLiterals(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{name: "IPv4 loopback", address: "127.0.0.1"},
		{name: "IPv4 unspecified", address: "0.0.0.0"},
		{name: "IPv6 loopback", address: "::1"},
		{name: "not an address", address: "bad address", wantErr: true},
		{name: "address with port", address: "127.0.0.1:8080", wantErr: true},
		{name: "malformed IPv6", address: "2001:db8:::1", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{
				Networks:         []string{"saltbox"},
				DefaultNetwork:   "saltbox",
				HostsFile:        "/etc/hosts",
				BackupFile:       "/etc/hosts.backup",
				SectionName:      "DOCKER CONTAINERS",
				PeriodicInterval: time.Minute,
				DebounceDelay:    time.Second,
				MaxDebounceDelay: time.Second,
				HealthAddr:       test.address,
				HealthPort:       8080,
			}
			err := cfg.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Config.Validate() health address %q error = %v, wantErr %v", test.address, err, test.wantErr)
			}
		})
	}
}
