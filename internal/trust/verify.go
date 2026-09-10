package trust

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// Anchor is one trusted Ed25519 signing key. Revoked/expired anchors remain
// in the registry (for audit and error messages) but never verify.
type Anchor struct {
	KeyID     string
	PublicKey ed25519.PublicKey
	ExpiresAt time.Time
	Revoked   bool
}

// Registry holds the pinned trust anchors an installed binary ships with.
// It is deliberately small and in-memory: rotation/revocation is a documented
// operational action (see docs/contracts/RELEASE_FORMAT.md), not a runtime
// API that lets a request add its own key.
type Registry struct {
	anchors map[string]Anchor
}

func NewRegistry(anchors ...Anchor) (*Registry, error) {
	r := &Registry{anchors: make(map[string]Anchor, len(anchors))}
	for _, a := range anchors {
		if a.KeyID == "" {
			return nil, errors.New("trust: anchor key_id is required")
		}
		if len(a.PublicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("trust: anchor %q has an invalid public key length", a.KeyID)
		}
		r.anchors[a.KeyID] = a
	}
	return r, nil
}

// Verify checks a detached Ed25519 signature over already-canonicalized
// bytes, per docs/contracts/RELEASE_FORMAT.md: the signature file holds
// exactly 64 raw bytes, base64url-encoded without padding.
func (r *Registry) Verify(keyID string, canonical []byte, signatureB64 string, now time.Time) error {
	if r == nil {
		return errors.New("trust: registry is not configured")
	}
	anchor, ok := r.anchors[keyID]
	if !ok {
		return fmt.Errorf("trust: unknown signing key %q", keyID)
	}
	if anchor.Revoked {
		return fmt.Errorf("trust: signing key %q is revoked", keyID)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !anchor.ExpiresAt.IsZero() && now.After(anchor.ExpiresAt) {
		return fmt.Errorf("trust: signing key %q expired at %s", keyID, anchor.ExpiresAt.Format(time.RFC3339))
	}
	sig, err := base64.RawURLEncoding.DecodeString(signatureB64)
	if err != nil {
		return errors.New("trust: signature is not valid unpadded base64url")
	}
	if len(sig) != ed25519.SignatureSize {
		return errors.New("trust: signature has an invalid length")
	}
	if !ed25519.Verify(anchor.PublicKey, canonical, sig) {
		return errors.New("trust: signature verification failed")
	}
	return nil
}

// Sign is a test/tooling helper: it canonicalizes v and produces the same
// detached, unpadded-base64url signature format Verify expects. Production
// signing keys never live in this repository or ordinary CI; this exists so
// tests and local catalog-authoring tooling can produce valid fixtures.
func Sign(priv ed25519.PrivateKey, v any) (canonical []byte, signatureB64 string, err error) {
	canonical, err = Canonicalize(v)
	if err != nil {
		return nil, "", err
	}
	sig := ed25519.Sign(priv, canonical)
	return canonical, base64.RawURLEncoding.EncodeToString(sig), nil
}
