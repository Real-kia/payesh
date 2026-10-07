package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func runCutoverCLI(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := cutoverCommandTo(ctx, &out, args)
	return out.String(), err
}

func decodeCLI(t *testing.T, text string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &value); err != nil {
		t.Fatalf("not JSON: %q: %v", text, err)
	}
	return value
}

func TestCutoverIdentityAndGrantCommandsValidateAndProtectFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	out, err := runCutoverCLI(t, ctx, "identity", "--dir", filepath.Join(dir, "id"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, _ := decodeCLI(t, out)["fingerprint"].(string)
	if len(fingerprint) != 64 {
		t.Fatalf("identity output=%q", out)
	}
	if _, err := runCutoverCLI(t, ctx, "identity", "--dir", filepath.Join(dir, "id")); err == nil {
		t.Fatal("an existing identity was overwritten")
	}
	grants := filepath.Join(dir, "grants.json")
	args := []string{"grant", "--grants-file", grants, "--server-id", "server-0123456789", "--cutover-id", "cutover-0001", "--client-fingerprint", fingerprint}
	if _, err := runCutoverCLI(t, ctx, args...); err != nil {
		t.Fatal(err)
	}
	if _, err := runCutoverCLI(t, ctx, append(args[:len(args)-1:len(args)-1], "nope")...); err == nil {
		t.Fatal("a malformed fingerprint was accepted")
	}
	if _, err := runCutoverCLI(t, ctx, append(args, "--ttl", "72h")...); err == nil {
		t.Fatal("a grant longer than the maximum lifetime was accepted")
	}
	if _, err := runCutoverCLI(t, ctx, "bogus"); err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
}

// The complete operator workflow through the command layer: the destination
// generates a certificate and serves; the source makes an identity; the
// destination grants that identity one cutover; the source runs it.
func TestCutoverCommandsMoveAServerEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	sourceDB, destDB := filepath.Join(dir, "source.db"), filepath.Join(dir, "dest.db")
	server := contracts.Server{ID: "server-0123456789", Name: "node", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "never-connected", FreshnessState: "unknown"}
	store, err := monitoring.OpenStore(ctx, sourceDB, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	batch := make([]contracts.MetricSample, 0, 8)
	for sequence := uint64(0); sequence < 8; sequence++ {
		at := base.Add(time.Duration(sequence) * time.Second)
		batch = append(batch, contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "cli-epoch-0000001", Sequence: sequence, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": float64(sequence)}})
	}
	if _, err := store.IngestSamples(ctx, server.ID, batch, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	identity, err := runCutoverCLI(t, ctx, "identity", "--dir", filepath.Join(dir, "client"))
	if err != nil {
		t.Fatal(err)
	}
	client := decodeCLI(t, identity)

	type ready struct{ addr, pin string }
	readyCh := make(chan ready, 1)
	var once sync.Once
	cutoverServeReady = func(addr, pin string) { once.Do(func() { readyCh <- ready{addr, pin} }) }
	defer func() { cutoverServeReady = nil }()
	served := make(chan error, 1)
	go func() {
		_, err := runCutoverCLI(t, ctx, "serve", "--db", destDB, "--listen", "127.0.0.1:0", "--generate-tls", "--tls-cert", filepath.Join(dir, "srv-cert.pem"),
			"--tls-key", filepath.Join(dir, "srv-key.pem"), "--grants-file", filepath.Join(dir, "grants.json"), "--spool-dir", filepath.Join(dir, "spool"))
		served <- err
	}()
	var peer ready
	select {
	case peer = <-readyCh:
	case err := <-served:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the peer did not start")
	}

	// An earlier grant for a different cutover must not shadow the one that follows.
	if _, err := runCutoverCLI(t, ctx, "grant", "--grants-file", filepath.Join(dir, "grants.json"), "--server-id", string(server.ID),
		"--cutover-id", "cutover-0002", "--client-fingerprint", client["fingerprint"].(string)); err != nil {
		t.Fatal(err)
	}
	if _, err := runCutoverCLI(t, ctx, "grant", "--grants-file", filepath.Join(dir, "grants.json"), "--server-id", string(server.ID),
		"--cutover-id", "cutover-0001", "--client-fingerprint", client["fingerprint"].(string)); err != nil {
		t.Fatal(err)
	}
	runArgs := []string{"run", "--db", sourceDB, "--server-id", string(server.ID), "--cutover-id", "cutover-0001", "--source-id", "owner-hub-aaaa", "--destination-id", "owner-hub-bbbb",
		"--source-role", "standalone", "--destination-role", "node", "--peer-url", "https://" + peer.addr, "--peer-pin", peer.pin,
		"--client-cert", client["certificate_file"].(string), "--client-key", client["key_file"].(string), "--state-dir", filepath.Join(dir, "state")}
	out, err := runCutoverCLI(t, ctx, runArgs...)
	if err != nil {
		t.Fatalf("cutover run: %v\n%s", err, out)
	}
	if record := decodeCLI(t, out); record["phase"] != "completed" {
		t.Fatalf("run record=%v", record)
	}
	status, err := runCutoverCLI(t, ctx, "status", "--journal", filepath.Join(dir, "state", "cutover-journal.db"), "--cutover-id", "cutover-0001")
	if err != nil || decodeCLI(t, status)["phase"] != "completed" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	// A wrong pin must stop the run before anything is sent.
	bad := append([]string(nil), runArgs...)
	for i, arg := range bad {
		if arg == "--peer-pin" {
			bad[i+1] = strings.Repeat("0", 64)
		}
		if arg == "--cutover-id" {
			bad[i+1] = "cutover-0002"
		}
	}
	if _, err := runCutoverCLI(t, ctx, bad...); err == nil {
		t.Fatal("a run trusted a server whose certificate did not match the pin")
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve returned %v after shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop on shutdown")
	}
	dest, err := monitoring.OpenStore(context.Background(), destDB, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	authority, found, err := dest.GetServerAuthority(context.Background(), server.ID)
	if err != nil || !found || authority.State != monitoring.AuthorityActive || authority.Owner != "owner-hub-bbbb" {
		t.Fatalf("destination authority=%+v found=%v err=%v", authority, found, err)
	}
}
