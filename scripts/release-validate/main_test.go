package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Real-kia/payesh/internal/release"
	"github.com/Real-kia/payesh/internal/trust"
)

func TestValidateWithExplicitAnchorAcceptsSignedBundle(t *testing.T) {
	for _, schema := range []bool{false, true} {
		t.Run(strconv.FormatBool(schema), func(t *testing.T) {
			dir := t.TempDir()
			archiveName := "payesh-linux-amd64.tar.gz"
			archivePath := filepath.Join(dir, archiveName)
			if err := writeTestArchive(archivePath); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			manifest := map[string]any{
				"format": "payesh.release.v1", "release": "1.2.3", "created_at": "2026-09-12T00:00:00Z", "min_core": "1.2.3",
				"artifacts":      []any{map[string]any{"name": "payesh", "os": "linux", "arch": "amd64", "sha256": hex.EncodeToString(sum[:]), "compressed_bytes": "", "unpacked_bytes": "", "url": archiveName}},
				"signing_key_id": "unavailable-local",
			}
			if schema {
				manifest["database_schema"] = map[string]string{"min_readable": "1", "current": "6"}
			}
			// Fill decimal-string sizes after writing the archive.
			artifact := manifest["artifacts"].([]any)[0].(map[string]any)
			// The manifest contract encodes sizes as JSON strings, not numbers.
			artifact["compressed_bytes"] = strconv.Itoa(len(body))
			artifact["unpacked_bytes"] = "4"
			manifestPath := filepath.Join(dir, "manifest.json")
			manifestBody, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, manifestBody, 0o644); err != nil {
				t.Fatal(err)
			}
			pub, private, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			manifest["signing_key_id"] = "release-2026"
			canonical, err := trust.Canonicalize(manifest)
			if err != nil {
				t.Fatal(err)
			}
			signature := ed25519.Sign(private, canonical)
			if err := os.WriteFile(manifestPath, mustJSON(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json.sig"), []byte(base64.RawURLEncoding.EncodeToString(signature)), 0o644); err != nil {
				t.Fatal(err)
			}
			bootstrap := []byte("#!/bin/sh\nexit 0\n")
			bootstrapDigest := sha256.Sum256(bootstrap)
			checksums := []byte(hex.EncodeToString(bootstrapDigest[:]) + "  install.sh\n" + hex.EncodeToString(sum[:]) + "  " + archiveName + "\n")
			if schema {
				digest := sha256.Sum256(mustJSON(manifest))
				checksums = append(checksums, []byte(hex.EncodeToString(digest[:])+"  manifest.json\n")...)
			}
			// The checksum index is sorted by complete checksum lines, as the builder does.
			lines := strings.Split(strings.TrimSuffix(string(checksums), "\n"), "\n")
			sort.Strings(lines)
			checksums = []byte(strings.Join(lines, "\n") + "\n")
			if err := os.WriteFile(filepath.Join(dir, "install.sh"), bootstrap, 0o644); err != nil {
				t.Fatal(err)
			}
			payload, _ := release.BootstrapPayload("1.2.3", "release-2026", checksums)
			bootstrapSig := []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload)))
			if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), bootstrapSig, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), checksums, 0o644); err != nil {
				t.Fatal(err)
			}
			publicKeyPath := filepath.Join(dir, "public.key")
			if err := os.WriteFile(publicKeyPath, pub, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := validateWithTrust(dir, publicKeyPath, "release-2026"); err != nil {
				t.Fatalf("signed bundle rejected: %v", err)
			}
			if err := validateWithTrust(dir, "", ""); err == nil || !strings.Contains(err.Error(), "explicit") {
				t.Fatalf("missing anchor error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), []byte(strings.Repeat("a", 86)), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := validateWithTrust(dir, publicKeyPath, "release-2026"); err == nil {
				t.Fatal("tampered bootstrap signature accepted")
			}
		})
	}
}

func writeTestArchive(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tarWriter := tar.NewWriter(gz)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "payesh", Mode: 0o755, Size: 4, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tarWriter.Write([]byte("test")); err != nil {
		return err
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

func mustJSON(value any) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}
