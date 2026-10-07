package cutoverpeer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// WriteCredentials creates a self-signed ECDSA P-256 certificate and private key
// for a cutover client or peer and returns the certificate fingerprint to give to
// the other side. It refuses to overwrite existing files, and the private key is
// written with mode 0600.
func WriteCredentials(certPath, keyPath, commonName string, notAfter time.Time) (string, error) {
	if commonName == "" || !notAfter.After(time.Now()) {
		return "", errors.New("cutoverpeer: a common name and a future expiry are required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return "", err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:    []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("cutoverpeer: create private key: %w", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); err != nil {
		_ = keyFile.Close()
		_ = os.Remove(keyPath)
		return "", err
	}
	if err := keyFile.Close(); err != nil {
		_ = os.Remove(keyPath)
		return "", err
	}
	certFile, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		_ = os.Remove(keyPath)
		return "", fmt.Errorf("cutoverpeer: create certificate: %w", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		_ = certFile.Close()
		_ = os.Remove(certPath)
		_ = os.Remove(keyPath)
		return "", err
	}
	if err := certFile.Close(); err != nil {
		_ = os.Remove(certPath)
		_ = os.Remove(keyPath)
		return "", err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return "", err
	}
	return Fingerprint(parsed), nil
}

// LoadCredentials loads a certificate and key pair, refusing a private key that is
// readable by anyone but its owner or a certificate that has expired.
func LoadCredentials(certPath, keyPath string) (tls.Certificate, string, error) {
	info, err := os.Stat(keyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return tls.Certificate{}, "", errors.New("cutoverpeer: the private key must be a regular file readable only by its owner")
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if !time.Now().Before(leaf.NotAfter) {
		return tls.Certificate{}, "", errors.New("cutoverpeer: the certificate has expired")
	}
	return certificate, Fingerprint(leaf), nil
}
