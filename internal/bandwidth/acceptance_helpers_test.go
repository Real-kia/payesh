package bandwidth

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func acceptanceUintEnv(t *testing.T, name string, fallback, minimum, maximum uint64) uint64 {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		t.Fatalf("%s must be a decimal integer in [%d,%d], got %q", name, minimum, maximum, raw)
	}
	return value
}

func acceptanceDurationSeconds(t *testing.T, name string, fallback, minimum, maximum uint64) uint64 {
	return acceptanceUintEnv(t, name, fallback, minimum, maximum)
}
