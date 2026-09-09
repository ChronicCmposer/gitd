package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML duration strings
// ("90d", "6h", "30s"). The day suffix is required by the config schema
// (spool.retention 90d, R6-Q1) and is unsupported by time.ParseDuration.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler for the duration-string protocol.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

// D returns the duration as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders the duration in Go's canonical format.
func (d Duration) String() string { return time.Duration(d).String() }

// ParseDuration parses Go duration strings plus a day suffix ("90d").
func ParseDuration(s string) (time.Duration, error) {
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse day duration: %w", err)
		}
		if days < 0 {
			return 0, fmt.Errorf("negative duration")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
