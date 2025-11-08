package timeutil

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		// Valid cases
		{"seconds", "30s", 30 * time.Second, false},
		{"minutes", "5m", 5 * time.Minute, false},
		{"hours", "1h", 1 * time.Hour, false},
		{"days", "1d", 24 * time.Hour, false},
		{"multiple days", "7d", 7 * 24 * time.Hour, false},
		{"uppercase unit", "10S", 10 * time.Second, false},

		// Invalid cases
		{"too short", "5", 0, true},
		{"invalid number", "abcm", 0, true},
		{"invalid unit", "5x", 0, true},
		{"zero value", "0m", 0, true},
		{"negative value", "-5m", 0, true},
		{"empty string", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseDuration() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}
