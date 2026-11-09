package alerts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Vault encrypts destination secrets (URL, token, header values) at rest with
// AES-256-GCM. The key lives in a root/service-owned 0600 file next to the
// database, so a copied or leaked database alone does not expose them.
type Vault struct {
	aead cipher.AEAD
}

const vaultKeyBytes = 32

// OpenExistingVault loads the key at path without ever creating it. Command
// line tools use this so a run as another user cannot leave a key file the
// service cannot read. It returns (nil, nil) when no key exists yet.
func OpenExistingVault(path string) (*Vault, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return OpenVault(path)
}

// OpenVault loads the key at path, creating it with mode 0600 on first use.
func OpenVault(path string) (*Vault, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, vaultKeyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				// Another process created it first.
				return OpenVault(path)
			}
			return nil, err
		}
		if _, err := file.Write(key); err != nil {
			file.Close()
			_ = os.Remove(path)
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != vaultKeyBytes {
		return nil, errors.New("notification secret key file is invalid")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

// destinationSecrets is everything about a destination that must not be read
// back or logged.
type destinationSecrets struct {
	URL     string            `json:"url"`
	Secret  string            `json:"secret,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (v *Vault) seal(id string, secrets destinationSecrets) ([]byte, error) {
	plain, err := json.Marshal(secrets)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// The destination ID is authenticated, so a blob cannot be moved to
	// another row.
	return v.aead.Seal(nonce, nonce, plain, []byte(id)), nil
}

func (v *Vault) open(id string, blob []byte) (destinationSecrets, error) {
	var secrets destinationSecrets
	if len(blob) < v.aead.NonceSize() {
		return secrets, errors.New("stored secrets are unreadable")
	}
	nonce, sealed := blob[:v.aead.NonceSize()], blob[v.aead.NonceSize():]
	plain, err := v.aead.Open(nil, nonce, sealed, []byte(id))
	if err != nil {
		return secrets, errors.New("stored secrets cannot be decrypted with the current key")
	}
	err = json.Unmarshal(plain, &secrets)
	return secrets, err
}
