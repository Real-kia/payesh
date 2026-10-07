package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapSignatureBindsReleaseKeyAndExactIndex(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	script := []byte("#!/bin/sh\nexit 0\n")
	digest := sha256.Sum256(script)
	sums := []byte(hex.EncodeToString(digest[:]) + "  install.sh\n")
	payload, err := BootstrapPayload("1.2.3", "test-release", sums)
	if err != nil {
		t.Fatal(err)
	}
	sig := []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload)))
	if err := VerifyBootstrap(public, "1.2.3", "test-release", sums, sig); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksumFile(sums, "install.sh", script); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, version, key string
		sums, sig          []byte
	}{
		{"replay", "1.2.4", "test-release", sums, sig},
		{"wrong-key", "1.2.3", "other-key", sums, sig},
		{"changed-index", "1.2.3", "test-release", append([]byte("0"), sums[1:]...), sig},
		{"trailing-signature-whitespace", "1.2.3", "test-release", sums, append(append([]byte(nil), sig...), '\n')},
		{"missing-signature", "1.2.3", "test-release", sums, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if VerifyBootstrap(public, tc.version, tc.key, tc.sums, tc.sig) == nil {
				t.Fatal("unauthenticated index accepted")
			}
		})
	}
	if VerifyChecksumFile(sums, "install.sh", append(script, 'x')) == nil {
		t.Fatal("tampered script accepted")
	}
	if _, err := ParseChecksums(append(append([]byte(nil), sums...), sums...)); err == nil {
		t.Fatal("duplicate checksum accepted")
	}
}

func TestProductionAnchorRejectsWritableDirectory(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "anchor.pub")
	if err := os.WriteFile(key, pub, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProductionPublicKey(key); err != nil {
		t.Fatalf("protected anchor rejected: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := ReadProductionPublicKey(key); err == nil {
		t.Fatal("replaceable public anchor accepted")
	}
}
