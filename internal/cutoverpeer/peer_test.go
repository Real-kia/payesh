package cutoverpeer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/updater"
	_ "modernc.org/sqlite"
)

func selfSigned(t *testing.T, name string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:])
}

type peerFixture struct {
	source      *monitoring.Store
	sourceDB    *sql.DB
	dest        *monitoring.Store
	destDB      *sql.DB
	server      contracts.Server
	httpServer  *httptest.Server
	peerServer  *Server
	clientCert  tls.Certificate
	clientPrint string
	serverPin   string
	now         time.Time
	requests    atomic.Int64
	puts        atomic.Int64
}

func samples(server contracts.ServerID, from, to uint64) []contracts.MetricSample {
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	out := make([]contracts.MetricSample, 0, to-from)
	for sequence := from; sequence < to; sequence++ {
		at := base.Add(time.Duration(sequence) * time.Second)
		out = append(out, contracts.MetricSample{ServerID: server, CollectorEpoch: "peer-epoch-000001", Sequence: sequence, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": float64(sequence) + 0.25}})
	}
	return out
}

func newPeerFixture(t *testing.T, cutoverID string) *peerFixture {
	t.Helper()
	ctx := context.Background()
	f := &peerFixture{now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	open := func(name string) (*monitoring.Store, *sql.DB) {
		path := filepath.Join(dir, name)
		store, err := monitoring.OpenStore(ctx, path, monitoring.StoreOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return store, db
	}
	f.source, f.sourceDB = open("source.db")
	f.dest, f.destDB = open("dest.db")
	f.server = contracts.Server{ID: "server-0123456789", Name: "node", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "never-connected", FreshnessState: "unknown"}
	if err := f.source.EnsureServer(ctx, f.server); err != nil {
		t.Fatal(err)
	}
	if _, err := f.source.IngestSamples(ctx, f.server.ID, samples(f.server.ID, 0, 6), nil); err != nil {
		t.Fatal(err)
	}
	serverCert, serverPin := selfSigned(t, "peer-server")
	f.serverPin = serverPin
	f.clientCert, f.clientPrint = selfSigned(t, "peer-client")
	f.peerServer = &Server{DB: f.destDB, Store: f.dest, SpoolDir: t.TempDir(), Now: func() time.Time { return f.now },
		Grants: []Grant{{ClientFingerprint: f.clientPrint, ServerID: f.server.ID, CutoverID: cutoverID, ExpiresAt: f.now.Add(time.Hour)}}}
	handler := f.peerServer.Handler()
	f.httpServer = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method == http.MethodPut {
			f.puts.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	f.httpServer.TLS = ServerTLSConfig(serverCert)
	f.httpServer.StartTLS()
	t.Cleanup(f.httpServer.Close)
	return f
}

func (f *peerFixture) client() *Client {
	return &Client{BaseURL: f.httpServer.URL, ServerCertSHA256: f.serverPin, ClientCert: f.clientCert, ServerID: f.server.ID, ChunkSize: 1 << 10}
}

func cutoverRequest(id string) updater.RoleCutoverRequest {
	return updater.RoleCutoverRequest{ID: id, SourceRole: "standalone", DestinationRole: "node", SourceID: "owner-hub-aaaa", DestinationID: "owner-hub-bbbb"}
}

// The whole cutover runs over mutual TLS: chunked transfer, import, activation.
func TestCutoverCompletesOverTheAuthenticatedPeer(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t, "cutover-0001")
	cutover := &updater.StoreCutover{Source: f.source, SourceDB: f.sourceDB, Peer: f.client(), ServerID: f.server.ID, StateDir: t.TempDir()}
	journal, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	record, err := updater.RunRoleCutover(ctx, journal, cutoverRequest("cutover-0001"), cutover.Hooks(), func() time.Time { return f.now })
	if err != nil || record.Phase != updater.CutoverComplete {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	src, _, _ := f.source.GetServerAuthority(ctx, f.server.ID)
	dst, found, err := f.dest.GetServerAuthority(ctx, f.server.ID)
	if err != nil || !found || dst.State != monitoring.AuthorityActive || dst.Generation != src.Generation+1 || dst.FrontierDigest != src.FrontierDigest {
		t.Fatalf("destination=%+v source=%+v found=%v err=%v", dst, src, found, err)
	}
	digest, err := f.dest.IngestFrontierDigest(ctx, f.server.ID)
	if err != nil || digest != src.FrontierDigest {
		t.Fatalf("destination state %q != source frozen %q (err=%v)", digest, src.FrontierDigest, err)
	}
	if f.requests.Load() < 8 {
		t.Fatalf("expected the cutover to use the network, only %d requests", f.requests.Load())
	}
}

func TestPeerRefusesAClientWithoutACertificateOrWithAWrongServerPin(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t, "cutover-0002")
	noCert := f.client()
	noCert.ClientCert = tls.Certificate{}
	if _, _, err := noCert.GetAuthority(ctx, f.server.ID); err == nil {
		t.Fatal("a client without a certificate reached the peer")
	}
	wrongPin := f.client()
	wrongPin.ServerCertSHA256 = strings.Repeat("0", 64)
	if _, _, err := wrongPin.GetAuthority(ctx, f.server.ID); err == nil {
		t.Fatal("a client trusted a server certificate that does not match its pin")
	}
	if _, _, err := f.client().GetAuthority(ctx, f.server.ID); err != nil {
		t.Fatalf("the granted client was refused: %v", err)
	}
}

func TestPeerRefusesUnknownExpiredAndMisboundGrants(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t, "cutover-0003")
	stranger, _ := selfSigned(t, "stranger")
	other := f.client()
	other.ClientCert = stranger
	if _, _, err := other.GetAuthority(ctx, f.server.ID); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a certificate with no grant was accepted: %v", err)
	}
	f.now = f.now.Add(2 * time.Hour)
	if _, _, err := f.client().GetAuthority(ctx, f.server.ID); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("an expired grant was accepted: %v", err)
	}
	f.now = f.now.Add(-2 * time.Hour)
	wrongServer := f.client()
	wrongServer.ServerID = "server-other-00001"
	handoff := monitoring.AuthorityHandoff{ServerID: wrongServer.ServerID, CutoverID: "cutover-0003", SourceGeneration: 3, FrontierDigest: strings.Repeat("a", 64), SourceRequestDigest: strings.Repeat("b", 64), NewOwner: "owner-hub-bbbb"}
	if _, err := wrongServer.ActivateAuthority(ctx, handoff); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a grant for one server activated another: %v", err)
	}
	wrongCutover := handoff
	wrongCutover.ServerID, wrongCutover.CutoverID = f.server.ID, "cutover-9999"
	if _, err := f.client().ActivateAuthority(ctx, wrongCutover); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a grant for one cutover activated another: %v", err)
	}
	if _, found, err := f.dest.GetServerAuthority(ctx, f.server.ID); err != nil || found {
		t.Fatalf("a refused request left authority behind: found=%v err=%v", found, err)
	}
}

func TestPeerRefusesAnArtifactForAServerOutsideTheGrant(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t, "cutover-0004")
	foreign := f.server
	foreign.ID = "server-foreign-0001"
	if err := f.source.EnsureServer(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := f.source.IngestSamples(ctx, foreign.ID, samples(foreign.ID, 0, 2), nil); err != nil {
		t.Fatal(err)
	}
	artifact, err := updater.ExportMigration(ctx, f.sourceDB, updater.ExportOptions{ServerID: foreign.ID, ReplaySafe: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client().ImportArtifact(ctx, artifact, nil); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("an artifact for a server outside the grant was imported: %v", err)
	}
	var count int
	if err := f.destDB.QueryRow(`SELECT COUNT(*) FROM servers`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("the destination gained servers from a refused import: count=%d err=%v", count, err)
	}
}

func rawRequest(t *testing.T, f *peerFixture, method, path string, header map[string]string, body []byte) *http.Response {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{f.clientCert}, MinVersion: tls.VersionTLS13}}}
	request, err := http.NewRequest(method, f.httpServer.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range header {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func TestPeerRejectsTamperedChunksBadCommitsAndOversizeBodies(t *testing.T) {
	f := newPeerFixture(t, "cutover-0005")
	payload := []byte("hello chunk payload")
	sum := sha256.Sum256(payload)
	artifact := hex.EncodeToString(sum[:])
	good := hex.EncodeToString(func() []byte { s := sha256.Sum256(payload); return s[:] }())
	if response := rawRequest(t, f, http.MethodPut, "/v1/cutover/artifact/"+artifact+"/chunk/0", map[string]string{"X-Payesh-Chunk-SHA256": strings.Repeat("0", 64)}, payload); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a chunk with the wrong hash was accepted: %d", response.StatusCode)
	}
	if response := rawRequest(t, f, http.MethodPut, "/v1/cutover/artifact/"+artifact+"/chunk/0", map[string]string{"X-Payesh-Chunk-SHA256": good}, payload); response.StatusCode != http.StatusNoContent {
		t.Fatalf("a valid chunk was refused: %d", response.StatusCode)
	}
	if response := rawRequest(t, f, http.MethodPost, "/v1/cutover/artifact/"+artifact+"/commit", map[string]string{"Content-Type": "application/json"}, []byte(fmt.Sprintf(`{"chunks":1,"size":%d}`, len(payload)+1))); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a commit with the wrong size was accepted: %d", response.StatusCode)
	}
	wrongSum := strings.Repeat("1", 64)
	if response := rawRequest(t, f, http.MethodPut, "/v1/cutover/artifact/"+wrongSum+"/chunk/0", map[string]string{"X-Payesh-Chunk-SHA256": good}, payload); response.StatusCode != http.StatusNoContent {
		t.Fatalf("chunk upload under another artifact id: %d", response.StatusCode)
	}
	if response := rawRequest(t, f, http.MethodPost, "/v1/cutover/artifact/"+wrongSum+"/commit", map[string]string{"Content-Type": "application/json"}, []byte(fmt.Sprintf(`{"chunks":1,"size":%d}`, len(payload)))); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a commit whose content does not hash to the artifact id was accepted: %d", response.StatusCode)
	}
	huge := bytes.Repeat([]byte("x"), maxChunkBytes+1)
	hugeSum := sha256.Sum256(huge)
	if response := rawRequest(t, f, http.MethodPut, "/v1/cutover/artifact/"+artifact+"/chunk/1", map[string]string{"X-Payesh-Chunk-SHA256": hex.EncodeToString(hugeSum[:])}, huge); response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversize chunk was accepted: %d", response.StatusCode)
	}
	if response := rawRequest(t, f, http.MethodPut, "/v1/cutover/artifact/not-a-hash/chunk/0", map[string]string{"X-Payesh-Chunk-SHA256": good}, payload); response.StatusCode != http.StatusBadRequest && response.StatusCode != http.StatusNotFound {
		t.Fatalf("a malformed artifact id was accepted: %d", response.StatusCode)
	}
	if response := rawRequest(t, f, http.MethodPut, "/v1/cutover/artifact/"+artifact+"/chunk/9999", map[string]string{"X-Payesh-Chunk-SHA256": good}, payload); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a chunk index beyond the limit was accepted: %d", response.StatusCode)
	}
}

// A dropped connection must not restart the transfer: the peer reports which
// chunks it already holds and the client sends only the rest.
func TestUploadResumesFromTheChunksThePeerAlreadyHolds(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t, "cutover-0006")
	artifact, err := updater.ExportMigration(ctx, f.sourceDB, updater.ExportOptions{ServerID: f.server.ID, ReplaySafe: true})
	if err != nil {
		t.Fatal(err)
	}
	client := f.client()
	client.ChunkSize = 512
	chunks := (len(artifact) + client.ChunkSize - 1) / client.ChunkSize
	if chunks < 4 {
		t.Fatalf("artifact too small to exercise chunking: %d bytes", len(artifact))
	}
	sum := sha256.Sum256(artifact)
	id := hex.EncodeToString(sum[:])
	for index := 0; index < 2; index++ {
		part := artifact[index*client.ChunkSize : (index+1)*client.ChunkSize]
		partSum := sha256.Sum256(part)
		if response := rawRequest(t, f, http.MethodPut, fmt.Sprintf("/v1/cutover/artifact/%s/chunk/%d", id, index), map[string]string{"X-Payesh-Chunk-SHA256": hex.EncodeToString(partSum[:])}, part); response.StatusCode != http.StatusNoContent {
			t.Fatalf("seed chunk %d: %d", index, response.StatusCode)
		}
	}
	puts := f.puts.Load()
	if err := client.ImportArtifact(ctx, artifact, nil); err != nil {
		t.Fatal(err)
	}
	if want, got := int64(chunks-2), f.puts.Load()-puts; got != want {
		t.Fatalf("resumed upload sent %d chunks, want only the %d missing", got, want)
	}
	var count int
	if err := f.destDB.QueryRow(`SELECT COUNT(*) FROM metric_samples WHERE server_id=?`, string(f.server.ID)).Scan(&count); err != nil || count != 6 {
		t.Fatalf("imported samples=%d err=%v", count, err)
	}
	if err := client.ImportArtifact(ctx, artifact, nil); err != nil {
		t.Fatalf("re-importing the same artifact must be idempotent: %v", err)
	}
}

func TestClientErrorsAreNotRetriedWhenTheyCannotSucceed(t *testing.T) {
	ctx := context.Background()
	f := newPeerFixture(t, "cutover-0007")
	stranger, _ := selfSigned(t, "stranger")
	other := f.client()
	other.ClientCert = stranger
	before := f.requests.Load()
	if _, _, err := other.GetAuthority(ctx, f.server.ID); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected result: %v", err)
	}
	if got := f.requests.Load() - before; got != 1 {
		t.Fatalf("a 403 was retried %d times", got)
	}
}
