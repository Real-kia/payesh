package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/trust"
)

func signedManifest(t *testing.T, created time.Time, release string) (contracts.ReleaseManifest, string, *trust.Registry, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("release archive")
	sum := sha256.Sum256(body)
	m := contracts.ReleaseManifest{Format: contracts.ReleaseFormat, Release: release, CreatedAt: created, MinCore: "0.1.0", SigningKeyID: "release-test", Artifacts: []contracts.ReleaseArtifact{{Name: "payesh-agent", OS: "linux", Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), CompressedBytes: uint64(len(body)), UnpackedBytes: uint64(len(body)) + 1, URL: "https://example.invalid/payesh-agent"}}}
	_, sig, err := trust.Sign(priv, m)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := trust.NewRegistry(trust.Anchor{KeyID: "release-test", PublicKey: pub, ExpiresAt: created.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return m, sig, registry, body
}

func TestVerifyManifestAndArtifact(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	m, sig, registry, body := signedManifest(t, now, "0.2.0")
	if err := VerifyManifest(registry, m, sig, "0.1.0", AcceptedState{}, now); err != nil {
		t.Fatal(err)
	}
	a, err := SelectArtifact(m, "payesh-agent", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(a, body); err != nil {
		t.Fatal(err)
	}
	body[0] ^= 1
	if err := VerifyArtifact(a, body); err == nil {
		t.Fatal("tampered artifact accepted")
	}
}

func TestVerifyManifestRejectsReplayAndPlatformAbsence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	m, sig, registry, _ := signedManifest(t, now.Add(-time.Minute), "0.2.0")
	state := AcceptedState{Release: "0.3.0", CreatedAt: now}
	if err := VerifyManifest(registry, m, sig, "0.1.0", state, now); !errors.Is(err, ErrManifestReplay) {
		t.Fatalf("expected replay rejection, got %v", err)
	}
	if _, err := SelectArtifact(m, "payesh-agent", "linux", "arm64"); !errors.Is(err, ErrArtifactUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
}

func TestVerifyManifestRejectsCorruptAcceptedStateWithoutPanicking(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	m, sig, registry, _ := signedManifest(t, now, "0.2.0")
	err := VerifyManifest(registry, m, sig, "0.1.0", AcceptedState{Release: "broken", CreatedAt: now}, now)
	if err == nil {
		t.Fatal("corrupt durable anti-replay state was accepted")
	}
}

func TestAcceptedStateRoundTripIsRestricted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "accepted.json")
	want := AcceptedState{Release: "1.2.3", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := SaveAcceptedState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAcceptedState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
