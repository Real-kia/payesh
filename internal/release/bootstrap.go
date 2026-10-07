package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var bootstrapVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?$`)
var checksumName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// BootstrapPayload binds the exact checksum index to its release and external
// key identifier. The signature authenticates bootstrap bytes before executing
// any downloaded release code; it complements the canonical JSON manifest.
func BootstrapPayload(version, keyID string, checksums []byte) ([]byte, error) {
	if !bootstrapVersion.MatchString(version) {
		return nil, errors.New("invalid bootstrap release version")
	}
	if err := ValidateKeyID(keyID); err != nil {
		return nil, err
	}
	if keyID == "unavailable-local" {
		return nil, errors.New("unsigned key ID cannot authenticate a release")
	}
	if _, err := ParseChecksums(checksums); err != nil {
		return nil, err
	}
	return append([]byte("payesh.checksums.v1\n"+version+"\n"+keyID+"\n"), checksums...), nil
}

func ParseChecksums(body []byte) (map[string]string, error) {
	if len(body) == 0 || len(body) > 1<<20 || body[len(body)-1] != '\n' {
		return nil, errors.New("invalid checksum index size or termination")
	}
	entries := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		parts := strings.Split(line, "  ")
		if len(parts) != 2 || len(parts[0]) != 64 || parts[0] != strings.ToLower(parts[0]) || !checksumName.MatchString(parts[1]) {
			return nil, errors.New("invalid checksum index entry")
		}
		if _, err := hex.DecodeString(parts[0]); err != nil {
			return nil, errors.New("invalid checksum index digest")
		}
		if _, ok := entries[parts[1]]; ok {
			return nil, errors.New("duplicate checksum index entry")
		}
		entries[parts[1]] = parts[0]
	}
	return entries, nil
}

func VerifyBootstrap(public ed25519.PublicKey, version, keyID string, checksums, signature []byte) error {
	payload, err := BootstrapPayload(version, keyID, checksums)
	if err != nil {
		return err
	}
	if len(signature) != base64.RawURLEncoding.EncodedLen(ed25519.SignatureSize) || strings.ContainsAny(string(signature), "\r\n\t ") {
		return errors.New("invalid bootstrap detached signature length or whitespace")
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(string(signature))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("invalid bootstrap detached signature")
	}
	if len(public) != ed25519.PublicKeySize || !ed25519.Verify(public, payload, sig) {
		return errors.New("bootstrap signature verification failed")
	}
	return nil
}

func VerifyChecksumFile(checksums []byte, name string, body []byte) error {
	entries, err := ParseChecksums(checksums)
	if err != nil {
		return err
	}
	digest, ok := entries[name]
	if !ok {
		return fmt.Errorf("authenticated checksum index does not contain %s", name)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("authenticated checksum mismatch for %s", name)
	}
	return nil
}
