package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/release"
	"github.com/Real-kia/payesh/internal/trust"
	"github.com/Real-kia/payesh/internal/updater"
	"github.com/Real-kia/payesh/internal/webupdate"
)

type trustRoundTripper func(*http.Request) (*http.Response, error)

func (f trustRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdateAuthenticatesInstallerBeforeReturningExecutableBytes(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "release.pub")
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYESH_RELEASE_MODE", "production")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", key)
	t.Setenv("PAYESH_RELEASE_KEY_ID", "test-release")
	script := []byte("#!/bin/sh\nexit 0\n")
	digest := sha256.Sum256(script)
	sums := []byte(hex.EncodeToString(digest[:]) + "  install.sh\n")
	payload, _ := release.BootstrapPayload("1.2.3", "test-release", sums)
	sig := []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload)))
	for _, tc := range []struct {
		name              string
		script, sums, sig []byte
		target            string
		ok                bool
	}{
		{"authentic", script, sums, sig, "1.2.3", true},
		{"tampered-script", append(append([]byte(nil), script...), 'x'), sums, sig, "1.2.3", false},
		{"tampered-checksums", script, append([]byte("0"), sums[1:]...), sig, "1.2.3", false},
		{"missing-signature", script, sums, nil, "1.2.3", false},
		{"replayed-version", script, sums, sig, "1.2.4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: trustRoundTripper(func(r *http.Request) (*http.Response, error) {
				var body []byte
				switch filepath.Base(r.URL.Path) {
				case "install.sh":
					body = tc.script
				case "SHA256SUMS":
					body = tc.sums
				case "SHA256SUMS.sig":
					body = tc.sig
				default:
					t.Fatalf("unexpected request %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
			})}
			body, err := fetchInstaller(context.Background(), client, "", tc.target)
			if tc.ok {
				if err != nil || string(body) != string(script) {
					t.Fatalf("body=%q error=%v", body, err)
				}
			} else if err == nil || body != nil {
				t.Fatal("unauthenticated executable returned")
			}
		})
	}
}

func TestProductionMissingAnchorFailsBeforeNetwork(t *testing.T) {
	t.Setenv("PAYESH_RELEASE_MODE", "production")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", "")
	t.Setenv("PAYESH_RELEASE_KEY_ID", "")
	client := &http.Client{Transport: trustRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("network used before validating anchor")
		return nil, nil
	})}
	if _, err := fetchInstaller(context.Background(), client, "", "1.2.3"); err == nil {
		t.Fatal("missing anchor accepted")
	}
	t.Setenv("PAYESH_RELEASE_MODE", "preview")
	if p, err := releaseTrustFromEnvironment(); err != nil || p.mode != "preview" {
		t.Fatalf("explicit preview: %+v %v", p, err)
	}
}

func TestDefaultReleaseTrustIsPreviewWithoutAnchor(t *testing.T) {
	t.Setenv("PAYESH_RELEASE_MODE", "")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", "")
	t.Setenv("PAYESH_RELEASE_KEY_ID", "")
	t.Setenv("PAYESH_UPDATE_ENV", filepath.Join(t.TempDir(), "nonexistent.env"))
	p, err := releaseTrustFromEnvironment()
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if p.mode != "preview" {
		t.Fatalf("expected preview mode, got: %s", p.mode)
	}
}

func productionCandidateFixture(t *testing.T, schema *contracts.ReleaseDatabaseSchema) (map[string][]byte, *http.Client, releaseTrustPolicy, contracts.ReleaseManifest, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "release.pub")
	if err := os.WriteFile(key, pub, 0600); err != nil {
		t.Fatal(err)
	}
	policy := releaseTrustPolicy{mode: "production", publicKey: key, keyID: "test-release"}
	script, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := contracts.ReleaseManifest{Format: contracts.ReleaseFormat, Release: "1.2.3", MinCore: "1.0.0", CreatedAt: time.Now().UTC().Truncate(time.Second), SigningKeyID: policy.keyID, DatabaseSchema: schema, Artifacts: []contracts.ReleaseArtifact{{Name: "payesh", OS: "linux", Arch: "amd64", SHA256: strings.Repeat("a", 64), CompressedBytes: 1, UnpackedBytes: 1, URL: "https://example.invalid/payesh-linux-amd64.tar.gz"}}}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	_, sig, err := trust.Sign(priv, m)
	if err != nil {
		t.Fatal(err)
	}
	mh, sh := sha256.Sum256(body), sha256.Sum256(script)
	lines := []string{hex.EncodeToString(mh[:]) + "  manifest.json", hex.EncodeToString(sh[:]) + "  install.sh", m.Artifacts[0].SHA256 + "  payesh-linux-amd64.tar.gz"}
	sort.Strings(lines)
	sums := []byte(strings.Join(lines, "\n") + "\n")
	payload, err := release.BootstrapPayload(m.Release, policy.keyID, sums)
	if err != nil {
		t.Fatal(err)
	}
	assets := map[string][]byte{"manifest.json": body, "manifest.json.sig": []byte(sig), "install.sh": script, "SHA256SUMS": sums, "SHA256SUMS.sig": []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload)))}
	client := &http.Client{Transport: trustRoundTripper(func(r *http.Request) (*http.Response, error) {
		body, ok := assets[filepath.Base(r.URL.Path)]
		if !ok {
			return nil, fmt.Errorf("unexpected candidate asset %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	return assets, client, policy, m, priv
}
func TestPreparedCandidateRejectsIncompatibleSchemaBeforeActualWorkerPrepare(t *testing.T) {
	for _, schema := range []*contracts.ReleaseDatabaseSchema{nil, {MinReadable: 1, Current: 5}, {MinReadable: 7, Current: 8}, {MinReadable: 7, Current: 6}} {
		t.Run(fmt.Sprintf("schema=%v", schema), func(t *testing.T) {
			_, client, policy, m, _ := productionCandidateFixture(t, schema)
			t.Setenv("PAYESH_RELEASE_MODE", policy.mode)
			t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", policy.publicKey)
			t.Setenv("PAYESH_RELEASE_KEY_ID", policy.keyID)
			dbpath := filepath.Join(t.TempDir(), "store.sqlite")
			db, err := sql.Open("sqlite", dbpath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE schema_meta(version INTEGER NOT NULL);INSERT INTO schema_meta VALUES(6)`); err != nil {
				t.Fatal(err)
			}
			db.Close()
			before, _ := os.ReadFile(dbpath)
			dir := t.TempDir()
			req := webupdate.Request{JobID: "signed-schema-refusal", Version: m.Release, Deadline: time.Now().Add(time.Minute)}
			if err := webupdate.SubmitFleet(dir, req, time.Now()); err != nil {
				t.Fatal(err)
			}
			prepared := false
			done, err := runFleetUpdateWithHooks(t.Context(), dir, req, func(context.Context, webupdate.InstallationTransaction) (bool, error) { return false, nil }, func(webupdate.InstallationTransaction) (string, error) { return "", os.ErrNotExist }, fleetUpdateHooks{Scope: func() (webupdate.InstallationScope, error) {
				return webupdate.InstallationScope{Role: "hub", Init: "openrc"}, nil
			}, CandidatePreflight: func(ctx context.Context, target string) error {
				_, err := prepareProductionRelease(ctx, client, "", target, "1.0.0", policy, dbpath)
				return err
			}, Prepare: func(context.Context) error {
				prepared = true
				return fmt.Errorf("unexpected snapshot/service boundary")
			}})
			if done || err == nil || prepared {
				t.Fatalf("incompatible signed candidate crossed snapshot boundary: done=%v prepared=%v err=%v", done, prepared, err)
			}
			after, _ := os.ReadFile(dbpath)
			if string(before) != string(after) {
				t.Fatal("preflight mutated database")
			}
			result, err := webupdate.FleetResult(dir, req.JobID)
			if err != nil || result.State != webupdate.StateFailed {
				t.Fatalf("missing durable refusal: %+v %v", result, err)
			}
		})
	}
}
func TestPreparedCandidateBindsManifestIndexScriptAndSignature(t *testing.T) {
	for _, mode := range []string{"authentic", "tampered-manifest", "wrong-manifest-signature", "removed-schema", "different-archive-generation", "legacy-installer"} {
		t.Run(mode, func(t *testing.T) {
			assets, client, policy, m, priv := productionCandidateFixture(t, &contracts.ReleaseDatabaseSchema{MinReadable: 1, Current: 6})
			rebind := func(name string, body []byte) {
				assets[name] = body
				entries, _ := release.ParseChecksums(assets["SHA256SUMS"])
				digest := sha256.Sum256(body)
				entries[name] = hex.EncodeToString(digest[:])
				lines := []string{}
				for name, digest := range entries {
					lines = append(lines, digest+"  "+name)
				}
				sort.Strings(lines)
				assets["SHA256SUMS"] = []byte(strings.Join(lines, "\n") + "\n")
				payload, _ := release.BootstrapPayload(m.Release, policy.keyID, assets["SHA256SUMS"])
				assets["SHA256SUMS.sig"] = []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload)))
			}
			switch mode {
			case "tampered-manifest":
				assets["manifest.json"] = []byte(strings.Replace(string(assets["manifest.json"]), `"current":"6"`, `"current":"7"`, 1))
			case "wrong-manifest-signature":
				rebind("manifest.json", []byte(strings.Replace(string(assets["manifest.json"]), `"current":"6"`, `"current":"7"`, 1)))
			case "removed-schema":
				legacy := m
				legacy.DatabaseSchema = nil
				body, err := json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				rebind("manifest.json", body)
			case "different-archive-generation":
				rebind("payesh-linux-amd64.tar.gz", []byte("new generation"))
			case "legacy-installer":
				rebind("install.sh", []byte("#!/bin/sh\nexit 0\n"))
			}
			prepared, err := prepareProductionRelease(t.Context(), client, "", m.Release, "1.0.0", policy, filepath.Join(t.TempDir(), "absent.sqlite"))
			if mode == "authentic" {
				if err != nil || prepared.Manifest.DatabaseSchema.Current != 6 || len(prepared.ChecksumsSHA256) != 64 {
					t.Fatalf("authentic candidate refused: %+v %v", prepared, err)
				}
			} else if err == nil {
				t.Fatal("unbound candidate accepted")
			}
			if mode == "authentic" {
				registry, _ := trust.NewRegistry(trust.Anchor{KeyID: policy.keyID, PublicKey: priv.Public().(ed25519.PublicKey)})
				legacy := m
				legacy.DatabaseSchema = nil
				_, sig, _ := trust.Sign(priv, legacy)
				if err := updater.VerifyManifest(registry, legacy, sig, "1.0.0", updater.AcceptedState{}, time.Now()); err != nil {
					t.Fatalf("generic legacy verification lost: %v", err)
				}
			}
		})
	}
}

func TestCLIProductionInstallerRefusesCandidateSchemaBeforeScriptExecution(t *testing.T) {
	_, client, policy, m, _ := productionCandidateFixture(t, &contracts.ReleaseDatabaseSchema{MinReadable: 1, Current: 5})
	dbpath := filepath.Join(t.TempDir(), "store.sqlite")
	db, err := sql.Open("sqlite", dbpath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_meta(version INTEGER NOT NULL);INSERT INTO schema_meta VALUES(6)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	err = runReleaseInstallerWithClient(t.Context(), m.Release, "", policy, client, "1.0.0", dbpath, "--role", "hub")
	if err == nil || !strings.Contains(err.Error(), "outside candidate readable range") {
		t.Fatalf("CLI production preflight skipped: %v", err)
	}
}

func TestAnchorOwnerAllowed(t *testing.T) {
	for _, tc := range []struct {
		owner uint32
		euid  int
		want  bool
	}{{0, 0, true}, {0, 1000, true}, {1000, 1000, true}, {1001, 1000, false}, {1001, 0, false}, {33, 0, false}} {
		if got := anchorOwnerAllowed(tc.owner, tc.euid); got != tc.want {
			t.Errorf("anchorOwnerAllowed(%d,%d)=%v want %v", tc.owner, tc.euid, got, tc.want)
		}
	}
}

func TestReleaseAnchorRejectsWritableDirectory(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "release.pub")
	if err := os.WriteFile(key, pub, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYESH_RELEASE_MODE", "production")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", key)
	t.Setenv("PAYESH_RELEASE_KEY_ID", "test-release")
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseTrustFromEnvironment(); err != nil {
		t.Fatalf("private anchor directory rejected: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseTrustFromEnvironment(); err == nil {
		t.Fatal("anchor in a group/world-writable directory accepted")
	}
}

func TestReleaseTrustFromUpdateEnvFile(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "release.pub")
	if err := os.WriteFile(key, pub, 0o600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "payesh-update.env")
	envContent := fmt.Sprintf("PAYESH_RELEASE_MODE=production\nPAYESH_RELEASE_PUBLIC_KEY=%s\nPAYESH_RELEASE_KEY_ID=test-release\n", key)
	if err := os.WriteFile(envFile, []byte(envContent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYESH_RELEASE_MODE", "")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", "")
	t.Setenv("PAYESH_RELEASE_KEY_ID", "")
	t.Setenv("PAYESH_UPDATE_ENV", envFile)

	policy, err := releaseTrustFromEnvironment()
	if err != nil {
		t.Fatalf("expected policy from env file: %v", err)
	}
	if policy.mode != "production" || policy.publicKey != key || policy.keyID != "test-release" {
		t.Fatalf("unexpected policy loaded: %+v", policy)
	}
}
