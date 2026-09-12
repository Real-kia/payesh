package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/trust"
)

func TestSignWritesContractCompatibleManifestAndSignature(t *testing.T) {
	dir := t.TempDir()
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "release.key")
	if err := os.WriteFile(keyPath, private, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := `{"format":"payesh.release.v1","release":"1.2.3","created_at":"2026-09-12T00:00:00Z","min_core":"1.2.3","artifacts":[{"name":"payesh","os":"linux","arch":"amd64","sha256":"` + strings.Repeat("a", 64) + `","compressed_bytes":"1","unpacked_bytes":"1","url":"https://example.invalid/payesh.tar.gz"}],"signing_key_id":"unavailable-local"}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sign(dir, keyPath, "release-2026"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["signing_key_id"] != "release-2026" {
		t.Fatalf("manifest key ID = %v", decoded["signing_key_id"])
	}
	sigBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json.sig"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sigBytes) != base64.RawURLEncoding.EncodedLen(ed25519.SignatureSize) || strings.ContainsAny(string(sigBytes), "=\r\n \t") {
		t.Fatalf("signature is not exact unpadded base64url: %q", sigBytes)
	}
	canonical, err := trust.Canonicalize(decoded)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := trust.NewRegistry(trust.Anchor{KeyID: "release-2026", PublicKey: pub})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Verify("release-2026", canonical, string(sigBytes), timeNow()); err != nil {
		t.Fatalf("signature did not verify: %v", err)
	}
}

func TestSignRefusesExistingSignatureAndInsecureKey(t *testing.T) {
	dir := t.TempDir()
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "release.key")
	if err := os.WriteFile(keyPath, private, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"format":"payesh.release.v1","release":"1.2.3","created_at":"2026-09-12T00:00:00Z","min_core":"1.2.3","artifacts":[{"name":"payesh","os":"linux","arch":"amd64","sha256":"` + strings.Repeat("a", 64) + `","compressed_bytes":"1","unpacked_bytes":"1","url":"payesh.tar.gz"}],"signing_key_id":"unavailable-local"}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sign(dir, keyPath, "release-2026"); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("insecure key error = %v", err)
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json.sig"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sign(dir, keyPath, "release-2026"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing signature error = %v", err)
	}
}

// Kept as a function to make the test's trust check independent of wall-clock
// expiration policy while avoiding a production time override in the signer.
func timeNow() time.Time { return time.Now().UTC() }
