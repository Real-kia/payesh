// Package cutoverpeer is the authenticated network peer for a server cutover.
//
// The destination runs a Server over TLS 1.3 with mutual authentication. A client
// is identified by the SHA-256 fingerprint of its certificate and may act only
// through an explicit, expiring Grant that names one server and one cutover; the
// server never takes the server or cutover identity from the request. Artifacts
// travel in hash-verified, resumable chunks into a private spool, are inspected
// to confirm they contain only the granted server, and are then imported with the
// updater's existing replay-safe migration. Authority is activated only through
// the store's verification of the transferred state.
package cutoverpeer

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/updater"
)

const (
	maxChunkBytes    = 4 << 20
	maxChunks        = 256
	maxArtifactBytes = 64 << 20
	maxJSONBytes     = 64 << 10
	chunkHashHeader  = "X-Payesh-Chunk-SHA256"
)

var (
	hexDigest   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	cutoverName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$`)
)

// Grant authorizes one client certificate to run one cutover for one server
// until it expires. It is the only authority the peer accepts.
type Grant struct {
	ClientFingerprint string
	ServerID          contracts.ServerID
	CutoverID         string
	ExpiresAt         time.Time
}

// Server serves the destination side of a cutover.
type Server struct {
	DB     *sql.DB
	Store  *monitoring.Store
	Grants []Grant
	// GrantsFunc, when set, is consulted on every request so grants added while
	// serving take effect; an error fails closed and grants nothing.
	GrantsFunc func() ([]Grant, error)
	SpoolDir   string
	Now        func() time.Time

	mu sync.Mutex
}

// ServerTLSConfig requires TLS 1.3 and a client certificate. Clients are
// authorized by grant fingerprint, not by a certificate chain.
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert}
}

// Fingerprint is the lowercase hex SHA-256 of a certificate's DER encoding.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) lookup(fingerprint string) (Grant, bool) {
	now := s.now()
	grants := s.Grants
	if s.GrantsFunc != nil {
		loaded, err := s.GrantsFunc()
		if err != nil {
			return Grant{}, false
		}
		grants = loaded
	}
	for _, grant := range grants {
		if subtle.ConstantTimeCompare([]byte(grant.ClientFingerprint), []byte(fingerprint)) != 1 {
			continue
		}
		if !hexDigest.MatchString(grant.ClientFingerprint) || !cutoverName.MatchString(grant.CutoverID) || grant.ServerID == "" || !now.Before(grant.ExpiresAt) {
			continue
		}
		return grant, true
	}
	return Grant{}, false
}

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: code, Message: message})
}

// Handler returns the peer's HTTP routes. Every route requires a granted client.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/cutover/server", s.authorize(s.serverStatus))
	mux.HandleFunc("GET /v1/cutover/artifact/{sha}", s.authorize(s.artifactStatus))
	mux.HandleFunc("PUT /v1/cutover/artifact/{sha}/chunk/{index}", s.authorize(s.putChunk))
	mux.HandleFunc("POST /v1/cutover/artifact/{sha}/commit", s.authorize(s.commit))
	mux.HandleFunc("POST /v1/cutover/import", s.authorize(s.importArtifact))
	mux.HandleFunc("POST /v1/cutover/activate", s.authorize(s.activate))
	return mux
}

func (s *Server) authorize(next func(http.ResponseWriter, *http.Request, Grant)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			fail(w, http.StatusUnauthorized, "client_certificate_required", "a client certificate is required")
			return
		}
		grant, ok := s.lookup(Fingerprint(r.TLS.PeerCertificates[0]))
		if !ok {
			fail(w, http.StatusForbidden, "forbidden", "no valid grant for this client certificate")
			return
		}
		next(w, r, grant)
	}
}

func (s *Server) spool(grant Grant) (string, error) {
	if s.SpoolDir == "" {
		return "", errors.New("cutoverpeer: spool directory is not configured")
	}
	dir := filepath.Join(s.SpoolDir, grant.CutoverID)
	return dir, os.MkdirAll(dir, 0o700)
}

func decodeStrict(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "the request body is not valid")
		return false
	}
	return true
}

type authorityWire struct {
	ServerID         string `json:"server_id"`
	Generation       uint64 `json:"generation"`
	Owner            string `json:"owner"`
	State            string `json:"state"`
	CutoverID        string `json:"cutover_id"`
	TransitionDigest string `json:"transition_digest"`
	FrontierDigest   string `json:"frontier_digest"`
}

func toWire(a monitoring.ServerAuthority) authorityWire {
	return authorityWire{ServerID: string(a.ServerID), Generation: a.Generation, Owner: a.Owner, State: string(a.State), CutoverID: a.CutoverID, TransitionDigest: a.TransitionDigest, FrontierDigest: a.FrontierDigest}
}

func fromWire(w authorityWire) monitoring.ServerAuthority {
	return monitoring.ServerAuthority{ServerID: contracts.ServerID(w.ServerID), Generation: w.Generation, Owner: w.Owner, State: monitoring.AuthorityState(w.State), CutoverID: w.CutoverID, TransitionDigest: w.TransitionDigest, FrontierDigest: w.FrontierDigest}
}

type serverStatusWire struct {
	HasServer bool          `json:"has_server"`
	Found     bool          `json:"found"`
	Authority authorityWire `json:"authority"`
}

func (s *Server) serverStatus(w http.ResponseWriter, r *http.Request, grant Grant) {
	var count int
	if err := s.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM servers WHERE id=?`, string(grant.ServerID)).Scan(&count); err != nil {
		fail(w, http.StatusInternalServerError, "storage_error", "server lookup failed")
		return
	}
	authority, found, err := s.Store.GetServerAuthority(r.Context(), grant.ServerID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "storage_error", "authority lookup failed")
		return
	}
	status := serverStatusWire{HasServer: count == 1, Found: found}
	if found {
		status.Authority = toWire(authority)
	}
	writeJSON(w, http.StatusOK, status)
}

func artifactPaths(dir, sha string) (parts, final string) {
	return filepath.Join(dir, sha), filepath.Join(dir, sha+".artifact")
}

func validSHA(sha string) bool { return hexDigest.MatchString(sha) }

type artifactStatusWire struct {
	Complete bool  `json:"complete"`
	Chunks   []int `json:"chunks"`
}

func (s *Server) artifactStatus(w http.ResponseWriter, r *http.Request, grant Grant) {
	sha := r.PathValue("sha")
	if !validSHA(sha) {
		fail(w, http.StatusBadRequest, "invalid_artifact", "the artifact identifier is not a SHA-256 digest")
		return
	}
	dir, err := s.spool(grant)
	if err != nil {
		fail(w, http.StatusInternalServerError, "spool_error", "the spool is unavailable")
		return
	}
	partsDir, final := artifactPaths(dir, sha)
	status := artifactStatusWire{Chunks: []int{}}
	if _, err := os.Stat(final); err == nil {
		status.Complete = true
		writeJSON(w, http.StatusOK, status)
		return
	}
	entries, err := os.ReadDir(partsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fail(w, http.StatusInternalServerError, "spool_error", "the spool is unavailable")
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".part") {
			continue
		}
		if index, err := strconv.Atoi(strings.TrimSuffix(name, ".part")); err == nil && index >= 0 && index < maxChunks {
			status.Chunks = append(status.Chunks, index)
		}
	}
	sort.Ints(status.Chunks)
	writeJSON(w, http.StatusOK, status)
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Server) putChunk(w http.ResponseWriter, r *http.Request, grant Grant) {
	sha := r.PathValue("sha")
	index, indexErr := strconv.Atoi(r.PathValue("index"))
	want := r.Header.Get(chunkHashHeader)
	if !validSHA(sha) || indexErr != nil || index < 0 || index >= maxChunks || !validSHA(want) {
		fail(w, http.StatusBadRequest, "invalid_chunk", "the chunk identifier, index or hash is invalid")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChunkBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, "chunk_too_large", "the chunk exceeds the size limit")
			return
		}
		fail(w, http.StatusBadRequest, "invalid_chunk", "the chunk could not be read")
		return
	}
	sum := sha256.Sum256(body)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(want)) != 1 {
		fail(w, http.StatusBadRequest, "chunk_hash_mismatch", "the chunk does not match its declared hash")
		return
	}
	dir, err := s.spool(grant)
	if err != nil {
		fail(w, http.StatusInternalServerError, "spool_error", "the spool is unavailable")
		return
	}
	partsDir, final := artifactPaths(dir, sha)
	if _, err := os.Stat(final); err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := os.MkdirAll(partsDir, 0o700); err != nil {
		fail(w, http.StatusInternalServerError, "spool_error", "the spool is unavailable")
		return
	}
	if err := writeAtomic(filepath.Join(partsDir, strconv.Itoa(index)+".part"), body); err != nil {
		fail(w, http.StatusInternalServerError, "spool_error", "the chunk could not be stored")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type commitWire struct {
	Chunks int `json:"chunks"`
	Size   int `json:"size"`
}

func (s *Server) commit(w http.ResponseWriter, r *http.Request, grant Grant) {
	sha := r.PathValue("sha")
	var request commitWire
	if !validSHA(sha) || !decodeStrict(w, r, &request) {
		if !validSHA(sha) {
			fail(w, http.StatusBadRequest, "invalid_artifact", "the artifact identifier is not a SHA-256 digest")
		}
		return
	}
	if request.Chunks < 1 || request.Chunks > maxChunks || request.Size < 1 || request.Size > maxArtifactBytes {
		fail(w, http.StatusBadRequest, "invalid_commit", "the chunk count or size is out of range")
		return
	}
	dir, err := s.spool(grant)
	if err != nil {
		fail(w, http.StatusInternalServerError, "spool_error", "the spool is unavailable")
		return
	}
	partsDir, final := artifactPaths(dir, sha)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(final); err == nil {
		writeJSON(w, http.StatusOK, artifactStatusWire{Complete: true, Chunks: []int{}})
		return
	}
	assembled := make([]byte, 0, request.Size)
	for index := 0; index < request.Chunks; index++ {
		part, err := os.ReadFile(filepath.Join(partsDir, strconv.Itoa(index)+".part"))
		if errors.Is(err, fs.ErrNotExist) {
			fail(w, http.StatusConflict, "chunk_missing", fmt.Sprintf("chunk %d has not been uploaded", index))
			return
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, "spool_error", "a chunk could not be read")
			return
		}
		if len(assembled)+len(part) > maxArtifactBytes {
			fail(w, http.StatusBadRequest, "invalid_commit", "the artifact exceeds the size limit")
			return
		}
		assembled = append(assembled, part...)
	}
	sum := sha256.Sum256(assembled)
	if len(assembled) != request.Size || hex.EncodeToString(sum[:]) != sha {
		fail(w, http.StatusBadRequest, "artifact_mismatch", "the assembled artifact does not match its identifier")
		return
	}
	if err := writeAtomic(final, assembled); err != nil {
		fail(w, http.StatusInternalServerError, "spool_error", "the artifact could not be stored")
		return
	}
	_ = os.RemoveAll(partsDir)
	writeJSON(w, http.StatusOK, artifactStatusWire{Complete: true, Chunks: []int{}})
}

type importWire struct {
	ArtifactSHA256 string `json:"artifact_sha256"`
	PreviousSHA256 string `json:"previous_sha256,omitempty"`
}

type importResultWire struct {
	AlreadyApplied bool `json:"already_applied"`
	Samples        int  `json:"samples"`
}

// loadGranted reads a completed artifact and refuses anything that is not a
// plain single-server artifact for exactly the granted server.
func (s *Server) loadGranted(dir, sha string, grant Grant) ([]byte, int, string) {
	_, final := artifactPaths(dir, sha)
	data, err := os.ReadFile(final)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, http.StatusConflict, "artifact_missing"
	}
	if err != nil {
		return nil, http.StatusInternalServerError, "spool_error"
	}
	info, err := updater.InspectMigration(data)
	if err != nil || info.Kind == updater.ExportFullHub {
		return nil, http.StatusForbidden, "artifact_outside_grant"
	}
	if info.SourceServerID != "" && info.SourceServerID != grant.ServerID {
		return nil, http.StatusForbidden, "artifact_outside_grant"
	}
	if len(info.ServerIDs) == 0 {
		return nil, http.StatusForbidden, "artifact_outside_grant"
	}
	for _, id := range info.ServerIDs {
		if id != grant.ServerID {
			return nil, http.StatusForbidden, "artifact_outside_grant"
		}
	}
	return data, 0, ""
}

func (s *Server) importArtifact(w http.ResponseWriter, r *http.Request, grant Grant) {
	var request importWire
	if !decodeStrict(w, r, &request) {
		return
	}
	if !validSHA(request.ArtifactSHA256) || (request.PreviousSHA256 != "" && !validSHA(request.PreviousSHA256)) {
		fail(w, http.StatusBadRequest, "invalid_request", "artifact identifiers must be SHA-256 digests")
		return
	}
	dir, err := s.spool(grant)
	if err != nil {
		fail(w, http.StatusInternalServerError, "spool_error", "the spool is unavailable")
		return
	}
	artifact, status, code := s.loadGranted(dir, request.ArtifactSHA256, grant)
	if code != "" {
		fail(w, status, code, "the artifact cannot be imported for this grant")
		return
	}
	var previous []byte
	if request.PreviousSHA256 != "" {
		if previous, status, code = s.loadGranted(dir, request.PreviousSHA256, grant); code != "" {
			fail(w, status, code, "the previous artifact cannot be used for this grant")
			return
		}
	}
	s.mu.Lock()
	result, err := updater.ImportMigration(r.Context(), s.DB, artifact, updater.ImportOptions{PreviousArtifact: previous})
	s.mu.Unlock()
	if err != nil {
		fail(w, http.StatusConflict, "import_refused", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, importResultWire{AlreadyApplied: result.AlreadyApplied, Samples: result.Samples})
}

type handoffWire struct {
	ServerID            string `json:"server_id"`
	CutoverID           string `json:"cutover_id"`
	SourceGeneration    uint64 `json:"source_generation"`
	FrontierDigest      string `json:"frontier_digest"`
	SourceRequestDigest string `json:"source_request_digest"`
	NewOwner            string `json:"new_owner"`
}

func (s *Server) activate(w http.ResponseWriter, r *http.Request, grant Grant) {
	var request handoffWire
	if !decodeStrict(w, r, &request) {
		return
	}
	// The grant, not the request, decides which server and cutover may activate.
	if contracts.ServerID(request.ServerID) != grant.ServerID || request.CutoverID != grant.CutoverID {
		fail(w, http.StatusForbidden, "forbidden", "the handoff is outside this grant")
		return
	}
	handoff := monitoring.AuthorityHandoff{ServerID: grant.ServerID, CutoverID: grant.CutoverID, SourceGeneration: request.SourceGeneration,
		FrontierDigest: request.FrontierDigest, SourceRequestDigest: request.SourceRequestDigest, NewOwner: request.NewOwner}
	authority, err := s.Store.ActivateServerAuthority(r.Context(), handoff)
	if err != nil {
		if errors.Is(err, monitoring.ErrAuthorityConflict) {
			fail(w, http.StatusConflict, "authority_conflict", err.Error())
			return
		}
		fail(w, http.StatusBadRequest, "activation_refused", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toWire(authority))
}
