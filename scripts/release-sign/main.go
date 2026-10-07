// Command release-sign authorizes an already-built local release bundle.
//
// The private key is always supplied by the operator at invocation time and
// is never generated, copied, or written by this command. The manifest's key
// ID is changed from unavailable-local, canonicalized with the repository JCS
// implementation, and signed in the release contract's detached format.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/release"
	"github.com/Real-kia/payesh/internal/trust"
)

type artifact struct {
	Name            string `json:"name"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	SHA256          string `json:"sha256"`
	CompressedBytes uint64 `json:"compressed_bytes,string"`
	UnpackedBytes   uint64 `json:"unpacked_bytes,string"`
	URL             string `json:"url"`
}

type manifest struct {
	DatabaseSchema *contracts.ReleaseDatabaseSchema `json:"database_schema,omitempty"`
	Format         string                           `json:"format"`
	Release        string                           `json:"release"`
	CreatedAt      interface{}                      `json:"created_at"`
	MinCore        string                           `json:"min_core"`
	Artifacts      []artifact                       `json:"artifacts"`
	SigningKeyID   string                           `json:"signing_key_id"`
}

// canonicalManifest is kept separate from the loose decoding type above so
// timestamps retain the exact time.Time JSON representation used by the
// package builder and validator.
type canonicalManifest struct {
	DatabaseSchema *contracts.ReleaseDatabaseSchema `json:"database_schema,omitempty"`
	Format         string                           `json:"format"`
	Release        string                           `json:"release"`
	CreatedAt      timeValue                        `json:"created_at"`
	MinCore        string                           `json:"min_core"`
	Artifacts      []artifact                       `json:"artifacts"`
	SigningKeyID   string                           `json:"signing_key_id"`
}

// timeValue delegates JSON parsing/marshaling to time.Time without exposing
// another wire format. It exists only to keep the manifest declarations local
// to this standalone command.
type timeValue struct{ raw string }

func (t *timeValue) UnmarshalJSON(b []byte) error {
	var value string
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	t.raw = value
	return nil
}

func (t timeValue) MarshalJSON() ([]byte, error) {
	value, err := time.Parse(time.RFC3339, t.raw)
	if err != nil {
		return nil, err
	}
	return value.MarshalJSON()
}

func main() {
	flags := flag.NewFlagSet("release-sign", flag.ExitOnError)
	dir := flags.String("dir", envOr("PAYESH_RELEASE_DIR", ""), "existing release directory to sign")
	keyPath := flags.String("key", "", "operator-owned Ed25519 private key file (required)")
	keyID := flags.String("key-id", "", "trusted release signing key ID (required)")
	flags.Parse(os.Args[1:])
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "release-sign: positional arguments are not accepted; use -dir")
		os.Exit(2)
	}
	if err := sign(*dir, *keyPath, *keyID); err != nil {
		fmt.Fprintln(os.Stderr, "release-sign:", err)
		os.Exit(1)
	}
}

func sign(dir, keyPath, keyID string) error {
	if err := release.ValidateKeyID(keyID); err != nil {
		return err
	}
	if keyID == "unavailable-local" {
		return errors.New("key ID unavailable-local is reserved for unsigned local bundles")
	}
	releaseDir, err := safeReleaseDir(dir)
	if err != nil {
		return err
	}
	privateKey, err := release.ReadPrivateKey(keyPath)
	if err != nil {
		return err
	}
	defer func() {
		for i := range privateKey {
			privateKey[i] = 0
		}
	}()
	manifestPath := filepath.Join(releaseDir, "manifest.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return fmt.Errorf("inspect manifest: %w", err)
	}
	if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
		return errors.New("manifest.json must be a regular file and not a symlink")
	}
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var m canonicalManifest
	if err := decodeStrict(body, &m); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if m.DatabaseSchema != nil {
		if err := m.DatabaseSchema.Validate(); err != nil {
			return err
		}
	}
	if m.Format != "payesh.release.v1" || m.Release == "" || m.MinCore == "" || m.rawCreatedAt() == "" || len(m.Artifacts) == 0 {
		return errors.New("manifest is not a complete payesh.release.v1 bundle")
	}
	if _, err := time.Parse(time.RFC3339, m.rawCreatedAt()); err != nil {
		return fmt.Errorf("manifest created_at is not RFC3339: %w", err)
	}
	if m.SigningKeyID != "unavailable-local" {
		return errors.New("manifest is already signed or has an unexpected signing_key_id")
	}
	// Do not allow a pre-existing signature to be silently replaced. This also
	// rejects a symlink at the destination before any manifest mutation.
	sigPath := filepath.Join(releaseDir, "manifest.json.sig")
	if info, statErr := os.Lstat(sigPath); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("manifest.json.sig is a symlink; refusing overwrite")
		}
		return errors.New("manifest.json.sig already exists; refusing overwrite")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect signature destination: %w", statErr)
	}
	m.SigningKeyID = keyID
	// Older local fixtures may contain only the canonical manifest. Production
	// bootstrap requires a separately authenticated checksum index and script.
	var bootstrapSignature []byte
	var reboundChecksums, originalChecksums []byte
	checksumPublished, complete := false, false
	defer func() {
		if checksumPublished && !complete {
			_ = atomicWrite(filepath.Join(releaseDir, "SHA256SUMS"), originalChecksums, 0644, true)
		}
	}()
	checksums, sumsErr := os.ReadFile(filepath.Join(releaseDir, "SHA256SUMS"))
	if sumsErr == nil {
		for _, name := range []string{"SHA256SUMS", "install.sh"} {
			info, err := os.Lstat(filepath.Join(releaseDir, name))
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("%s must be a regular non-symlink file", name)
			}
		}
		bootstrap, err := os.ReadFile(filepath.Join(releaseDir, "install.sh"))
		if err != nil {
			return fmt.Errorf("read bootstrap script: %w", err)
		}
		if err := release.VerifyChecksumFile(checksums, "install.sh", bootstrap); err != nil {
			return err
		}
		entries, err := release.ParseChecksums(checksums)
		if err != nil {
			return err
		}
		expectedEntries := len(m.Artifacts) + 1
		if m.DatabaseSchema != nil {
			expectedEntries++
		}
		if len(entries) != expectedEntries {
			return errors.New("bootstrap checksum inventory differs from manifest and install.sh")
		}
		for _, a := range m.Artifacts {
			if entries[filepath.Base(a.URL)] != a.SHA256 {
				return errors.New("bootstrap checksum inventory differs from manifest")
			}
		}
		if m.DatabaseSchema != nil {
			if err := release.VerifyChecksumFile(checksums, "manifest.json", body); err != nil {
				return err
			}
			signedBody := append(prettyManifest(m), '\n')
			digest := sha256.Sum256(signedBody)
			entries["manifest.json"] = hex.EncodeToString(digest[:])
			lines := make([]string, 0, len(entries))
			for name, digest := range entries {
				lines = append(lines, digest+"  "+name)
			}
			sort.Strings(lines)
			originalChecksums = checksums
			reboundChecksums = []byte(strings.Join(lines, "\n") + "\n")
			checksums = reboundChecksums
		}
		payload, err := release.BootstrapPayload(m.Release, keyID, checksums)
		if err != nil {
			return err
		}
		bootstrapSignature = []byte(base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)))
		if _, err := os.Lstat(filepath.Join(releaseDir, "SHA256SUMS.sig")); !os.IsNotExist(err) {
			return errors.New("SHA256SUMS.sig already exists or cannot be inspected")
		}
	} else if !os.IsNotExist(sumsErr) {
		return sumsErr
	} else if m.DatabaseSchema != nil {
		return errors.New("schema-declared releases require authenticated bootstrap checksum inventory")
	}
	canonical, err := trust.Canonicalize(m)
	if err != nil {
		return fmt.Errorf("canonicalize manifest: %w", err)
	}
	signature := ed25519.Sign(privateKey, canonical)
	signatureB64 := base64.RawURLEncoding.EncodeToString(signature)
	// Publish the updated manifest first, then the signature. If signature
	// publication fails, restore the original unsigned manifest atomically.
	if len(reboundChecksums) > 0 {
		if err := atomicWrite(filepath.Join(releaseDir, "SHA256SUMS"), reboundChecksums, 0644, true); err != nil {
			return err
		}
		checksumPublished = true
	}
	if err := atomicWrite(manifestPath, append(prettyManifest(m), '\n'), manifestInfo.Mode().Perm(), true); err != nil {
		return fmt.Errorf("write signed manifest: %w", err)
	}
	if err := atomicWrite(sigPath, []byte(signatureB64), 0o644, false); err != nil {
		_ = atomicWrite(manifestPath, body, manifestInfo.Mode().Perm(), true)
		return fmt.Errorf("write detached signature: %w", err)
	}
	if len(bootstrapSignature) > 0 {
		if err := atomicWrite(filepath.Join(releaseDir, "SHA256SUMS.sig"), bootstrapSignature, 0o644, false); err != nil {
			_ = os.Remove(sigPath)
			_ = atomicWrite(manifestPath, body, manifestInfo.Mode().Perm(), true)
			return fmt.Errorf("write bootstrap signature: %w", err)
		}
	}
	complete = true
	fmt.Printf("signed release=%s key_id=%s dir=%s\n", m.Release, keyID, releaseDir)
	return nil
}

func (m canonicalManifest) rawCreatedAt() string { return m.CreatedAt.raw }

func prettyManifest(m canonicalManifest) []byte {
	body, _ := json.MarshalIndent(m, "", "  ")
	return body
}

func decodeStrict(body []byte, dst any) error {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON values are not allowed")
		}
		return err
	}
	return nil
}

func safeReleaseDir(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("release directory is required")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve release directory: %w", err)
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return "", errors.New("refusing filesystem root as release directory")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("inspect release directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("release directory must be a regular directory and not a symlink")
	}
	return abs, nil
}

func atomicWrite(path string, body []byte, mode os.FileMode, replace bool) error {
	dir := filepath.Dir(path)
	if info, err := os.Lstat(dir); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("destination parent is not a regular directory")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("destination is not a regular file")
		}
		if !replace {
			return errors.New("destination already exists")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".release-write-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(body)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !replace {
			return errors.New("destination changed or already exists")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	return os.Rename(tmpPath, path)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
