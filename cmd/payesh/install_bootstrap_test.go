package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Real-kia/payesh/internal/release"
)

// A disposable command fixture runs the real shell bootstrap without touching
// system services. The signed installer writes only a temp marker.
func TestShellBootstrapAuthenticatesBeforeExecutingInstaller(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("OpenSSL unavailable")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"authentic", "tampered", "different-signed-generation", "schema-unaware-installer"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			assets := filepath.Join(dir, "assets")
			os.Mkdir(bin, 0o755)
			os.Mkdir(assets, 0o755)
			write := func(path string, body []byte, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, body, mode); err != nil {
					t.Fatal(err)
				}
			}
			der, _ := x509.MarshalPKIXPublicKey(pub)
			key := filepath.Join(dir, "release.pub")
			write(key, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)
			write(filepath.Join(bin, "uname"), []byte("#!/bin/sh\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; esac\n"), 0o755)
			write(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 0\n"), 0o755)
			write(filepath.Join(bin, "stat"), []byte("#!/bin/sh\ncase \"$2\" in %u) echo 0;; %a) echo 644;; esac\n"), 0o755)
			write(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in -o) dest=$2; shift 2;; *) url=$1; shift;; esac; done\ncp \"$TEST_ASSETS/${url##*/}\" \"$dest\"\n"), 0o755)
			var sums []byte
			for _, name := range []string{"payesh-install", "payesh"} {
				body := []byte("#!/bin/sh\nexit 0\n")
				if name == "payesh-install" {
					body = []byte("#!/bin/sh\nif [ \"$1\" = --check-schema ]; then exit 0; fi\nprintf installed >\"$TEST_MARKER\"\nprintf '{}\\n'\n")
					if mode == "schema-unaware-installer" {
						body = []byte("#!/bin/sh\nif [ \"$1\" = --check-schema ]; then exit 2; fi\nprintf installed >\"$TEST_MARKER\"\nprintf '{}\\n'\n")
					}
				}
				var b bytes.Buffer
				gz := gzip.NewWriter(&b)
				tw := tar.NewWriter(gz)
				tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
				tw.Write(body)
				tw.Close()
				gz.Close()
				filename := name + "-linux-amd64.tar.gz"
				write(filepath.Join(assets, filename), b.Bytes(), 0o644)
				digest := sha256.Sum256(b.Bytes())
				sums = append(sums, []byte(hex.EncodeToString(digest[:])+"  "+filename+"\n")...)
			}
			payload, _ := release.BootstrapPayload("1.2.3", "test-release", sums)
			sig := []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload)))
			expectedIndex := sha256.Sum256(sums)
			if mode == "tampered" || mode == "different-signed-generation" {
				if sums[0] == '0' {
					sums[0] = '1'
				} else {
					sums[0] = '0'
				}
			}
			if mode == "different-signed-generation" {
				payload, _ := release.BootstrapPayload("1.2.3", "test-release", sums)
				sig = []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload)))
			}
			write(filepath.Join(assets, "SHA256SUMS"), sums, 0o644)
			write(filepath.Join(assets, "SHA256SUMS.sig"), sig, 0o644)
			marker := filepath.Join(dir, "installed")
			cmd := exec.Command("/bin/sh", "../../install.sh", "--role", "cli-only", "--version", "1.2.3", "--release-public-key", key, "--release-key-id", "test-release", "--release-checksums-sha256", hex.EncodeToString(expectedIndex[:]))
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ASSETS="+assets, "TEST_MARKER="+marker, "PAYESH_RELEASE_MODE=production", "GITHUB_TOKEN=")
			out, err := cmd.CombinedOutput()
			_, markerErr := os.Stat(marker)
			if mode == "schema-unaware-installer" {
				if err == nil || !os.IsNotExist(markerErr) || !strings.Contains(string(out), "candidate database schema preflight") {
					t.Fatalf("schema-unaware installer ran: err=%v marker=%v out=%s", err, markerErr, out)
				}
			} else if mode == "different-signed-generation" {
				if err == nil || !os.IsNotExist(markerErr) || !strings.Contains(string(out), "metadata changed after authenticated preflight") {
					t.Fatalf("different signed generation accepted: err=%v marker=%v out=%s", err, markerErr, out)
				}
			} else if mode == "tampered" {
				if err == nil || !os.IsNotExist(markerErr) || !strings.Contains(string(out), "signature verification failed") {
					t.Fatalf("tamper err=%v marker=%v output=%s", err, markerErr, out)
				}
			} else if err != nil || markerErr != nil {
				t.Fatalf("authentic err=%v marker=%v output=%s", err, markerErr, out)
			}
		})
	}
}

func TestLegacyUpdateWithoutTrustAnchorDefaultsToPreview(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	assets := filepath.Join(dir, "assets")
	os.Mkdir(bin, 0o755)
	os.Mkdir(assets, 0o755)
	write := func(path string, body []byte, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, body, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "uname"), []byte("#!/bin/sh\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; esac\n"), 0o755)
	write(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 0\n"), 0o755)
	write(filepath.Join(bin, "stat"), []byte("#!/bin/sh\ncase \"$2\" in %u) echo 0;; %a) echo 644;; esac\n"), 0o755)
	write(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in -o) dest=$2; shift 2;; *) url=$1; shift;; esac; done\ncp \"$TEST_ASSETS/${url##*/}\" \"$dest\"\n"), 0o755)
	var sums []byte
	for _, name := range []string{"payesh-install", "payesh"} {
		body := []byte("#!/bin/sh\nif [ \"$1\" = --check-schema ]; then exit 0; fi\nprintf installed >\"$TEST_MARKER\"\nprintf '{}\\n'\n")
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write(body)
		tw.Close()
		gz.Close()
		filename := name + "-linux-amd64.tar.gz"
		write(filepath.Join(assets, filename), b.Bytes(), 0o644)
		digest := sha256.Sum256(b.Bytes())
		sums = append(sums, []byte(hex.EncodeToString(digest[:])+"  "+filename+"\n")...)
	}
	write(filepath.Join(assets, "SHA256SUMS"), sums, 0o644)
	stateFile := filepath.Join(dir, "install-state.json")
	write(stateFile, []byte(`{"role":"cli-only"}`), 0o644)
	marker := filepath.Join(dir, "installed")

	cmd := exec.Command("/bin/sh", "../../install.sh", "--role", "cli-only", "--version", "1.2.3")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ASSETS="+assets, "TEST_MARKER="+marker, "PAYESH_STATE_FILE="+stateFile, "PAYESH_RELEASE_MODE=", "PAYESH_RELEASE_PUBLIC_KEY=", "PAYESH_RELEASE_KEY_ID=", "GITHUB_TOKEN=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("legacy update failed: err=%v output=%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected marker to be written: %v", err)
	}
}

func TestFreshInstallExplicitProductionRequiresTrustAnchor(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	write := func(path string, body []byte, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, body, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "uname"), []byte("#!/bin/sh\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; esac\n"), 0o755)
	write(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 0\n"), 0o755)

	cmd := exec.Command("/bin/sh", "../../install.sh", "--role", "cli-only", "--version", "1.2.3", "--release-mode", "production")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PAYESH_STATE_FILE="+filepath.Join(dir, "nonexistent"), "PAYESH_RELEASE_MODE=", "PAYESH_RELEASE_PUBLIC_KEY=", "PAYESH_RELEASE_KEY_ID=", "GITHUB_TOKEN=")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "production releases require an externally configured") {
		t.Fatalf("expected failure for unauthenticated explicit production install: err=%v output=%s", err, out)
	}
}
