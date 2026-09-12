package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func testArchive(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	tarWriter := tar.NewWriter(gz)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "bin/payesh", Typeflag: tar.TypeReg, Mode: 0o755, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func testArtifact(body []byte, url string) contracts.ReleaseArtifact {
	sum := sha256.Sum256(body)
	return contracts.ReleaseArtifact{Name: "payesh", OS: "linux", Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), CompressedBytes: uint64(len(body)), UnpackedBytes: 5, URL: url}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownloadArtifactStreamsAndCommitsOnlyVerifiedBytes(t *testing.T) {
	body := []byte("verified artifact")
	artifact := testArtifact(body, "https://example.invalid/artifact")
	destination := filepath.Join(t.TempDir(), "nested", "artifact")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Header: make(http.Header), Request: r}, nil
	})}
	if err := DownloadArtifact(context.Background(), artifact, destination, DownloadOptions{Client: client, RequiredFree: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != string(body) {
		t.Fatalf("downloaded=%q err=%v", got, err)
	}
	if mode := fileMode(t, destination); mode != 0o600 {
		t.Fatalf("mode=%o, want 600", mode)
	}
}

func TestDownloadArtifactRejectsOversizedResponseAndLeavesNoFile(t *testing.T) {
	body := []byte("small")
	artifact := testArtifact(body, "https://example.invalid/artifact")
	destination := filepath.Join(t.TempDir(), "artifact")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader([]byte("too large"))), ContentLength: -1, Header: make(http.Header), Request: r}, nil
	})}
	if err := DownloadArtifact(context.Background(), artifact, destination, DownloadOptions{Client: client, RequiredFree: 1}); err == nil {
		t.Fatal("expected size mismatch")
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination exists after rejection: %v", err)
	}
}

func TestDownloadArtifactTimeout(t *testing.T) {
	body := []byte("slow")
	artifact := testArtifact(body, "https://example.invalid/artifact")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	err := DownloadArtifact(context.Background(), artifact, filepath.Join(t.TempDir(), "artifact"), DownloadOptions{Client: client, Timeout: 5 * time.Millisecond, RequiredFree: 1})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

func TestStageArtifactExtractsAndRejectsLinks(t *testing.T) {
	archive := testArchive(t)
	root := t.TempDir()
	archivePath := filepath.Join(root, "archive.tgz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := testArtifact(archive, "https://example.invalid/archive")
	destination := filepath.Join(root, "releases", "candidate")
	if err := StageArtifact(artifact, archivePath, destination, 0, 1); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "bin", "payesh"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("staged data=%q err=%v", data, err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
