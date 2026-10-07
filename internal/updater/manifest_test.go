package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/trust"
)

func signedManifest(t *testing.T, created time.Time, release string) (contracts.ReleaseManifest, string, *trust.Registry, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("release archive")
	sum := sha256.Sum256(body)
	m := contracts.ReleaseManifest{Format: contracts.ReleaseFormat, Release: release, CreatedAt: created, MinCore: "0.1.0", SigningKeyID: "release-test", Artifacts: []contracts.ReleaseArtifact{{Name: "payesh-agent", OS: "linux", Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), CompressedBytes: uint64(len(body)), UnpackedBytes: uint64(len(body)) + 1, URL: "https://example.invalid/payesh-agent"}}}
	_, sig, err := trust.Sign(priv, m)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := trust.NewRegistry(trust.Anchor{KeyID: "release-test", PublicKey: pub, ExpiresAt: created.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return m, sig, registry, body
}

func TestVerifyManifestAndArtifact(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	m, sig, registry, body := signedManifest(t, now, "0.2.0")
	if err := VerifyManifest(registry, m, sig, "0.1.0", AcceptedState{}, now); err != nil {
		t.Fatal(err)
	}
	a, err := SelectArtifact(m, "payesh-agent", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(a, body); err != nil {
		t.Fatal(err)
	}
	body[0] ^= 1
	if err := VerifyArtifact(a, body); err == nil {
		t.Fatal("tampered artifact accepted")
	}
}

func TestVerifyManifestRejectsReplayAndPlatformAbsence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	m, sig, registry, _ := signedManifest(t, now.Add(-time.Minute), "0.2.0")
	state := AcceptedState{Release: "0.3.0", CreatedAt: now}
	if err := VerifyManifest(registry, m, sig, "0.1.0", state, now); !errors.Is(err, ErrManifestReplay) {
		t.Fatalf("expected replay rejection, got %v", err)
	}
	if _, err := SelectArtifact(m, "payesh-agent", "linux", "arm64"); !errors.Is(err, ErrArtifactUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
}

func TestVerifyManifestRejectsCorruptAcceptedStateWithoutPanicking(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	m, sig, registry, _ := signedManifest(t, now, "0.2.0")
	err := VerifyManifest(registry, m, sig, "0.1.0", AcceptedState{Release: "broken", CreatedAt: now}, now)
	if err == nil {
		t.Fatal("corrupt durable anti-replay state was accepted")
	}
}

func TestAcceptedStateRoundTripIsRestricted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "accepted.json")
	want := AcceptedState{Release: "1.2.3", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := SaveAcceptedState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAcceptedState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestCandidateSchemaRangeRefusesExistingUnsupportedDatabaseWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		schema  *contracts.ReleaseDatabaseSchema
		version int
		ok      bool
	}{
		{"lower-bound", &contracts.ReleaseDatabaseSchema{MinReadable: 5, Current: 6}, 5, true},
		{"upper-bound", &contracts.ReleaseDatabaseSchema{MinReadable: 5, Current: 6}, 6, true},
		{"too-old", &contracts.ReleaseDatabaseSchema{MinReadable: 5, Current: 6}, 4, false},
		{"too-new", &contracts.ReleaseDatabaseSchema{MinReadable: 1, Current: 6}, 7, false},
		{"missing-declaration", nil, 6, false},
		{"invalid-range", &contracts.ReleaseDatabaseSchema{MinReadable: 7, Current: 6}, 6, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(?)`, tc.version); err != nil {
				t.Fatal(err)
			}
			db.Close()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			err = CheckCandidateDatabaseSchema(t.Context(), contracts.ReleaseManifest{DatabaseSchema: tc.schema}, path)
			if (err == nil) != tc.ok {
				t.Fatalf("compatibility err=%v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("candidate preflight changed database")
			}
		})
	}
}
func TestCandidateSchemaReadsCommittedWALAndRejectsMalformedOrMissingMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(5); PRAGMA wal_checkpoint(TRUNCATE); UPDATE schema_meta SET version=6;`); err != nil {
		t.Fatal(err)
	}
	m := contracts.ReleaseManifest{DatabaseSchema: &contracts.ReleaseDatabaseSchema{MinReadable: 6, Current: 6}}
	if err := CheckCandidateDatabaseSchema(t.Context(), m, path); err != nil {
		t.Fatalf("committed WAL ignored: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_meta VALUES(6)`); err != nil {
		t.Fatal(err)
	}
	if err := CheckCandidateDatabaseSchema(t.Context(), m, path); err == nil {
		t.Fatal("ambiguous metadata accepted")
	}
	for _, body := range []string{"", "not sqlite"} {
		p := filepath.Join(t.TempDir(), "bad.sqlite")
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := CheckCandidateDatabaseSchema(t.Context(), m, p); err == nil {
			t.Fatal("malformed existing database accepted")
		}
	}
	missing := filepath.Join(t.TempDir(), "absent.sqlite")
	if err := CheckCandidateDatabaseSchema(t.Context(), contracts.ReleaseManifest{}, missing); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("fresh preflight created database")
	}
}
func TestLegacyManifestOmissionPreservesCanonicalSignature(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	m, sig, registry, _ := signedManifest(t, now, "0.2.0")
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "database_schema") {
		t.Fatal("legacy omitted field changed wire bytes")
	}
	if err := VerifyManifest(registry, m, sig, "0.1.0", AcceptedState{}, now); err != nil {
		t.Fatal(err)
	}
	m.DatabaseSchema = &contracts.ReleaseDatabaseSchema{MinReadable: 1, Current: 6}
	if err := VerifyManifest(registry, m, sig, "0.1.0", AcceptedState{}, now); err == nil {
		t.Fatal("unauthenticated schema addition accepted")
	}
}
