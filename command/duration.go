package command

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const day = 24 * time.Hour

// ParseDuration accepts positive Go durations and positive integer days.
func ParseDuration(raw string) (time.Duration, error) {
	parsed, parseErr := time.ParseDuration(raw)
	if parseErr == nil {
		if parsed <= 0 {
			return 0, errors.New("duration must be positive")
		}
		return parsed, nil
	}

	daysRaw, isDays := strings.CutSuffix(raw, "d")
	if !isDays || daysRaw == "" {
		return 0, fmt.Errorf("parse duration %q: %w", raw, parseErr)
	}
	for _, digit := range daysRaw {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("parse duration %q: %w", raw, parseErr)
		}
	}

	days, err := strconv.ParseUint(daysRaw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", raw, err)
	}
	if days == 0 {
		return 0, errors.New("duration must be positive")
	}
	if days > uint64(math.MaxInt64/int64(day)) {
		return 0, fmt.Errorf("duration %q overflows time.Duration", raw)
	}
	return time.Duration(days) * day, nil
}
