package timeutil

import (
	"fmt"
	"strconv"
	"time"
)

// ParseDuration parses a human-readable duration string (e.g., "30s", "5m", "1h", "1d")
// Supports: s = seconds, m = minutes, h = hours, d = days
func ParseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid format: too short")
	}

	unit := s[len(s)-1]
	valueStr := s[:len(s)-1]

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return 0, fmt.Errorf("invalid number: %w", err)
	}

	if value <= 0 {
		return 0, fmt.Errorf("value must be positive")
	}

	switch unit {
	case 's', 'S':
		return time.Duration(value) * time.Second, nil
	case 'm', 'M':
		return time.Duration(value) * time.Minute, nil
	case 'h', 'H':
		return time.Duration(value) * time.Hour, nil
	case 'd', 'D':
		return time.Duration(value) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unsupported unit '%c', use s/m/h/d", unit)
	}
}
