package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"github.com/Real-kia/payesh/internal/trust"
)

type fixtureSource struct{ bundle ReleaseBundle }

func (s fixtureSource) Fetch(context.Context, string) (ReleaseBundle, error) { return s.bundle, nil }

func buildExecutorArchive(t *testing.T, body []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gz)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "bin/payesh-agent", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestReleaseExecutorDownloadsVerifiesStagesActivatesAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	releaseRoot := filepath.Join(root, "releases")
	active := filepath.Join(releaseRoot, "current")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "old"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := buildExecutorArchive(t, []byte("new"))
	digest := sha256Hex(body)
	manifest := contracts.ReleaseManifest{Format: contracts.ReleaseFormat, Release: "1.2.3", CreatedAt: time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC), MinCore: "1.0.0", SigningKeyID: "test", Artifacts: []contracts.ReleaseArtifact{{Name: "payesh-agent", OS: "linux", Arch: "amd64", SHA256: digest, CompressedBytes: uint64(len(body)), UnpackedBytes: 3, URL: "https://example.invalid/agent.tar.gz"}}}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := trust.NewRegistry(trust.Anchor{KeyID: "test", PublicKey: public})
	if err != nil {
		t.Fatal(err)
	}
	_, signature, err := trust.Sign(private, manifest)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 12, 10, 1, 0, 0, time.UTC)
	var backupPath string
	executor, err := NewReleaseExecutor(ExecutorConfig{
		Registry: registry, Source: fixtureSource{bundle: ReleaseBundle{Manifest: manifest, Signature: signature}}, CurrentCore: "1.0.0",
		AcceptedStatePath: filepath.Join(root, "state", "accepted.json"), ReleaseRoot: releaseRoot, ActiveDir: active,
		JournalPath: filepath.Join(root, "state", "journal.json"), BackupDir: filepath.Join(root, "backups"),
		LocalServerID: "server-executor-test01", GOOS: "linux", GOARCH: "amd64", Download: DownloadOptions{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Header: make(http.Header), Request: r}, nil
		})}, RequiredFree: 1},
		Backup: func(_ context.Context, destination string) error { backupPath = destination; return nil },
		Health: func(_ context.Context, dir string) error {
			got, readErr := os.ReadFile(filepath.Join(dir, "bin", "payesh-agent"))
			if readErr != nil {
				return readErr
			}
			if string(got) != "new" {
				return errors.New("unexpected candidate contents")
			}
			return nil
		}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := Execution{JobID: "update-node-executor-test01", Release: "1.2.3", Target: contracts.Server{ID: "server-executor-test01", Role: "node", ConnectionState: "connected"}}
	if err := executor.Execute(context.Background(), execution); err != nil {
		t.Fatal(err)
	}
	if backupPath == "" || filepath.Dir(backupPath) != filepath.Join(root, "backups") {
		t.Fatalf("backup path=%q", backupPath)
	}
	got, err := os.ReadFile(filepath.Join(active, "bin", "payesh-agent"))
	if err != nil || string(got) != "new" {
		t.Fatalf("active release=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(active, "old")); !os.IsNotExist(err) {
		t.Fatalf("old release still active, err=%v", err)
	}
	if err := executor.Execute(context.Background(), execution); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	accepted := filepath.Join(root, "state", "accepted.json")
	if err := os.Remove(accepted); err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(context.Background(), execution); err != nil {
		t.Fatalf("committed retry should repair accepted state: %v", err)
	}
	if _, err := os.Stat(accepted); err != nil {
		t.Fatalf("accepted state was not repaired: %v", err)
	}
}

func TestNewReleaseExecutorRequiresBackupAndHealth(t *testing.T) {
	root := t.TempDir()
	_, err := NewReleaseExecutor(ExecutorConfig{CurrentCore: "1.0.0", ReleaseRoot: filepath.Join(root, "releases"), ActiveDir: filepath.Join(root, "releases", "current"), JournalPath: filepath.Join(root, "journal"), AcceptedStatePath: filepath.Join(root, "accepted"), BackupDir: filepath.Join(root, "backup")})
	if err == nil {
		t.Fatal("expected required trust/source/backup/health configuration error")
	}
}

func TestReleaseExecutorRejectsRemoteTargetBeforeFetching(t *testing.T) {
	root := t.TempDir()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := trust.NewRegistry(trust.Anchor{KeyID: "test", PublicKey: public})
	if err != nil {
		t.Fatal(err)
	}
	source := fixtureSource{}
	executor, err := NewReleaseExecutor(ExecutorConfig{
		Registry: registry, Source: source, CurrentCore: "1.0.0", AcceptedStatePath: filepath.Join(root, "state", "accepted"),
		ReleaseRoot: filepath.Join(root, "releases"), ActiveDir: filepath.Join(root, "releases", "current"), JournalPath: filepath.Join(root, "state", "journal"), BackupDir: filepath.Join(root, "backups"),
		LocalServerID: "server-executor-test01",
		Backup:        func(context.Context, string) error { return nil }, Health: func(context.Context, string) error { return nil }, GOOS: "linux", GOARCH: "amd64", Now: func() time.Time { return time.Now() },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = executor.Execute(context.Background(), Execution{JobID: "update-node-remote-test01", Release: "1.2.3", Target: contracts.Server{ID: "server-different-test01", Role: "node", ConnectionState: "connected"}})
	if !errors.Is(err, ErrExecutorTarget) {
		t.Fatalf("expected target rejection, got %v", err)
	}
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
