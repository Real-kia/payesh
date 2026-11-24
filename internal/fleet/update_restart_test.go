package fleet

import (
	"context"
	"testing"

	"github.com/Real-kia/payesh/internal/version"
)

func TestRestartPendingOnlyForHubWithoutWorker(t *testing.T) {
	old := version.Value
	defer func() { version.Value = old }()
	version.Value = "1.0.0"
	s := &UpdateService{WebUpdateRoot: t.TempDir(), WebUpdateDir: t.TempDir(), InstalledVersion: func(context.Context) string { return "v1.2.0\n" }}
	if got := s.restartPending(context.Background()); got != "1.2.0" {
		t.Fatalf("got %q", got)
	}
	s.InstalledVersion = func(context.Context) string { return "1.0.0" }
	if got := s.restartPending(context.Background()); got != "" {
		t.Fatalf("same version offered restart: %q", got)
	}
	s.InstalledVersion = func(context.Context) string { return "garbage" }
	if got := s.restartPending(context.Background()); got != "" {
		t.Fatalf("invalid version offered restart: %q", got)
	}
}
