package trust

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"
)

type sample struct {
	Zebra string   `json:"zebra"`
	Alpha string   `json:"alpha"`
	Nums  []string `json:"nums"`
}

func TestCanonicalizeSortsKeysAndIsOrderInsensitive(t *testing.T) {
	a := map[string]any{"b": "2", "a": "1", "c": map[string]any{"y": "1", "x": "2"}}
	b := map[string]any{"c": map[string]any{"x": "2", "y": "1"}, "a": "1", "b": "2"}
	ca, err := Canonicalize(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := Canonicalize(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(cb) {
		t.Fatalf("expected identical canonical bytes, got %q vs %q", ca, cb)
	}
	want := `{"a":"1","b":"2","c":{"x":"2","y":"1"}}`
	if string(ca) != want {
		t.Fatalf("got %q want %q", ca, want)
	}
}

func TestCanonicalizeStructUsesJSONFieldOrderThenSorts(t *testing.T) {
	// Struct field order is Zebra, Alpha, Nums but canonical output must sort
	// by key regardless of Go struct declaration order.
	out, err := Canonicalize(sample{Zebra: "z", Alpha: "a", Nums: []string{"1", "2"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"alpha":"a","nums":["1","2"],"zebra":"z"}`
	if string(out) != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestCanonicalizeEscapesControlCharactersAndQuotes(t *testing.T) {
	out, err := Canonicalize(map[string]any{"k": "a\"b\\c\nd\te"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"k":"a\"b\\c\nd\te"}`
	if string(out) != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestCanonicalizeRejectsRawNumbers(t *testing.T) {
	if _, err := Canonicalize(map[string]any{"n": 1}); err != ErrUnsupportedNumber {
		t.Fatalf("expected ErrUnsupportedNumber, got %v", err)
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(Anchor{KeyID: "release-2026", PublicKey: pub, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	doc := sample{Zebra: "z", Alpha: "a", Nums: []string{"1"}}
	canonical, sig, err := Sign(priv, doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Verify("release-2026", canonical, sig, time.Now()); err != nil {
		t.Fatalf("expected verification to succeed: %v", err)
	}
}

func TestVerifyRejectsTamperedBytes(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := NewRegistry(Anchor{KeyID: "k1", PublicKey: pub, ExpiresAt: time.Now().Add(time.Hour)})
	canonical, sig, err := Sign(priv, sample{Zebra: "z"})
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte{}, canonical...)
	tampered[0] = 'X'
	if err := registry.Verify("k1", tampered, sig, time.Now()); err == nil {
		t.Fatal("expected tampered document to fail verification")
	}
}

func TestVerifyRejectsUnknownExpiredAndRevokedKeys(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := NewRegistry(Anchor{KeyID: "old", PublicKey: pub, ExpiresAt: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := NewRegistry(Anchor{KeyID: "old", PublicKey: pub, ExpiresAt: time.Now().Add(time.Hour), Revoked: true})
	if err != nil {
		t.Fatal(err)
	}
	canonical, sig, err := Sign(priv, sample{Zebra: "z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := expired.Verify("missing", canonical, sig, time.Now()); err == nil {
		t.Fatal("expected unknown key to be rejected")
	}
	if err := expired.Verify("old", canonical, sig, time.Now()); err == nil {
		t.Fatal("expected expired key to be rejected")
	}
	if err := revoked.Verify("old", canonical, sig, time.Now()); err == nil {
		t.Fatal("expected revoked key to be rejected")
	}
}

func TestVerifyRejectsMalformedSignature(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := NewRegistry(Anchor{KeyID: "k1", PublicKey: pub, ExpiresAt: time.Now().Add(time.Hour)})
	canonical, err := Canonicalize(sample{Zebra: "z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Verify("k1", canonical, "not-base64url!!", time.Now()); err == nil {
		t.Fatal("expected malformed base64url to be rejected")
	}
	shortSig := base64.RawURLEncoding.EncodeToString([]byte("too-short"))
	if err := registry.Verify("k1", canonical, shortSig, time.Now()); err == nil {
		t.Fatal("expected short signature to be rejected")
	}
}

func TestNewRegistryRejectsInvalidAnchors(t *testing.T) {
	if _, err := NewRegistry(Anchor{KeyID: "", PublicKey: make([]byte, ed25519.PublicKeySize)}); err == nil {
		t.Fatal("expected empty key_id to be rejected")
	}
	if _, err := NewRegistry(Anchor{KeyID: "k1", PublicKey: []byte{1, 2, 3}}); err == nil {
		t.Fatal("expected short public key to be rejected")
	}
}
