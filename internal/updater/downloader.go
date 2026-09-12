package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/trust"
)

const (
	DefaultDownloadTimeout = 10 * time.Minute
	DefaultDownloadMax     = uint64(512 << 20)
	DefaultHeadroomBytes   = uint64(64 << 20)
	MaxArchiveEntries      = 4096
)

var (
	ErrInsufficientDisk = errors.New("updater: insufficient disk headroom")
	ErrDownloadTooLarge = errors.New("updater: downloaded artifact exceeds its bound")
)

// DownloadOptions bounds network and temporary-file use. The destination is
// committed with a same-directory rename only after the manifest-declared
// length and digest have been checked.
type DownloadOptions struct {
	Client          *http.Client
	Timeout         time.Duration
	MaxBytes        uint64
	RequiredFree    uint64
	ReplaceExisting bool
}

// HeadroomError includes enough information for a caller to report a useful
// preflight failure without exposing a path or response body.
type HeadroomError struct {
	Available uint64
	Required  uint64
}

func (e *HeadroomError) Error() string {
	return fmt.Sprintf("%v: available=%d required=%d", ErrInsufficientDisk, e.Available, e.Required)
}
func (e *HeadroomError) Unwrap() error { return ErrInsufficientDisk }

// FreeBytes returns blocks available to the process on the filesystem that
// contains path. Statfs.Bavail is used rather than total free blocks so an
// unprivileged updater does not consume the filesystem reserve.
func FreeBytes(path string) (uint64, error) {
	if path == "" {
		return 0, errors.New("updater: disk path is required")
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("updater: inspect disk headroom: %w", err)
	}
	blockSize := uint64(stat.Bsize)
	blocks := uint64(stat.Bavail)
	if blockSize == 0 || blocks > ^uint64(0)/blockSize {
		return 0, errors.New("updater: disk headroom is invalid")
	}
	return blocks * blockSize, nil
}

func checkedAdd(values ...uint64) (uint64, error) {
	var total uint64
	for _, value := range values {
		if ^uint64(0)-total < value {
			return 0, errors.New("updater: disk headroom overflows")
		}
		total += value
	}
	return total, nil
}

// RequiredHeadroom is the temporary space needed while the compressed
// artifact, unpacked candidate and optional backup coexist.
func RequiredHeadroom(artifact contracts.ReleaseArtifact, backupBytes, reserve uint64) (uint64, error) {
	if artifact.CompressedBytes == 0 || artifact.UnpackedBytes == 0 {
		return 0, errors.New("updater: artifact sizes are required")
	}
	return checkedAdd(artifact.CompressedBytes, artifact.UnpackedBytes, backupBytes, reserve)
}

func checkHeadroom(path string, required uint64) error {
	available, err := FreeBytes(path)
	if err != nil {
		return err
	}
	if available < required {
		return &HeadroomError{Available: available, Required: required}
	}
	return nil
}

func (o DownloadOptions) normalized() (DownloadOptions, error) {
	if o.Timeout == 0 {
		o.Timeout = DefaultDownloadTimeout
	}
	if o.Timeout <= 0 || o.Timeout > 24*time.Hour {
		return o, errors.New("updater: download timeout must be positive and no longer than 24h")
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = DefaultDownloadMax
	}
	if o.MaxBytes == 0 || o.MaxBytes > uint64(^uint(0)>>1) {
		return o, errors.New("updater: download size bound is invalid")
	}
	if o.RequiredFree == 0 {
		o.RequiredFree = DefaultHeadroomBytes
	}
	return o, nil
}

func validateArtifactURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("updater: artifact URL must be an HTTP(S) URL without credentials")
	}
	return u, nil
}

// DownloadArtifact streams one manifest-selected artifact to destination.
// It never allocates the artifact in memory and leaves no partial destination
// behind after a failed request, timeout, size check, or digest check.
func DownloadArtifact(ctx context.Context, artifact contracts.ReleaseArtifact, destination string, options DownloadOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	options, err := options.normalized()
	if err != nil {
		return err
	}
	if artifact.CompressedBytes == 0 || artifact.CompressedBytes > options.MaxBytes || artifact.CompressedBytes > uint64(^uint64(0)>>1) {
		return fmt.Errorf("%w: manifest size=%d max=%d", ErrDownloadTooLarge, artifact.CompressedBytes, options.MaxBytes)
	}
	if len(artifact.SHA256) != sha256.Size*2 || strings.ToLower(artifact.SHA256) != artifact.SHA256 {
		return errors.New("updater: artifact digest is invalid")
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil {
		return errors.New("updater: artifact digest is invalid")
	}
	if _, err := validateArtifactURL(artifact.URL); err != nil {
		return err
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination || destination == string(filepath.Separator) {
		return errors.New("updater: download destination must be a clean absolute path")
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("updater: prepare download directory: %w", err)
	}
	if !options.ReplaceExisting {
		if _, statErr := os.Stat(destination); statErr == nil {
			return errors.New("updater: download destination already exists")
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
	}
	required, err := checkedAdd(artifact.CompressedBytes, options.RequiredFree)
	if err != nil {
		return err
	}
	if err := checkHeadroom(parent, required); err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, artifact.URL, nil)
	if err != nil {
		return fmt.Errorf("updater: create artifact request: %w", err)
	}
	client := options.Client
	if client == nil {
		client = &http.Client{}
	}
	clientCopy := *client
	userRedirect := clientCopy.CheckRedirect
	clientCopy.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if _, redirectErr := validateArtifactURL(next.URL.String()); redirectErr != nil {
			return redirectErr
		}
		if len(via) >= 5 {
			return errors.New("updater: artifact download followed too many redirects")
		}
		if userRedirect != nil {
			return userRedirect(next, via)
		}
		return nil
	}
	response, err := clientCopy.Do(req)
	if err != nil {
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("updater: artifact download timed out after %s: %w", options.Timeout, requestCtx.Err())
		}
		return fmt.Errorf("updater: download artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("updater: artifact server returned HTTP %s", response.Status)
	}
	if response.ContentLength >= 0 && uint64(response.ContentLength) != artifact.CompressedBytes {
		return fmt.Errorf("updater: artifact content length %d does not match manifest %d", response.ContentLength, artifact.CompressedBytes)
	}
	tmp, err := os.CreateTemp(parent, ".artifact-*")
	if err != nil {
		return fmt.Errorf("updater: create artifact temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	hash := sha256.New()
	reader := io.LimitReader(response.Body, int64(artifact.CompressedBytes)+1)
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), reader)
	if copyErr != nil {
		tmp.Close()
		return fmt.Errorf("updater: receive artifact: %w", copyErr)
	}
	if written != int64(artifact.CompressedBytes) {
		tmp.Close()
		return fmt.Errorf("updater: artifact size %d does not match manifest %d", written, artifact.CompressedBytes)
	}
	if hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		tmp.Close()
		return errors.New("updater: artifact checksum mismatch")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("updater: sync downloaded artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("updater: close downloaded artifact: %w", err)
	}
	if options.ReplaceExisting {
		if err := os.Rename(tmpName, destination); err != nil {
			return fmt.Errorf("updater: commit downloaded artifact: %w", err)
		}
	} else if err := os.Link(tmpName, destination); err != nil {
		return fmt.Errorf("updater: commit downloaded artifact: %w", err)
	} else if err := os.Remove(tmpName); err != nil {
		return fmt.Errorf("updater: finalize downloaded artifact: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("updater: sync artifact directory: %w", err)
	}
	return nil
}

// DownloadVerifiedRelease authenticates release metadata before making any
// network request for its selected artifact.
func DownloadVerifiedRelease(ctx context.Context, registry *trust.Registry, manifest contracts.ReleaseManifest, signature, currentCore string, state AcceptedState, now time.Time, name, goos, goarch, destination string, options DownloadOptions) (contracts.ReleaseArtifact, error) {
	if err := VerifyManifest(registry, manifest, signature, currentCore, state, now); err != nil {
		return contracts.ReleaseArtifact{}, err
	}
	artifact, err := SelectArtifact(manifest, name, goos, goarch)
	if err != nil {
		return contracts.ReleaseArtifact{}, err
	}
	if err := DownloadArtifact(ctx, artifact, destination, options); err != nil {
		return contracts.ReleaseArtifact{}, err
	}
	return artifact, nil
}

// StageArtifact verifies a downloaded archive and extracts it into a fresh,
// sibling directory. The candidate is renamed into place only after complete
// extraction; links and paths escaping the candidate are rejected.
func StageArtifact(artifact contracts.ReleaseArtifact, archivePath, destination string, backupBytes, reserve uint64) error {
	if !filepath.IsAbs(archivePath) || filepath.Clean(archivePath) != archivePath || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination || archivePath == destination {
		return errors.New("updater: invalid artifact staging paths")
	}
	if err := VerifyArtifactFile(artifact, archivePath); err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil {
		return errors.New("updater: staging destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("updater: prepare staging directory: %w", err)
	}
	required, err := checkedAdd(artifact.UnpackedBytes, backupBytes, reserve)
	if err != nil {
		return err
	}
	if err := checkHeadroom(filepath.Dir(destination), required); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(destination), ".release-*")
	if err != nil {
		return fmt.Errorf("updater: create release staging directory: %w", err)
	}
	if err := extractTarGzFile(archivePath, tmp, artifact.UnpackedBytes); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("updater: commit staged release: %w", err)
	}
	return syncDirectory(filepath.Dir(destination))
}

func VerifyArtifactFile(artifact contracts.ReleaseArtifact, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("updater: artifact path must be a clean absolute path")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("updater: open downloaded artifact: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 0 || uint64(info.Size()) != artifact.CompressedBytes {
		return errors.New("updater: artifact size mismatch")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, int64(artifact.CompressedBytes)+1)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return errors.New("updater: artifact checksum mismatch")
	}
	return nil
}

func extractTarGzFile(archivePath, destination string, maxBytes uint64) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("updater: open release archive: %w", err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("updater: release archive is not gzip: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	var unpacked uint64
	entries := 0
	for {
		header, nextErr := tarReader.Next()
		if nextErr == io.EOF {
			return nil
		}
		if nextErr != nil {
			return fmt.Errorf("updater: read release archive: %w", nextErr)
		}
		entries++
		if entries > MaxArchiveEntries {
			return fmt.Errorf("updater: release archive exceeds %d entries", MaxArchiveEntries)
		}
		target, pathErr := safeArchivePath(destination, header.Name)
		if pathErr != nil {
			return pathErr
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size < 0 || uint64(header.Size) > maxBytes-unpacked {
				return fmt.Errorf("updater: release archive exceeds declared unpacked size %d bytes", maxBytes)
			}
			unpacked += uint64(header.Size)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(header.Mode & 0o777)
			if mode == 0 {
				mode = 0o644
			}
			out, createErr := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if createErr != nil {
				return createErr
			}
			written, writeErr := io.Copy(out, io.LimitReader(tarReader, header.Size))
			closeErr := out.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
			if written != header.Size {
				return fmt.Errorf("updater: release archive entry %q is truncated", header.Name)
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("updater: release archive entry %q is a link", header.Name)
		default:
			return fmt.Errorf("updater: release archive entry %q has unsupported type", header.Name)
		}
	}
}

func safeArchivePath(root, name string) (string, error) {
	if name == "" || strings.Contains(name, "\x00") || filepath.IsAbs(name) {
		return "", fmt.Errorf("updater: release archive entry has unsafe name %q", name)
	}
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("updater: release archive entry escapes staging directory: %q", name)
	}
	target := filepath.Join(root, cleaned)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("updater: release archive entry escapes staging directory: %q", name)
	}
	return target, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
