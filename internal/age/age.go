// Package age provides a small duration type that accepts values such as
// "12h", "7d" and "2w" on the command line.
package age

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that can be parsed from human friendly strings.
// A bare number is interpreted as a number of days.
type Duration struct {
	time.Duration
}

// Parse parses strings like "7d", "2w", "36h", "90m" or "30".
func Parse(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if s == "0" {
		return 0, nil
	}

	switch {
	case strings.HasSuffix(s, "d"):
		return parseNumber(s[:len(s)-1], 24*time.Hour, "d")
	case strings.HasSuffix(s, "w"):
		return parseNumber(s[:len(s)-1], 7*24*time.Hour, "w")
	}

	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("duration must not be negative: %q", s)
		}
		return d, nil
	}

	// A bare number means days, which is the most common unit here.
	if isNumber(s) {
		return parseNumber(s, 24*time.Hour, "d")
	}

	return 0, fmt.Errorf("invalid duration %q (use e.g. 7d, 36h, 90m)", s)
}

func parseNumber(s string, unit time.Duration, suffix string) (time.Duration, error) {
	if !isNumber(s) {
		return 0, fmt.Errorf("invalid duration %q", s+suffix)
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s+suffix, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("duration must not be negative: %q", s+suffix)
	}
	return time.Duration(n * float64(unit)), nil
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	dot := false
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' && !dot && i > 0 && i < len(s)-1:
			dot = true
		default:
			return false
		}
	}
	return true
}

// UnmarshalText implements encoding.TextUnmarshaler, which go-arg uses for
// custom flag types and default values.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := Parse(string(text))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// String renders the duration in its compact form.
func (d Duration) String() string {
	if d.Duration == 0 {
		return "0"
	}
	return d.Duration.String()
}

// Days is a convenience helper for building human readable descriptions.
func (d Duration) Days() float64 {
	return d.Duration.Hours() / 24
}
