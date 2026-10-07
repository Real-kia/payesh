// Command release-validate checks a locally generated Payesh release bundle.
// It verifies archive bytes, manifest sizes, checksums, and safe tar members.
// Signed bundles require an explicitly supplied public trust anchor. Signing
// remains a separate owner operation using a key outside this repository.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
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
	CreatedAt      time.Time                        `json:"created_at"`
	MinCore        string                           `json:"min_core"`
	Artifacts      []artifact                       `json:"artifacts"`
	SigningKeyID   string                           `json:"signing_key_id"`
}

func main() {
	flags := flag.NewFlagSet("release-validate", flag.ExitOnError)
	dir := flags.String("dir", envOr("PAYESH_RELEASE_DIR", "dist/releases/"+envOr("PAYESH_RELEASE_VERSION", "0.1.0")), "release directory")
	publicKeyPath := flags.String("public-key", os.Getenv("PAYESH_RELEASE_PUBLIC_KEY"), "explicit Ed25519 public-key anchor file (required for signed bundles)")
	keyID := flags.String("key-id", os.Getenv("PAYESH_RELEASE_KEY_ID"), "trusted signing key ID (required for signed bundles)")
	flags.Parse(os.Args[1:])
	if flags.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "release-validate: at most one positional release directory is accepted")
		os.Exit(2)
	}
	if flags.NArg() == 1 {
		*dir = flags.Arg(0)
	}
	if err := validateWithTrust(*dir, *publicKeyPath, *keyID); err != nil {
		fmt.Fprintln(os.Stderr, "release-validate:", err)
		os.Exit(1)
	}
	fmt.Printf("valid release bundle: %s\n", *dir)
}

func validate(dir string) error {
	return validateWithTrust(dir, "", "")
}

func validateWithTrust(dir, publicKeyPath, keyID string) error {
	if !filepath.IsAbs(dir) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		dir = abs
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var m manifest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("manifest has trailing JSON values")
		}
		return fmt.Errorf("decode manifest: %w", err)
	}
	if m.DatabaseSchema != nil {
		if err := m.DatabaseSchema.Validate(); err != nil {
			return err
		}
	}
	if m.Format != "payesh.release.v1" || m.Release == "" || m.MinCore == "" || m.CreatedAt.IsZero() || m.SigningKeyID == "" {
		return errors.New("manifest identity is not a complete payesh.release.v1 bundle")
	}
	if err := release.ValidateKeyID(m.SigningKeyID); err != nil {
		return fmt.Errorf("manifest signing_key_id: %w", err)
	}
	if len(m.Artifacts) == 0 {
		return errors.New("manifest has no artifacts")
	}
	seen := map[string]bool{}
	expectedChecksums := make([]string, 0, len(m.Artifacts))
	for _, a := range m.Artifacts {
		if a.OS != "linux" || (a.Arch != "amd64" && a.Arch != "arm64") || a.Name == "" || a.CompressedBytes == 0 || a.UnpackedBytes == 0 || len(a.SHA256) != sha256.Size*2 || strings.ToLower(a.SHA256) != a.SHA256 {
			return fmt.Errorf("invalid artifact metadata for %q/%q", a.Name, a.Arch)
		}
		key := a.Name + "\x00" + a.OS + "\x00" + a.Arch
		if seen[key] {
			return fmt.Errorf("duplicate artifact %s/%s", a.Name, a.Arch)
		}
		seen[key] = true
		filename := filepath.Base(a.URL)
		if filename == "." || filename == ".." || filename == "" || filename != filepath.Clean(filename) {
			return fmt.Errorf("artifact URL does not name a safe local archive: %q", a.URL)
		}
		archivePath := filepath.Join(dir, filename)
		info, statErr := os.Lstat(archivePath)
		if statErr != nil {
			return fmt.Errorf("artifact %s: %w", filename, statErr)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact %s is not a regular file", filename)
		}
		digest, size, err := digestFile(archivePath)
		if err != nil {
			return err
		}
		if digest != a.SHA256 || size != a.CompressedBytes {
			return fmt.Errorf("artifact %s digest/size does not match manifest", filename)
		}
		unpacked, err := validateArchive(archivePath, a.UnpackedBytes)
		if err != nil {
			return fmt.Errorf("artifact %s: %w", filename, err)
		}
		if unpacked != a.UnpackedBytes {
			return fmt.Errorf("artifact %s unpacked bytes=%d manifest=%d", filename, unpacked, a.UnpackedBytes)
		}
		expectedChecksums = append(expectedChecksums, a.SHA256+"  "+filename)
	}
	sigPath := filepath.Join(dir, "manifest.json.sig")
	sigInfo, sigErr := os.Lstat(sigPath)
	if sigErr == nil {
		if sigInfo.Mode()&os.ModeSymlink != 0 || !sigInfo.Mode().IsRegular() {
			return errors.New("detached signature must be a regular file and not a symlink")
		}
		if m.SigningKeyID == "unavailable-local" {
			return errors.New("signed bundle cannot use signing_key_id=unavailable-local")
		}
		if strings.TrimSpace(publicKeyPath) == "" || strings.TrimSpace(keyID) == "" {
			return errors.New("signed bundle requires explicit -public-key and -key-id")
		}
		if keyID != m.SigningKeyID {
			return fmt.Errorf("public-key key ID %q does not match manifest signing_key_id %q", keyID, m.SigningKeyID)
		}
		publicKey, err := release.ReadPublicKey(publicKeyPath)
		if err != nil {
			return err
		}
		signatureBytes, err := os.ReadFile(sigPath)
		if err != nil {
			return fmt.Errorf("read detached signature: %w", err)
		}
		signature := string(signatureBytes)
		if signature == "" || strings.TrimSpace(signature) != signature || strings.ContainsAny(signature, "\r\n\t ") {
			return errors.New("detached signature must be unpadded base64url with no whitespace or trailing newline")
		}
		canonical, err := trust.Canonicalize(m)
		if err != nil {
			return fmt.Errorf("canonicalize manifest: %w", err)
		}
		registry, err := trust.NewRegistry(releaseAnchor(keyID, publicKey))
		if err != nil {
			return err
		}
		if err := registry.Verify(keyID, canonical, signature, time.Now().UTC()); err != nil {
			return fmt.Errorf("verify detached signature: %w", err)
		}
	} else if !os.IsNotExist(sigErr) {
		return fmt.Errorf("inspect detached signature: %w", sigErr)
	} else {
		if publicKeyPath != "" || keyID != "" {
			return errors.New("public-key and key-id may only be supplied when manifest.json.sig exists")
		}
		if m.SigningKeyID != "unavailable-local" {
			return errors.New("unsigned bundle must use signing_key_id=unavailable-local")
		}
	}
	if info, err := os.Lstat(filepath.Join(dir, "install.sh")); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("bootstrap script must be a regular non-symlink file")
		}
		digest, _, err := digestFile(filepath.Join(dir, "install.sh"))
		if err != nil {
			return err
		}
		expectedChecksums = append(expectedChecksums, digest+"  install.sh")
	} else if !os.IsNotExist(err) {
		return err
	}
	if m.DatabaseSchema != nil {
		digest, _, err := digestFile(manifestPath)
		if err != nil {
			return err
		}
		expectedChecksums = append(expectedChecksums, digest+"  manifest.json")
	}
	sort.Strings(expectedChecksums)
	checksums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return fmt.Errorf("read SHA256SUMS: %w", err)
	}
	if string(checksums) != strings.Join(expectedChecksums, "\n")+"\n" {
		return errors.New("SHA256SUMS does not exactly match manifest artifacts")
	}
	if publicKeyPath != "" {
		if info, err := os.Lstat(filepath.Join(dir, "SHA256SUMS.sig")); err == nil && !info.Mode().IsRegular() {
			return errors.New("bootstrap signature must be a regular non-symlink file")
		}
		sig, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS.sig"))
		if err == nil {
			key, err := release.ReadPublicKey(publicKeyPath)
			if err != nil {
				return err
			}
			if err := release.VerifyBootstrap(key, m.Release, keyID, checksums, sig); err != nil {
				return err
			}
		} else if _, bootstrapErr := os.Stat(filepath.Join(dir, "install.sh")); bootstrapErr == nil || !os.IsNotExist(err) {
			return fmt.Errorf("read bootstrap signature: %w", err)
		}
	}
	return nil
}

func releaseAnchor(keyID string, publicKey []byte) trust.Anchor {
	return trust.Anchor{KeyID: keyID, PublicKey: publicKey}
}

func digestFile(path string) (string, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), uint64(n), nil
}

func validateArchive(path string, max uint64) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, errors.New("not a gzip archive")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total uint64
	entries := 0
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read tar: %w", err)
		}
		entries++
		if entries > 4096 {
			return 0, errors.New("archive has too many entries")
		}
		name := filepath.FromSlash(h.Name)
		if h.Name == "" || strings.Contains(h.Name, "\x00") || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return 0, fmt.Errorf("unsafe archive path %q", h.Name)
		}
		if seen[h.Name] {
			return 0, fmt.Errorf("duplicate archive path %q", h.Name)
		}
		seen[h.Name] = true
		if h.Typeflag != tar.TypeReg {
			return 0, fmt.Errorf("unsupported archive member %q", h.Name)
		}
		if h.Size < 0 || uint64(h.Size) > max-total {
			return 0, errors.New("archive exceeds declared unpacked size")
		}
		written, err := io.Copy(io.Discard, io.LimitReader(tr, h.Size))
		if err != nil {
			return 0, err
		}
		if written != h.Size {
			return 0, fmt.Errorf("truncated archive member %q", h.Name)
		}
		total += uint64(h.Size)
	}
	return total, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
