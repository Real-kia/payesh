package fleet

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/transport"
)

const (
	enrollmentJobKind    = "enrollment"
	maxEnrollmentToken   = 256
	defaultEnrollmentTTL = 10 * time.Minute
	maxIssuedTokenCache  = 256
)

// EnrollmentService is the authenticated browser-side job producer. It
// validates pairing-token ownership without persisting the token and creates
// a durable, idempotent job for the node transport consumer.
type EnrollmentService struct {
	Store     *monitoring.Store
	Authority *transport.CertificateAuthority
	Now       func() time.Time
	// issuedMu protects the process-memory retry cache.  Enrollment tokens are
	// intentionally never persisted in cleartext; keeping this bounded cache
	// allows an operator to safely retry a timed-out response during the token's
	// short lifetime without putting the secret in SQLite or logs.
	issuedMu sync.Mutex
	issued   map[string]issuedEnrollment
}

type issuedEnrollment struct {
	serverID  contracts.ServerID
	idemKey   string
	token     string
	expiresAt time.Time
}

func NewEnrollmentService(store *monitoring.Store, authority *transport.CertificateAuthority) (*EnrollmentService, error) {
	if store == nil || authority == nil {
		return nil, errors.New("enrollment service requires store and authority")
	}
	return &EnrollmentService{Store: store, Authority: authority, issued: make(map[string]issuedEnrollment)}, nil
}

func (s *EnrollmentService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *EnrollmentService) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// TokenHandler is the owner-facing pairing-token issuance seam. It is
// expected to be wrapped in auth.Manager.Middleware by the API, so the only
// caller is the authenticated owner and browser mutations still require CSRF.
// The returned token is write-only from the service's perspective: it is held
// only in bounded process memory until expiry and never enters durable state.
func (s *EnrollmentService) TokenHandler() http.Handler { return http.HandlerFunc(s.serveTokenHTTP) }

type enrollmentTokenRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
}

type enrollmentTokenResponse struct {
	ServerID  contracts.ServerID `json:"server_id"`
	Token     string             `json:"token"`
	ExpiresAt time.Time          `json:"expires_at"`
}

func (s *EnrollmentService) serveTokenHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
		return
	}
	serverID, ok := serverIDForSuffix(r.URL.Path, "/enrollment-token")
	if !ok {
		writeFleetError(w, http.StatusNotFound, "not_found", "server not found", false)
		return
	}
	var request enrollmentTokenRequest
	if !decode(w, r, &request) {
		return
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if len(request.IdempotencyKey) == 0 || len(request.IdempotencyKey) > 128 {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "idempotency_key is invalid", false)
		return
	}
	if _, found, err := s.Store.GetServer(r.Context(), serverID); err != nil {
		writeFleetError(w, http.StatusServiceUnavailable, "storage_unavailable", "server lookup failed", true)
		return
	} else if !found {
		writeFleetError(w, http.StatusNotFound, "not_found", "server not found", false)
		return
	}
	now := s.now()
	key := string(serverID) + "\x00" + request.IdempotencyKey

	s.issuedMu.Lock()
	s.pruneIssuedLocked(now)
	if existing, found := s.issued[key]; found {
		s.issuedMu.Unlock()
		writeEnrollmentToken(w, http.StatusCreated, enrollmentTokenResponse{ServerID: existing.serverID, Token: existing.token, ExpiresAt: existing.expiresAt})
		return
	}
	// Hold the mutex across IssueEnrollment so concurrent requests cannot both
	// issue a token before the CA's server reservation becomes visible.
	enrollment, err := s.Authority.IssueEnrollment(serverID, now)
	if err != nil {
		s.issuedMu.Unlock()
		switch {
		case errors.Is(err, transport.ErrEnrollmentAlreadyIssued), errors.Is(err, transport.ErrServerAlreadyEnrolled), errors.Is(err, transport.ErrEnrollmentInProgress):
			writeFleetError(w, http.StatusConflict, "enrollment_already_issued", "an active enrollment token already exists", false)
		default:
			writeFleetError(w, http.StatusServiceUnavailable, "enrollment_unavailable", "could not issue enrollment token", true)
		}
		return
	}
	if len(s.issued) >= maxIssuedTokenCache {
		var oldestKey string
		var oldestExpiry time.Time
		for candidate, record := range s.issued {
			if oldestKey == "" || record.expiresAt.Before(oldestExpiry) {
				oldestKey, oldestExpiry = candidate, record.expiresAt
			}
		}
		if oldestKey != "" {
			delete(s.issued, oldestKey)
		}
	}
	s.issued[key] = issuedEnrollment{serverID: serverID, idemKey: request.IdempotencyKey, token: enrollment.Token, expiresAt: enrollment.ExpiresAt}
	s.issuedMu.Unlock()
	writeEnrollmentToken(w, http.StatusCreated, enrollmentTokenResponse{ServerID: serverID, Token: enrollment.Token, ExpiresAt: enrollment.ExpiresAt})
}

func (s *EnrollmentService) pruneIssuedLocked(now time.Time) {
	for key, record := range s.issued {
		if !now.Before(record.expiresAt) {
			delete(s.issued, key)
		}
	}
}

func writeEnrollmentToken(w http.ResponseWriter, status int, response enrollmentTokenResponse) {
	data, err := json.Marshal(response)
	if err != nil {
		writeFleetError(w, http.StatusInternalServerError, "internal_error", "request failed", false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

type enrollmentRequest struct {
	Token          string `json:"token"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *EnrollmentService) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
		return
	}
	serverID, ok := enrollmentServerID(r.URL.Path)
	if !ok {
		writeFleetError(w, http.StatusNotFound, "not_found", "server not found", false)
		return
	}
	var request enrollmentRequest
	if !decode(w, r, &request) {
		return
	}
	if len(request.Token) < 16 || len(request.Token) > maxEnrollmentToken || len(request.IdempotencyKey) == 0 || len(request.IdempotencyKey) > 128 {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "token and idempotency_key are invalid", false)
		return
	}
	if _, found, err := s.Store.GetServer(r.Context(), serverID); err != nil {
		writeFleetError(w, http.StatusServiceUnavailable, "storage_unavailable", "server lookup failed", true)
		return
	} else if !found {
		writeFleetError(w, http.StatusNotFound, "not_found", "server not found", false)
		return
	}
	now := s.now()
	hash := enrollmentRequestHash(serverID, request)
	if existing, storedHash, found, err := s.Store.GetJobByOperation(r.Context(), enrollmentJobKind, serverID, request.IdempotencyKey); err != nil {
		writeFleetError(w, http.StatusServiceUnavailable, "job_storage_unavailable", "could not read enrollment job history", true)
		return
	} else if found {
		if storedHash != hash {
			writeFleetError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for different input", false)
			return
		}
		writeJob(w, http.StatusAccepted, existing)
		return
	}
	if err := s.Authority.ValidateEnrollmentToken(serverID, request.Token, now); err != nil {
		writeFleetError(w, http.StatusBadRequest, "invalid_or_expired_enrollment", "pairing token is invalid or expired", false)
		return
	}
	job, created, err := s.Store.CreateJob(r.Context(), contracts.Job{
		ID: newEnrollmentJobID(), Kind: enrollmentJobKind, State: contracts.JobQueued,
		Revision: 0, IdempotencyKey: request.IdempotencyKey, TargetServerID: serverID,
		ExpiresAt: now.Add(defaultEnrollmentTTL), Progress: 0,
	}, hash, now)
	if err != nil {
		switch {
		case errors.Is(err, monitoring.ErrJobIdempotencyConflict):
			writeFleetError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for different input", false)
		case errors.Is(err, monitoring.ErrStoragePressure):
			writeFleetError(w, http.StatusServiceUnavailable, "storage_pressure", "job storage is full", true)
		default:
			writeFleetError(w, http.StatusServiceUnavailable, "job_storage_unavailable", "could not create enrollment job", true)
		}
		return
	}
	if !created {
		// CreateJob can win a concurrent race between the preflight lookup and
		// this insert; it already verified the request hash for us.
		writeJob(w, http.StatusAccepted, job)
		return
	}
	writeJob(w, http.StatusAccepted, job)
}

func enrollmentServerID(path string) (contracts.ServerID, bool) {
	return serverIDForSuffix(path, "/enrollment")
}

func serverIDForSuffix(path, suffix string) (contracts.ServerID, bool) {
	const prefix = "/api/v1/servers/"
	if len(path) <= len(prefix)+len(suffix) || len(path) <= len(suffix) || path[:len(prefix)] != prefix || path[len(path)-len(suffix):] != suffix {
		return "", false
	}
	raw := path[len(prefix) : len(path)-len(suffix)]
	if raw == "" || len(raw) < 16 || len(raw) > 128 {
		return "", false
	}
	for _, ch := range raw {
		if !(ch == '-' || ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9') {
			return "", false
		}
	}
	return contracts.ServerID(raw), true
}

func enrollmentRequestHash(serverID contracts.ServerID, request enrollmentRequest) string {
	sum := sha256.Sum256([]byte(string(serverID) + "\x00" + request.Token + "\x00" + request.IdempotencyKey))
	return hex.EncodeToString(sum[:])
}

func newEnrollmentJobID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("enrollment-%d", time.Now().UnixNano())
	}
	return "enrollment-" + hex.EncodeToString(raw[:])
}
