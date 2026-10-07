package release

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOperatorPKCS8PEMPrivateKey(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "operator.key")
	body := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("test-only signing fixture")
	if !ed25519.Verify(public, message, ed25519.Sign(loaded, message)) {
		t.Fatal("PKCS8 signer did not match public anchor")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateKey(path); err == nil {
		t.Fatal("insecure PKCS8 private key permissions accepted")
	}
}

func TestPrivateKeyRejectsWrongPEMAndInconsistentRawKey(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(private)
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherDER, _ := x509.MarshalPKCS8PrivateKey(other)
	valid := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	corruptRaw := append([]byte(nil), private...)
	corruptRaw[len(corruptRaw)-1] ^= 1
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"wrong-algorithm", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherDER})},
		{"encrypted-block", pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: der})},
		{"extra-block", append(append([]byte(nil), valid...), valid...)},
		{"malformed-pkcs8", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("invalid")})},
		{"inconsistent-raw", corruptRaw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "operator.key")
			if err := os.WriteFile(path, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadPrivateKey(path); err == nil {
				t.Fatal("invalid private key accepted")
			}
		})
	}
}
