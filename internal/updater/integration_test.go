package updater

// These tests deliberately use only local disposable resources. They exercise
// the boundaries that unit fixtures cannot cover together: an HTTP metadata
// source, the pinned release signature, the artifact stream, activation
// journal, and a durable multi-node scheduler. No production key, SSH host,
// or remote filesystem is involved.

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/trust"
)

func TestLocalSignedHTTPReleaseExecutorRollsBackAndRecovers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	releaseRoot := filepath.Join(root, "releases")
	active := filepath.Join(releaseRoot, "current")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "version"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive := buildExecutorArchive(t, []byte("new"))
	public, private, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := trust.NewRegistry(trust.Anchor{KeyID: "local-integration", PublicKey: public})
	if err != nil {
		t.Fatal(err)
	}
	manifest := contracts.ReleaseManifest{
		Format: contracts.ReleaseFormat, Release: "1.4.0",
		CreatedAt: time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC),
		MinCore:   "1.0.0", SigningKeyID: "local-integration",
		Artifacts: []contracts.ReleaseArtifact{{Name: "payesh-agent", OS: "linux", Arch: "amd64", SHA256: sha256Hex(archive), CompressedBytes: uint64(len(archive)), UnpackedBytes: 3}},
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local HTTP listener is unavailable in this test environment: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/1.4.0/manifest.json":
			body, _ := json.Marshal(manifest)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case "/1.4.0/manifest.json.sig":
			_, signature, signErr := trust.Sign(private, manifest)
			if signErr != nil {
				http.Error(w, signErr.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, signature)
		case "/1.4.0/agent.tar.gz":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	// The manifest is encoded lazily by the handler, so bind the artifact to
	// the actual ephemeral fixture URL after Start selects the port.
	manifest.Artifacts[0].URL = server.URL + "/1.4.0/agent.tar.gz"

	var healthFails bool = true
	executor, err := NewReleaseExecutor(ExecutorConfig{
		Registry:    registry,
		Source:      HTTPReleaseSource{BaseURL: server.URL},
		CurrentCore: "1.0.0", AcceptedStatePath: filepath.Join(root, "state", "accepted.json"),
		ReleaseRoot: releaseRoot, ActiveDir: active, JournalPath: filepath.Join(root, "state", "journal.json"),
		BackupDir: filepath.Join(root, "backups"), LocalServerID: "server-local-integration", GOOS: "linux", GOARCH: "amd64",
		Download: DownloadOptions{Client: server.Client(), RequiredFree: 1},
		Backup:   func(context.Context, string) error { return nil },
		Health: func(_ context.Context, candidate string) error {
			body, readErr := os.ReadFile(filepath.Join(candidate, "bin", "payesh-agent"))
			if readErr != nil {
				return readErr
			}
			if string(body) != "new" {
				return errors.New("candidate bytes are not the requested release")
			}
			if healthFails {
				return errors.New("simulated startup failure")
			}
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 9, 12, 10, 1, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := Execution{JobID: "update-node-local-integration", Release: "1.4.0", Target: contracts.Server{ID: "server-local-integration", Role: "node", ConnectionState: "connected"}}
	if err := executor.Execute(ctx, execution); err == nil {
		t.Fatal("expected simulated health failure")
	} else if !strings.Contains(err.Error(), "simulated startup failure") {
		t.Fatalf("execution failed before the intended health/rollback boundary: %v", err)
	}
	if body, readErr := os.ReadFile(filepath.Join(active, "version")); readErr != nil || string(body) != "old" {
		t.Fatalf("failed activation did not restore old release: %q (%v)", body, readErr)
	}
	journal, err := LoadJournal(filepath.Join(root, "state", "journal.json"))
	if err != nil || journal.Phase != PhaseRolledBack {
		t.Fatalf("rollback journal=%+v err=%v", journal, err)
	}

	healthFails = false
	if err := executor.Execute(ctx, execution); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if body, readErr := os.ReadFile(filepath.Join(active, "bin", "payesh-agent")); readErr != nil || string(body) != "new" {
		t.Fatalf("recovered activation=%q err=%v", body, readErr)
	}
	accepted, err := LoadAcceptedState(filepath.Join(root, "state", "accepted.json"))
	if err != nil || accepted.Release != "1.4.0" {
		t.Fatalf("accepted state=%+v err=%v", accepted, err)
	}
	if err := executor.Execute(ctx, execution); err != nil {
		t.Fatalf("idempotent committed retry: %v", err)
	}
}

func TestLocalMultiNodeSchedulerDurableRestartAndConflict(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, filepath.Join(t.TempDir(), "fleet.db"), monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 11, 0, 0, 0, time.UTC)
	servers := []contracts.Server{
		schedulerServer("hub-local-integration", "hub", "connected"),
		schedulerServer("node-local-integration-1", "node", "connected"),
		schedulerServer("node-local-integration-2", "node", "connected"),
	}
	for _, server := range servers {
		if err := store.EnsureServer(ctx, server); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var calls []string
	interrupted := true
	executor := ExecutorFunc(func(_ context.Context, execution Execution) error {
		mu.Lock()
		calls = append(calls, string(execution.Target.ID))
		mu.Unlock()
		if execution.Target.ID == servers[1].ID && interrupted {
			interrupted = false
			return context.Canceled
		}
		return nil
	})
	scheduler := &Scheduler{Store: store, Executor: executor, Now: func() time.Time { return now }}
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.4.0", IdempotencyKey: "local-fleet-integration", Targets: []Target{{Server: servers[2], Compatible: true}, {Server: servers[1], Compatible: true}, {Server: servers[0], Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx, parent.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected worker interruption, got %v", err)
	}
	children, err := store.ListJobsByKind(ctx, UpdateNodeJobKind)
	if err != nil || len(children) != 3 {
		t.Fatalf("children=%d err=%v", len(children), err)
	}
	var running contracts.Job
	for _, child := range children {
		if child.TargetServerID == servers[1].ID {
			running = child
		}
	}
	if running.State != contracts.JobRunning {
		t.Fatalf("interrupted child=%+v", running)
	}
	// A second scheduler instance resumes the durable running child and then
	// advances exactly one remaining node on each pass.
	resumed := &Scheduler{Store: store, Executor: executor, Now: func() time.Time { return now }}
	if _, err := resumed.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	completed, found, err := store.GetJob(ctx, parent.ID)
	if err != nil || !found || completed.State != contracts.JobSucceeded {
		t.Fatalf("parent=%+v found=%v err=%v", completed, found, err)
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	want := []string{string(servers[0].ID), string(servers[1].ID), string(servers[1].ID), string(servers[2].ID)}
	if !reflect.DeepEqual(gotCalls, want) {
		t.Fatalf("execution order=%v want=%v", gotCalls, want)
	}
}
