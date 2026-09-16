// Package transport contains the authenticated node identity primitives. The
// WebSocket framing/reconnect loop can be layered on these records without
// weakening certificate or enrollment checks.
package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

type CertificateAuthority struct {
	mu                  sync.Mutex
	key                 *ecdsa.PrivateKey
	cert                *x509.Certificate
	revoked             map[string]struct{}
	revocationListeners []func(contracts.ServerID, string)
	enrollments         map[[sha256.Size]byte]enrollmentRecord
	// serverEnrollments and certificates make server identity ownership
	// authoritative CA state. A server may have at most one pending pairing
	// token and one live certificate at a time; otherwise two independently
	// enrolled certificates could impersonate the same node after reconnect.
	serverEnrollments map[contracts.ServerID][sha256.Size]byte
	certificates      map[contracts.ServerID]string
	pending           map[contracts.ServerID]struct{}
	repository        IdentityRepository
}

// Stable enrollment errors let the authenticated operator API distinguish an
// active reservation from an authority/storage failure without exposing CA
// internals or token material.
var (
	ErrServerAlreadyEnrolled   = errors.New("server_already_enrolled")
	ErrEnrollmentInProgress    = errors.New("server_enrollment_in_progress")
	ErrEnrollmentAlreadyIssued = errors.New("enrollment_already_issued")
)

// IdentityRepository atomically stores the bounded enrollment authority.
// It contains the CA private key, so implementations must use restricted
// local storage and must never expose this blob through an API.
type IdentityRepository interface {
	LoadIdentityState() (data []byte, found bool, err error)
	SaveIdentityState(data []byte) error
}

type enrollmentRecord struct {
	serverID  contracts.ServerID
	expiresAt time.Time
	consumed  bool
}

func NewCertificateAuthority(now time.Time) (*CertificateAuthority, error) {
	return NewPersistentCertificateAuthority(now, nil)
}

func NewPersistentCertificateAuthority(now time.Time, repository IdentityRepository) (*CertificateAuthority, error) {
	if repository != nil {
		data, found, err := repository.LoadIdentityState()
		if err != nil {
			return nil, fmt.Errorf("load enrollment authority: %w", err)
		}
		if found {
			ca := &CertificateAuthority{repository: repository}
			if err := ca.restoreLocked(data); err != nil {
				return nil, fmt.Errorf("restore enrollment authority: %w", err)
			}
			return ca, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Payesh enrollment CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * 365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	ca := &CertificateAuthority{
		key:               key,
		cert:              cert,
		revoked:           make(map[string]struct{}),
		enrollments:       make(map[[sha256.Size]byte]enrollmentRecord),
		serverEnrollments: make(map[contracts.ServerID][sha256.Size]byte),
		certificates:      make(map[contracts.ServerID]string),
		pending:           make(map[contracts.ServerID]struct{}),
		repository:        repository,
	}
	if err := ca.persistLocked(); err != nil {
		return nil, fmt.Errorf("persist enrollment authority: %w", err)
	}
	return ca, nil
}

func (ca *CertificateAuthority) PEM() []byte {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

type Enrollment struct {
	ServerID  contracts.ServerID
	Token     string
	ExpiresAt time.Time
}
type NodeIdentity struct {
	ServerID                      contracts.ServerID
	CertificatePEM, PrivateKeyPEM []byte
	NotAfter                      time.Time
	Fingerprint                   string
}

// Renew rotates an enrolled node certificate and private key without issuing
// a second pairing token. The previous certificate is revoked as part of the
// same CA state transition, so a reconnect using the old identity fails
// closed. Callers should replace their on-disk identity only after this
// method returns successfully.
func (ca *CertificateAuthority) Renew(identity NodeIdentity, now time.Time) (NodeIdentity, error) {
	if now.IsZero() || len(identity.CertificatePEM) == 0 {
		return NodeIdentity{}, errors.New("invalid renewal identity")
	}
	block, _ := pem.Decode(identity.CertificatePEM)
	if block == nil {
		return NodeIdentity{}, errors.New("invalid renewal certificate")
	}
	oldCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return NodeIdentity{}, err
	}
	serverID := contracts.ServerID(oldCert.Subject.CommonName)
	if serverID == "" || identity.ServerID != "" && identity.ServerID != serverID {
		return NodeIdentity{}, errors.New("renewal identity mismatch")
	}
	oldFingerprint := fingerprint(oldCert.Raw)
	ca.mu.Lock()
	if _, revoked := ca.revoked[oldFingerprint]; revoked {
		ca.mu.Unlock()
		return NodeIdentity{}, errors.New("certificate_revoked")
	}
	if owned, ok := ca.certificates[serverID]; !ok || owned != oldFingerprint {
		ca.mu.Unlock()
		return NodeIdentity{}, errors.New("certificate_not_registered")
	}
	if !now.Before(oldCert.NotAfter) {
		ca.mu.Unlock()
		return NodeIdentity{}, errors.New("certificate_expired")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		ca.mu.Unlock()
		return NodeIdentity{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		ca.mu.Unlock()
		return NodeIdentity{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: string(serverID)},
		DNSNames:     []string{string(serverID)},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(90 * 24 * time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		ca.mu.Unlock()
		return NodeIdentity{}, err
	}
	newCert, err := x509.ParseCertificate(der)
	if err != nil {
		ca.mu.Unlock()
		return NodeIdentity{}, err
	}
	newFingerprint := fingerprint(newCert.Raw)
	ca.revoked[oldFingerprint] = struct{}{}
	ca.certificates[serverID] = newFingerprint
	if err := ca.persistLocked(); err != nil {
		delete(ca.revoked, oldFingerprint)
		ca.certificates[serverID] = oldFingerprint
		ca.mu.Unlock()
		return NodeIdentity{}, fmt.Errorf("persist certificate renewal: %w", err)
	}
	listeners := append([]func(contracts.ServerID, string){}, ca.revocationListeners...)
	ca.mu.Unlock()
	for _, listener := range listeners {
		listener(serverID, oldFingerprint)
	}
	return NodeIdentity{
		ServerID:       serverID,
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		PrivateKeyPEM:  marshalKey(key),
		NotAfter:       newCert.NotAfter,
		Fingerprint:    newFingerprint,
	}, nil
}

// IssueEnrollment returns a short-lived, single-use pairing token. Tokens are
// stored as digests and are never logged or returned after Consume succeeds.
func (ca *CertificateAuthority) IssueEnrollment(serverID contracts.ServerID, now time.Time) (Enrollment, error) {
	if len(serverID) < 16 || len(serverID) > 128 {
		return Enrollment{}, errors.New("invalid server id")
	}
	token, err := randomToken(32)
	if err != nil {
		return Enrollment{}, fmt.Errorf("generate enrollment token: %w", err)
	}
	expiresAt := now.Add(10 * time.Minute)
	digest := sha256.Sum256([]byte(token))
	ca.mu.Lock()
	if _, enrolled := ca.certificates[serverID]; enrolled {
		ca.mu.Unlock()
		return Enrollment{}, ErrServerAlreadyEnrolled
	}
	if _, enrolling := ca.pending[serverID]; enrolling {
		ca.mu.Unlock()
		return Enrollment{}, ErrEnrollmentInProgress
	}
	if previousDigest, exists := ca.serverEnrollments[serverID]; exists {
		previous := ca.enrollments[previousDigest]
		if !previous.consumed && now.Before(previous.expiresAt) {
			ca.mu.Unlock()
			return Enrollment{}, ErrEnrollmentAlreadyIssued
		}
		// Expired or failed enrollments no longer reserve the server. Keep the
		// consumed digest in the CA map so a copied caller value cannot replay it.
		delete(ca.serverEnrollments, serverID)
	}
	ca.enrollments[digest] = enrollmentRecord{serverID: serverID, expiresAt: expiresAt}
	ca.serverEnrollments[serverID] = digest
	if err := ca.persistLocked(); err != nil {
		delete(ca.enrollments, digest)
		delete(ca.serverEnrollments, serverID)
		ca.mu.Unlock()
		return Enrollment{}, fmt.Errorf("persist enrollment: %w", err)
	}
	ca.mu.Unlock()
	return Enrollment{ServerID: serverID, Token: token, ExpiresAt: expiresAt}, nil
}

// ValidateEnrollmentToken checks that a pairing token is currently owned by
// serverID and has not expired or been consumed. It does not consume the
// token; the eventual node enrollment operation remains the single atomic
// consumer. This lets an authenticated job producer reject stale input
// without storing the cleartext token.
func (ca *CertificateAuthority) ValidateEnrollmentToken(serverID contracts.ServerID, token string, now time.Time) error {
	if len(serverID) < 16 || len(serverID) > 128 || token == "" || now.IsZero() {
		return errors.New("invalid_or_expired_enrollment")
	}
	digest := sha256.Sum256([]byte(token))
	ca.mu.Lock()
	defer ca.mu.Unlock()
	record, ok := ca.enrollments[digest]
	ownedDigest, ownsServer := ca.serverEnrollments[serverID]
	if !ok || !ownsServer || ownedDigest != digest || record.consumed || record.serverID != serverID || !now.Before(record.expiresAt) {
		return errors.New("invalid_or_expired_enrollment")
	}
	return nil
}

// ConsumeEnrollmentToken resolves and consumes a pairing token without
// requiring the caller to persist the cleartext token or a caller-owned
// Enrollment struct. This is the node-side bootstrap seam: the token is
// presented over the authenticated bootstrap channel, and is consumed exactly
// once by the CA before a client identity is returned.
func (ca *CertificateAuthority) ConsumeEnrollmentToken(token string, now time.Time) (NodeIdentity, error) {
	if token == "" || now.IsZero() {
		return NodeIdentity{}, errors.New("invalid_or_expired_enrollment")
	}
	digest := sha256.Sum256([]byte(token))
	ca.mu.Lock()
	record, ok := ca.enrollments[digest]
	if !ok || record.consumed || !now.Before(record.expiresAt) {
		ca.mu.Unlock()
		return NodeIdentity{}, errors.New("invalid_or_expired_enrollment")
	}
	serverID := record.serverID
	ca.mu.Unlock()
	enrollment := Enrollment{ServerID: serverID, Token: token, ExpiresAt: record.expiresAt}
	return ca.Enroll(&enrollment, token, now)
}

// EnrollmentServerID resolves a currently valid token without consuming it.
// It is used by the hub to check durable server ownership before the one-time
// consume transition.
func (ca *CertificateAuthority) EnrollmentServerID(token string, now time.Time) (contracts.ServerID, error) {
	if token == "" || now.IsZero() {
		return "", errors.New("invalid_or_expired_enrollment")
	}
	digest := sha256.Sum256([]byte(token))
	ca.mu.Lock()
	defer ca.mu.Unlock()
	record, ok := ca.enrollments[digest]
	if !ok || record.consumed || !now.Before(record.expiresAt) || record.serverID == "" {
		return "", errors.New("invalid_or_expired_enrollment")
	}
	return record.serverID, nil
}

func (ca *CertificateAuthority) Enroll(e *Enrollment, token string, now time.Time) (NodeIdentity, error) {
	if e == nil || token == "" || !constantEqual(token, e.Token) {
		return NodeIdentity{}, errors.New("invalid_or_expired_enrollment")
	}
	digest := sha256.Sum256([]byte(token))
	ca.mu.Lock()
	record, ok := ca.enrollments[digest]
	ownedDigest, ownsServer := ca.serverEnrollments[e.ServerID]
	if !ok || !ownsServer || ownedDigest != digest || record.consumed || !now.Before(record.expiresAt) || record.serverID != e.ServerID {
		ca.mu.Unlock()
		return NodeIdentity{}, errors.New("invalid_or_expired_enrollment")
	}
	if _, enrolled := ca.certificates[record.serverID]; enrolled {
		ca.mu.Unlock()
		return NodeIdentity{}, ErrServerAlreadyEnrolled
	}
	if _, enrolling := ca.pending[record.serverID]; enrolling {
		ca.mu.Unlock()
		return NodeIdentity{}, ErrEnrollmentInProgress
	}
	// Consumption is authoritative CA state, not caller-owned Enrollment
	// state. Mark it before generating credentials so concurrent calls and
	// copies of the original value cannot replay the pairing token.
	record.consumed = true
	ca.enrollments[digest] = record
	ca.pending[record.serverID] = struct{}{}
	if err := ca.persistLocked(); err != nil {
		record.consumed = false
		ca.enrollments[digest] = record
		delete(ca.pending, record.serverID)
		ca.mu.Unlock()
		return NodeIdentity{}, fmt.Errorf("persist enrollment consumption: %w", err)
	}
	ca.mu.Unlock()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		ca.clearPending(record.serverID, digest)
		return NodeIdentity{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		ca.clearPending(record.serverID, digest)
		return NodeIdentity{}, err
	}
	certTmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: string(record.serverID)}, DNSNames: []string{string(record.serverID)}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(90 * 24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, certTmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		ca.clearPending(record.serverID, digest)
		return NodeIdentity{}, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		ca.clearPending(record.serverID, digest)
		return NodeIdentity{}, err
	}
	fp := sha256.Sum256(cert.Raw)
	fingerprintValue := base64.RawURLEncoding.EncodeToString(fp[:])
	ca.mu.Lock()
	// The pending reservation prevents another enrollment from racing this
	// certificate into ownership. Publish ownership before returning it.
	delete(ca.pending, record.serverID)
	ca.certificates[record.serverID] = fingerprintValue
	if err := ca.persistLocked(); err != nil {
		delete(ca.certificates, record.serverID)
		ca.mu.Unlock()
		return NodeIdentity{}, fmt.Errorf("persist certificate ownership: %w", err)
	}
	ca.mu.Unlock()
	return NodeIdentity{ServerID: record.serverID, CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), PrivateKeyPEM: marshalKey(key), NotAfter: cert.NotAfter, Fingerprint: fingerprintValue}, nil
}

func (ca *CertificateAuthority) clearPending(serverID contracts.ServerID, digest [sha256.Size]byte) {
	ca.mu.Lock()
	delete(ca.pending, serverID)
	if ownedDigest, ok := ca.serverEnrollments[serverID]; ok && ownedDigest == digest {
		delete(ca.serverEnrollments, serverID)
	}
	_ = ca.persistLocked()
	ca.mu.Unlock()
}

func marshalKey(k *ecdsa.PrivateKey) []byte {
	b, _ := x509.MarshalPKCS8PrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})
}
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func constantEqual(a, b string) bool { return len(a) == len(b) && subtleCompare([]byte(a), []byte(b)) }
func subtleCompare(a, b []byte) bool {
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func (ca *CertificateAuthority) Revoke(identity NodeIdentity) error {
	fp := identity.Fingerprint
	serverID := identity.ServerID
	if len(identity.CertificatePEM) > 0 {
		block, _ := pem.Decode(identity.CertificatePEM)
		if block != nil {
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
				// The certificate subject is authoritative for the callback
				// target. Do not leave a caller-mutated ServerID pointing at a
				// different live channel.
				fp = fingerprint(cert.Raw)
				if cert.Subject.CommonName != "" {
					serverID = contracts.ServerID(cert.Subject.CommonName)
				}
			}
		}
	}
	if fp == "" {
		return errors.New("invalid certificate fingerprint")
	}
	ca.mu.Lock()
	wasRevoked := false
	if _, exists := ca.revoked[fp]; exists {
		wasRevoked = true
	}
	previousCertificate, hadCertificate := ca.certificates[serverID]
	ca.revoked[fp] = struct{}{}
	if current, ok := ca.certificates[serverID]; ok && current == fp {
		// Revocation explicitly releases the server for an authenticated
		// recovery/re-enrollment flow, while the revoked fingerprint remains
		// denied by Verify.
		delete(ca.certificates, serverID)
	}
	if err := ca.persistLocked(); err != nil {
		if !wasRevoked {
			delete(ca.revoked, fp)
		}
		if hadCertificate {
			ca.certificates[serverID] = previousCertificate
		}
		ca.mu.Unlock()
		return fmt.Errorf("persist certificate revocation: %w", err)
	}
	listeners := append([]func(contracts.ServerID, string){}, ca.revocationListeners...)
	ca.mu.Unlock()
	// Notify hubs after releasing the CA lock. A listener may close a live
	// connection and can therefore call back into certificate verification.
	for _, listener := range listeners {
		listener(serverID, fp)
	}
	return nil
}

// addRevocationListener lets a hub terminate a connection as soon as its
// certificate is revoked. The callback is deliberately process-local, just
// like the CA's current in-memory ownership state.
func (ca *CertificateAuthority) addRevocationListener(listener func(contracts.ServerID, string)) {
	if listener == nil {
		return
	}
	ca.mu.Lock()
	ca.revocationListeners = append(ca.revocationListeners, listener)
	ca.mu.Unlock()
}

func certificateFingerprintPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("invalid certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	return fingerprint(cert.Raw), nil
}
func (ca *CertificateAuthority) Verify(certPEM []byte, now time.Time) (contracts.ServerID, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("invalid certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if _, ok := ca.revoked[fingerprint(cert.Raw)]; ok {
		return "", errors.New("certificate_revoked")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err = cert.Verify(x509.VerifyOptions{Roots: pool, Intermediates: x509.NewCertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: now}); err != nil {
		return "", errors.New("certificate_untrusted")
	}
	if cert.Subject.CommonName == "" || strings.ContainsAny(cert.Subject.CommonName, "/\\") {
		return "", errors.New("invalid node identity")
	}
	serverID := contracts.ServerID(cert.Subject.CommonName)
	if ownedFingerprint, ok := ca.certificates[serverID]; !ok || ownedFingerprint != fingerprint(cert.Raw) {
		return "", errors.New("certificate_not_registered")
	}
	return serverID, nil
}
func fingerprint(raw []byte) string {
	s := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(s[:])
}

type persistedIdentityState struct {
	PrivateKey        []byte                               `json:"private_key"`
	Certificate       []byte                               `json:"certificate"`
	Revoked           []string                             `json:"revoked,omitempty"`
	Enrollments       map[string]persistedEnrollmentRecord `json:"enrollments,omitempty"`
	ServerEnrollments map[string]string                    `json:"server_enrollments,omitempty"`
	Certificates      map[string]string                    `json:"certificates,omitempty"`
}

type persistedEnrollmentRecord struct {
	ServerID  contracts.ServerID `json:"server_id"`
	ExpiresAt time.Time          `json:"expires_at"`
	Consumed  bool               `json:"consumed"`
}

func (ca *CertificateAuthority) persistLocked() error {
	if ca.repository == nil {
		return nil
	}
	key, err := x509.MarshalPKCS8PrivateKey(ca.key)
	if err != nil {
		return err
	}
	state := persistedIdentityState{PrivateKey: key, Certificate: ca.cert.Raw, Enrollments: make(map[string]persistedEnrollmentRecord, len(ca.enrollments)), ServerEnrollments: make(map[string]string, len(ca.serverEnrollments)), Certificates: make(map[string]string, len(ca.certificates))}
	for fp := range ca.revoked {
		state.Revoked = append(state.Revoked, fp)
	}
	for digest, record := range ca.enrollments {
		state.Enrollments[base64.RawURLEncoding.EncodeToString(digest[:])] = persistedEnrollmentRecord{ServerID: record.serverID, ExpiresAt: record.expiresAt, Consumed: record.consumed}
	}
	for serverID, digest := range ca.serverEnrollments {
		state.ServerEnrollments[string(serverID)] = base64.RawURLEncoding.EncodeToString(digest[:])
	}
	for serverID, fp := range ca.certificates {
		state.Certificates[string(serverID)] = fp
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return ca.repository.SaveIdentityState(data)
}

func (ca *CertificateAuthority) restoreLocked(data []byte) error {
	var state persistedIdentityState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(state.PrivateKey)
	if err != nil {
		return err
	}
	key, ok := parsedKey.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return errors.New("invalid enrollment CA key")
	}
	cert, err := x509.ParseCertificate(state.Certificate)
	if err != nil || !cert.IsCA {
		return errors.New("invalid enrollment CA certificate")
	}
	if len(state.Enrollments) > 4096 || len(state.Revoked) > 10000 || len(state.Certificates) > 4096 {
		return errors.New("enrollment authority exceeds bounds")
	}
	ca.key, ca.cert, ca.revoked = key, cert, make(map[string]struct{}, len(state.Revoked))
	ca.enrollments = make(map[[sha256.Size]byte]enrollmentRecord, len(state.Enrollments))
	ca.serverEnrollments = make(map[contracts.ServerID][sha256.Size]byte, len(state.ServerEnrollments))
	ca.certificates = make(map[contracts.ServerID]string, len(state.Certificates))
	ca.pending = make(map[contracts.ServerID]struct{})
	for _, fp := range state.Revoked {
		if fp == "" {
			return errors.New("invalid revoked fingerprint")
		}
		ca.revoked[fp] = struct{}{}
	}
	for encoded, record := range state.Enrollments {
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(raw) != sha256.Size || record.ServerID == "" {
			return errors.New("invalid persisted enrollment")
		}
		var digest [sha256.Size]byte
		copy(digest[:], raw)
		ca.enrollments[digest] = enrollmentRecord{serverID: record.ServerID, expiresAt: record.ExpiresAt, consumed: record.Consumed}
	}
	for serverID, encoded := range state.ServerEnrollments {
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(raw) != sha256.Size {
			return errors.New("invalid server enrollment ownership")
		}
		var digest [sha256.Size]byte
		copy(digest[:], raw)
		if record, exists := ca.enrollments[digest]; !exists || string(record.serverID) != serverID {
			return errors.New("orphaned server enrollment ownership")
		}
		ca.serverEnrollments[contracts.ServerID(serverID)] = digest
	}
	for serverID, fp := range state.Certificates {
		if serverID == "" || fp == "" {
			return errors.New("invalid certificate ownership")
		}
		ca.certificates[contracts.ServerID(serverID)] = fp
	}
	return nil
}

// RevokeServer revokes the currently active certificate for a server if one exists.
func (ca *CertificateAuthority) RevokeServer(serverID contracts.ServerID) error {
	ca.mu.Lock()
	fp, ok := ca.certificates[serverID]
	ca.mu.Unlock()
	if !ok {
		return nil
	}
	return ca.Revoke(NodeIdentity{ServerID: serverID, Fingerprint: fp})
}

// IssueNodeIdentity directly provisions a valid NodeIdentity for a server,
// revoking any prior certificate and clearing stale enrollments.
func (ca *CertificateAuthority) IssueNodeIdentity(serverID contracts.ServerID, now time.Time) (NodeIdentity, error) {
	if len(serverID) < 16 || len(serverID) > 128 {
		return NodeIdentity{}, errors.New("invalid server id")
	}
	_ = ca.RevokeServer(serverID)
	ca.mu.Lock()
	if prevDigest, exists := ca.serverEnrollments[serverID]; exists {
		delete(ca.enrollments, prevDigest)
		delete(ca.serverEnrollments, serverID)
	}
	delete(ca.pending, serverID)
	ca.mu.Unlock()

	enrollment, err := ca.IssueEnrollment(serverID, now)
	if err != nil {
		return NodeIdentity{}, err
	}
	return ca.ConsumeEnrollmentToken(enrollment.Token, now)
}
