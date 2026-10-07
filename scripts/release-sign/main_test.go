package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/release"
	"github.com/Real-kia/payesh/internal/trust"
)

func TestSignWritesContractCompatibleManifestAndSignature(t *testing.T) {
	for _, schema := range []bool{false, true} {
		t.Run(fmt.Sprintf("schema=%v", schema), func(t *testing.T) {
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
			if schema {
				manifest = strings.Replace(manifest, `"artifacts":`, `"database_schema":{"min_readable":"1","current":"6"},"artifacts":`, 1)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			bootstrap := []byte("#!/bin/sh\nexit 0\n")
			digest := sha256.Sum256(bootstrap)
			checksums := []byte(hex.EncodeToString(digest[:]) + "  install.sh\n" + strings.Repeat("a", 64) + "  payesh.tar.gz\n")
			if schema {
				digest := sha256.Sum256([]byte(manifest))
				checksums = append(checksums, []byte(hex.EncodeToString(digest[:])+"  manifest.json\n")...)
			}
			if err := os.WriteFile(filepath.Join(dir, "install.sh"), bootstrap, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), checksums, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := sign(dir, keyPath, "release-2026"); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var decoded validatorManifest
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.SigningKeyID != "release-2026" {
				t.Fatalf("manifest key ID = %v", decoded.SigningKeyID)
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
			bootstrapSignature, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS.sig"))
			if err != nil {
				t.Fatal(err)
			}
			if schema {
				checksums, err = os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
				if err != nil {
					t.Fatal(err)
				}
				if err := release.VerifyChecksumFile(checksums, "manifest.json", body); err != nil {
					t.Fatalf("signer did not rebind normalized manifest: %v", err)
				}
				if decoded.DatabaseSchema == nil || decoded.DatabaseSchema.Current != 6 {
					t.Fatal("signer lost schema declaration")
				}
			}
			if err := release.VerifyBootstrap(pub, "1.2.3", "release-2026", checksums, bootstrapSignature); err != nil {
				t.Fatal(err)
			}
			if release.VerifyBootstrap(pub, "1.2.4", "release-2026", checksums, bootstrapSignature) == nil {
				t.Fatal("bootstrap signature replay accepted")
			}
		})
	}
}

// Decode independently with time.Time, as the release validator does. Reusing
// the signer's timestamp wrapper or a raw map would miss wire normalization.
type validatorManifest struct {
	DatabaseSchema *contracts.ReleaseDatabaseSchema `json:"database_schema,omitempty"`
	Format         string                           `json:"format"`
	Release        string                           `json:"release"`
	CreatedAt      time.Time                        `json:"created_at"`
	MinCore        string                           `json:"min_core"`
	Artifacts      []artifact                       `json:"artifacts"`
	SigningKeyID   string                           `json:"signing_key_id"`
}

func TestSignTimestampMatchesValidatorCanonicalization(t *testing.T) {
	for _, test := range []struct {
		name, timestamp string
		invalid         bool
	}{
		{name: "UTC offset", timestamp: "2026-09-12T00:00:00+00:00"},
		{name: "fractional trailing zeros", timestamp: "2026-09-12T00:00:00.123400Z"},
		{name: "nonzero offset", timestamp: "2026-09-12T03:30:00.123400+03:30"},
		{name: "invalid timestamp", timestamp: "not-a-timestamp", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			public, private, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(dir, "release.key")
			if err := os.WriteFile(keyPath, private, 0o600); err != nil {
				t.Fatal(err)
			}
			body := []byte(fmt.Sprintf(`{"format":"payesh.release.v1","release":"1.2.3","created_at":%q,"min_core":"1.2.3","artifacts":[{"name":"payesh","os":"linux","arch":"amd64","sha256":"%s","compressed_bytes":"1","unpacked_bytes":"1","url":"payesh.tar.gz"}],"signing_key_id":"unavailable-local"}`, test.timestamp, strings.Repeat("a", 64)))
			manifestPath := filepath.Join(dir, "manifest.json")
			if err := os.WriteFile(manifestPath, body, 0o644); err != nil {
				t.Fatal(err)
			}
			err = sign(dir, keyPath, "release-2026")
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "manifest created_at is not RFC3339") {
					t.Fatalf("invalid timestamp error = %v", err)
				}
				unchanged, readErr := os.ReadFile(manifestPath)
				if readErr != nil || string(unchanged) != string(body) {
					t.Fatal("invalid timestamp changed the manifest")
				}
				if _, statErr := os.Lstat(filepath.Join(dir, "manifest.json.sig")); !os.IsNotExist(statErr) {
					t.Fatal("invalid timestamp created a signature")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			signed, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var decoded validatorManifest
			if err := json.Unmarshal(signed, &decoded); err != nil {
				t.Fatal(err)
			}
			canonical, err := trust.Canonicalize(decoded)
			if err != nil {
				t.Fatal(err)
			}
			signature, err := os.ReadFile(filepath.Join(dir, "manifest.json.sig"))
			if err != nil {
				t.Fatal(err)
			}
			registry, err := trust.NewRegistry(trust.Anchor{KeyID: "release-2026", PublicKey: public})
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.Verify("release-2026", canonical, string(signature), timeNow()); err != nil {
				t.Fatalf("validator timestamp representation rejects signer output: %v", err)
			}
		})
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
