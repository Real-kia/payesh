package install

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestLiveSSHInstall is an opt-in disposable-host acceptance test. Artifacts
// are caller-built test inputs; production callers must bind VerifyArtifact
// to an accepted signed release manifest.
func TestLiveSSHInstall(t *testing.T) {
	if os.Getenv("PAYESH_LIVE_SSH_INSTALL") != "1" {
		t.Skip("set PAYESH_LIVE_SSH_INSTALL=1 on a disposable target")
	}
	host := os.Getenv("PAYESH_SSH_HOST")
	user := os.Getenv("PAYESH_SSH_USER")
	keyPath := os.Getenv("PAYESH_SSH_KEY")
	fingerprint := os.Getenv("PAYESH_SSH_HOST_KEY_FINGERPRINT")
	bindAddress := os.Getenv("PAYESH_SSH_BIND_ADDRESS")
	artifactDir := os.Getenv("PAYESH_SSH_ARTIFACT_DIR")
	if host == "" || user == "" || keyPath == "" || fingerprint == "" || artifactDir == "" {
		t.Fatal("live SSH acceptance environment is incomplete")
	}
	port := 22
	if value := os.Getenv("PAYESH_SSH_PORT"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
		port = parsed
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := map[string]string{}
	for _, name := range requiredArtifacts("node") {
		artifacts[name] = filepath.Join(artifactDir, name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	result, err := InstallOverSSH(ctx, SSHInstallOptions{
		Endpoint:                   SSHEndpoint{Host: host, Port: port, User: user, BindAddress: bindAddress},
		ExpectedHostKeyFingerprint: fingerprint,
		Auth:                       SSHAuth{PrivateKey: key},
		InstallerPath:              filepath.Join(artifactDir, "payesh-install"),
		Artifacts:                  artifacts,
		Role:                       "node",
		Start:                      true,
		VerifyArtifact: func(name, path string) error {
			_, err := ArtifactDigest(path, name == "web-assets")
			return err
		},
		// Enrollment and measurement are independently exercised by the live
		// transport tests. These callbacks prove the SSH state machine reaches
		// both mandatory post-install gates without weakening them.
		Enroll:             func(context.Context) error { return nil },
		VerifyMeasurements: func(context.Context) error { return nil },
	})
	if err != nil || result.Stage != "complete" || !result.Enrolled || !result.MeasurementsVerified {
		t.Fatalf("live SSH result=%+v err=%v", result, err)
	}
}
