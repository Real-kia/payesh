// Package release contains small, offline release-authoring helpers.
//
// It intentionally never provides a key-generation or key-storage workflow.
// Signing keys are operator-owned inputs and are read only for the duration
// of a signing operation.
package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateKeyID applies the release contract's deliberately narrow key ID
// syntax. It must never be interpreted as a path or shell fragment.
func ValidateKeyID(keyID string) error {
	if !keyIDPattern.MatchString(keyID) {
		return errors.New("key ID must be 1-128 characters: letters, digits, '.', '_' or '-' (starting with a letter or digit)")
	}
	return nil
}

// ReadPrivateKey reads a raw, hex, or base64-encoded Ed25519 seed/private key
// from an owner-only regular file. A 32-byte seed and 64-byte private key are
// accepted; no key material is logged or persisted by this package.
func ReadPrivateKey(path string) (ed25519.PrivateKey, error) {
	body, err := readRestrictedKeyFile(path, "private key")
	if err != nil {
		return nil, err
	}
	decoded, err := decodeKeyMaterial(body, 32, 64)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	if len(decoded) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(decoded), nil
	}
	private := ed25519.PrivateKey(append([]byte(nil), decoded...))
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok || !bytes.Equal(private[ed25519.SeedSize:], public) {
		return nil, errors.New("private key: 64-byte key has an inconsistent public half")
	}
	return private, nil
}

// ReadPublicKey reads a raw, hex, or base64-encoded 32-byte Ed25519 public
// key from a regular file. Public keys need not be owner-only, but symlinks,
// directories, and oversized files are rejected to avoid ambiguous anchors.
func ReadPublicKey(path string) (ed25519.PublicKey, error) {
	body, err := readKeyFile(path, "public key", false)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeKeyMaterial(body, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	return ed25519.PublicKey(append([]byte(nil), decoded...)), nil
}

func readRestrictedKeyFile(path, label string) ([]byte, error) {
	return readKeyFile(path, label, true)
}

func readKeyFile(path, label string, ownerOnly bool) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%s path is required", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s path: %w", label, err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s must not be a symlink", label)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", label)
	}
	if ownerOnly && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s permissions must be owner-only (0600 or stricter)", label)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && uint64(stat.Uid) != uint64(os.Getuid()) {
		return nil, fmt.Errorf("%s must be owned by the current user", label)
	}
	if info.Size() <= 0 || info.Size() > 4096 {
		return nil, fmt.Errorf("%s has an unreasonable size", label)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if len(body) > 4096 {
		return nil, fmt.Errorf("%s is too large", label)
	}
	return body, nil
}

func decodeKeyMaterial(body []byte, sizes ...int) ([]byte, error) {
	for _, size := range sizes {
		if len(body) == size {
			return body, nil
		}
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil, errors.New("key is empty")
	}
	decoders := []func(string) ([]byte, error){
		hex.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.StdEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
	}
	for _, decode := range decoders {
		decoded, err := decode(text)
		if err != nil {
			continue
		}
		for _, size := range sizes {
			if len(decoded) == size {
				return decoded, nil
			}
		}
	}
	return nil, errors.New("must be a 32/64-byte raw key or an equivalent hex/base64 encoding")
}
