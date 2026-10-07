package cutoverpeer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGeneratedCredentialsRoundTripWithStrictModes(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "client-cert.pem"), filepath.Join(dir, "client-key.pem")
	fingerprint, err := WriteCredentials(certPath, keyPath, "payesh-cutover-client", time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !hexDigest.MatchString(fingerprint) {
		t.Fatalf("fingerprint=%q", fingerprint)
	}
	info, err := os.Stat(keyPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode=%v err=%v", info.Mode().Perm(), err)
	}
	certificate, loaded, err := LoadCredentials(certPath, keyPath)
	if err != nil || loaded != fingerprint || len(certificate.Certificate) != 1 {
		t.Fatalf("loaded=%q want %q err=%v", loaded, fingerprint, err)
	}
	if _, err := WriteCredentials(certPath, keyPath, "again", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("existing credentials were overwritten")
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadCredentials(certPath, keyPath); err == nil {
		t.Fatal("a world-readable private key was accepted")
	}
}

func TestGrantFileAddsValidatesPrunesAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grants.json")
	if grants, err := LoadGrants(path); err != nil || len(grants) != 0 {
		t.Fatalf("a missing grant file must mean no grants: %v %v", grants, err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	good := Grant{ClientFingerprint: strings.Repeat("a", 64), ServerID: "server-0123456789", CutoverID: "cutover-0001", ExpiresAt: now.Add(time.Hour)}
	if err := AddGrant(path, good, now); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("grant file mode=%v err=%v", info.Mode().Perm(), err)
	}
	for name, mutate := range map[string]func(*Grant){
		"bad-fingerprint": func(g *Grant) { g.ClientFingerprint = "xyz" },
		"bad-cutover":     func(g *Grant) { g.CutoverID = "x" },
		"no-server":       func(g *Grant) { g.ServerID = "" },
		"already-expired": func(g *Grant) { g.ExpiresAt = now.Add(-time.Second) },
		"too-long":        func(g *Grant) { g.ExpiresAt = now.Add(48 * time.Hour) },
	} {
		bad := good
		mutate(&bad)
		if err := AddGrant(path, bad, now); err == nil {
			t.Fatalf("%s: an invalid grant was accepted", name)
		}
	}
	stale := Grant{ClientFingerprint: strings.Repeat("b", 64), ServerID: "server-0123456789", CutoverID: "cutover-0002", ExpiresAt: now.Add(time.Minute)}
	if err := AddGrant(path, stale, now); err != nil {
		t.Fatal(err)
	}
	// Adding after the first grant expired prunes it; the file never accumulates dead grants.
	later := now.Add(2 * time.Hour)
	fresh := Grant{ClientFingerprint: strings.Repeat("c", 64), ServerID: "server-0123456789", CutoverID: "cutover-0003", ExpiresAt: later.Add(time.Hour)}
	if err := AddGrant(path, fresh, later); err != nil {
		t.Fatal(err)
	}
	grants, err := LoadGrants(path)
	if err != nil || len(grants) != 1 || grants[0].CutoverID != "cutover-0003" {
		t.Fatalf("grants after pruning=%+v err=%v", grants, err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGrants(path); err == nil {
		t.Fatal("a corrupt grant file was treated as empty")
	}
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGrants(path); err == nil {
		t.Fatal("a world-readable grant file was accepted")
	}
}

func TestServerPicksUpGrantsAddedWhileRunning(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t, "cutover-0010")
	path := filepath.Join(t.TempDir(), "grants.json")
	f.peerServer.Grants = nil
	f.peerServer.GrantsFunc = func() ([]Grant, error) { return LoadGrants(path) }
	if _, _, err := f.client().GetAuthority(ctx, f.server.ID); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a client with no grant was accepted: %v", err)
	}
	if err := AddGrant(path, Grant{ClientFingerprint: f.clientPrint, ServerID: f.server.ID, CutoverID: "cutover-0010", ExpiresAt: f.now.Add(time.Hour)}, f.now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.client().GetAuthority(ctx, f.server.ID); err != nil {
		t.Fatalf("a grant added while serving was not honoured: %v", err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.client().GetAuthority(ctx, f.server.ID); err == nil {
		t.Fatal("a corrupt grant file must fail closed, not grant access")
	}
}

func TestANewGrantSupersedesTheClientsPreviousGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grants.json")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	client := strings.Repeat("d", 64)
	first := Grant{ClientFingerprint: client, ServerID: "server-0123456789", CutoverID: "cutover-0001", ExpiresAt: now.Add(time.Hour)}
	other := Grant{ClientFingerprint: strings.Repeat("e", 64), ServerID: "server-0123456789", CutoverID: "cutover-0001", ExpiresAt: now.Add(time.Hour)}
	second := Grant{ClientFingerprint: client, ServerID: "server-0123456789", CutoverID: "cutover-0002", ExpiresAt: now.Add(time.Hour)}
	for _, grant := range []Grant{first, other, second} {
		if err := AddGrant(path, grant, now); err != nil {
			t.Fatal(err)
		}
	}
	grants, err := LoadGrants(path)
	if err != nil || len(grants) != 2 {
		t.Fatalf("grants=%+v err=%v", grants, err)
	}
	for _, grant := range grants {
		if grant.ClientFingerprint == client && grant.CutoverID != "cutover-0002" {
			t.Fatalf("the superseded grant for cutover %s survived", grant.CutoverID)
		}
	}
}

func TestAGrantFileWithTwoGrantsForOneClientFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grants.json")
	client := strings.Repeat("f", 64)
	body := `[{"client_fingerprint":"` + client + `","server_id":"server-0123456789","cutover_id":"cutover-0001","expires_at":"2099-01-01T00:00:00Z"},
{"client_fingerprint":"` + client + `","server_id":"server-0123456789","cutover_id":"cutover-0002","expires_at":"2099-01-01T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGrants(path); err == nil {
		t.Fatal("an ambiguous grant file (two grants for one client) was accepted")
	}
}
