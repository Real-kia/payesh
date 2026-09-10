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
	"encoding/pem"
	"errors"
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
}

type enrollmentRecord struct {
	serverID  contracts.ServerID
	expiresAt time.Time
	consumed  bool
}

func NewCertificateAuthority(now time.Time) (*CertificateAuthority, error) {
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
	return &CertificateAuthority{
		key:               key,
		cert:              cert,
		revoked:           make(map[string]struct{}),
		enrollments:       make(map[[sha256.Size]byte]enrollmentRecord),
		serverEnrollments: make(map[contracts.ServerID][sha256.Size]byte),
		certificates:      make(map[contracts.ServerID]string),
		pending:           make(map[contracts.ServerID]struct{}),
	}, nil
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

// IssueEnrollment returns a short-lived, single-use pairing token. Tokens are
// stored as digests and are never logged or returned after Consume succeeds.
func (ca *CertificateAuthority) IssueEnrollment(serverID contracts.ServerID, now time.Time) (Enrollment, error) {
	if len(serverID) < 16 || len(serverID) > 128 {
		return Enrollment{}, errors.New("invalid server id")
	}
	token := randomToken(32)
	expiresAt := now.Add(10 * time.Minute)
	digest := sha256.Sum256([]byte(token))
	ca.mu.Lock()
	if _, enrolled := ca.certificates[serverID]; enrolled {
		ca.mu.Unlock()
		return Enrollment{}, errors.New("server_already_enrolled")
	}
	if _, enrolling := ca.pending[serverID]; enrolling {
		ca.mu.Unlock()
		return Enrollment{}, errors.New("server_enrollment_in_progress")
	}
	if previousDigest, exists := ca.serverEnrollments[serverID]; exists {
		previous := ca.enrollments[previousDigest]
		if !previous.consumed && now.Before(previous.expiresAt) {
			ca.mu.Unlock()
			return Enrollment{}, errors.New("enrollment_already_issued")
		}
		// Expired or failed enrollments no longer reserve the server. Keep the
		// consumed digest in the CA map so a copied caller value cannot replay it.
		delete(ca.serverEnrollments, serverID)
	}
	ca.enrollments[digest] = enrollmentRecord{serverID: serverID, expiresAt: expiresAt}
	ca.serverEnrollments[serverID] = digest
	ca.mu.Unlock()
	return Enrollment{ServerID: serverID, Token: token, ExpiresAt: expiresAt}, nil
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
		return NodeIdentity{}, errors.New("server_already_enrolled")
	}
	if _, enrolling := ca.pending[record.serverID]; enrolling {
		ca.mu.Unlock()
		return NodeIdentity{}, errors.New("server_enrollment_in_progress")
	}
	// Consumption is authoritative CA state, not caller-owned Enrollment
	// state. Mark it before generating credentials so concurrent calls and
	// copies of the original value cannot replay the pairing token.
	record.consumed = true
	ca.enrollments[digest] = record
	ca.pending[record.serverID] = struct{}{}
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
	ca.mu.Unlock()
	return NodeIdentity{ServerID: record.serverID, CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), PrivateKeyPEM: marshalKey(key), NotAfter: cert.NotAfter, Fingerprint: fingerprintValue}, nil
}

func (ca *CertificateAuthority) clearPending(serverID contracts.ServerID, digest [sha256.Size]byte) {
	ca.mu.Lock()
	delete(ca.pending, serverID)
	if ownedDigest, ok := ca.serverEnrollments[serverID]; ok && ownedDigest == digest {
		delete(ca.serverEnrollments, serverID)
	}
	ca.mu.Unlock()
}

func marshalKey(k *ecdsa.PrivateKey) []byte {
	b, _ := x509.MarshalPKCS8PrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})
}
func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func constantEqual(a, b string) bool { return len(a) == len(b) && subtleCompare([]byte(a), []byte(b)) }
func subtleCompare(a, b []byte) bool {
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func (ca *CertificateAuthority) Revoke(identity NodeIdentity) {
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
		return
	}
	ca.mu.Lock()
	ca.revoked[fp] = struct{}{}
	if current, ok := ca.certificates[serverID]; ok && current == fp {
		// Revocation explicitly releases the server for an authenticated
		// recovery/re-enrollment flow, while the revoked fingerprint remains
		// denied by Verify.
		delete(ca.certificates, serverID)
	}
	listeners := append([]func(contracts.ServerID, string){}, ca.revocationListeners...)
	ca.mu.Unlock()
	// Notify hubs after releasing the CA lock. A listener may close a live
	// connection and can therefore call back into certificate verification.
	for _, listener := range listeners {
		listener(serverID, fp)
	}
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
