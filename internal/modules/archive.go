package modules

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxArchiveEntries bounds extraction against a manifest lying about
// unpacked size or an archive bomb with an enormous entry count.
const MaxArchiveEntries = 4096

// StageArchive verifies archiveBytes' size/checksum against the trusted
// manifest, then extracts it under destDir as a fresh, empty directory it
// creates itself. Every entry is validated before any file is written:
// absolute paths, ".." traversal, and symlink/hardlink entries are rejected
// outright (a signed catalog module has no legitimate reason to need a link),
// and cumulative extracted bytes may never exceed the manifest's declared
// unpacked size. destDir is left absent (not partially populated) on any
// rejection.
func StageArchive(archiveBytes []byte, declaredCompressedBytes, declaredUnpackedBytes uint64, declaredSHA256 hex256, destDir string) error {
	if uint64(len(archiveBytes)) != declaredCompressedBytes {
		return fmt.Errorf("modules: archive size %d does not match manifest %d", len(archiveBytes), declaredCompressedBytes)
	}
	sum := sha256.Sum256(archiveBytes)
	if hex.EncodeToString(sum[:]) != string(declaredSHA256) {
		return errors.New("modules: archive checksum does not match manifest")
	}
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return fmt.Errorf("modules: prepare staging parent: %w", err)
	}
	if _, err := os.Stat(destDir); err == nil {
		return fmt.Errorf("modules: staging directory %q already exists", destDir)
	} else if !os.IsNotExist(err) {
		return err
	}
	staged, err := os.MkdirTemp(filepath.Dir(destDir), ".stage-*")
	if err != nil {
		return fmt.Errorf("modules: create staging scratch dir: %w", err)
	}
	if err := extractTarGz(archiveBytes, staged, declaredUnpackedBytes); err != nil {
		os.RemoveAll(staged)
		return err
	}
	if err := os.Rename(staged, destDir); err != nil {
		os.RemoveAll(staged)
		return fmt.Errorf("modules: activate staged directory: %w", err)
	}
	return nil
}

// hex256 documents at the type level that a value is expected to already be
// a lowercase 64-character hex SHA-256 digest (the manifest's own
// contracts.ModuleManifest.Validate already checked its length).
type hex256 = string

func extractTarGz(archiveBytes []byte, destDir string, maxUnpackedBytes uint64) error {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		return fmt.Errorf("modules: archive is not valid gzip: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	var extractedBytes uint64
	entries := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("modules: archive is corrupt: %w", err)
		}
		entries++
		if entries > MaxArchiveEntries {
			return fmt.Errorf("modules: archive exceeds %d entries", MaxArchiveEntries)
		}
		targetPath, err := safeArchivePath(destDir, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return err
			}
			extractedBytes += uint64(header.Size)
			if extractedBytes > maxUnpackedBytes {
				return fmt.Errorf("modules: archive exceeds declared unpacked size %d bytes", maxUnpackedBytes)
			}
			if err := writeRegularFile(targetPath, tarReader, header.Size, os.FileMode(header.Mode&0o777)); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("modules: archive entry %q is a link, which is never accepted", header.Name)
		default:
			return fmt.Errorf("modules: archive entry %q has an unsupported type", header.Name)
		}
	}
	return nil
}

// safeArchivePath resolves a tar entry name against destDir and rejects
// anything that is absolute, empty, or that would resolve outside destDir —
// including a traversal built from many "..' segments or a path that is
// technically relative but whose cleaned form escapes the root.
func safeArchivePath(destDir, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.Contains(name, "\x00") {
		return "", fmt.Errorf("modules: archive entry has an unsafe name %q", name)
	}
	cleaned := filepath.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, string(filepath.Separator)) {
		return "", fmt.Errorf("modules: archive entry escapes the staging directory: %q", name)
	}
	target := filepath.Join(destDir, cleaned)
	relative, err := filepath.Rel(destDir, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("modules: archive entry escapes the staging directory: %q", name)
	}
	return target, nil
}

func writeRegularFile(path string, r io.Reader, size int64, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	written, err := io.Copy(file, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("modules: entry %q was truncated during extraction", path)
	}
	return nil
}

// ActivateAtomic makes stagedDir the live install at activeDir by removing
// any previous directory at activeDir and renaming — a single filesystem
// rename on the same volume, so a crash mid-activation leaves either the old
// or the new directory intact, never a partial mix. Callers keep stagedDir's
// sibling backup (the previous activeDir contents, moved aside first) until
// a health check confirms success.
func ActivateAtomic(stagedDir, activeDir string) (previousBackupDir string, err error) {
	if _, err := os.Stat(stagedDir); err != nil {
		return "", fmt.Errorf("modules: staged directory is not ready: %w", err)
	}
	if _, err := os.Stat(activeDir); err == nil {
		previousBackupDir = activeDir + ".previous"
		os.RemoveAll(previousBackupDir)
		if err := os.Rename(activeDir, previousBackupDir); err != nil {
			return "", fmt.Errorf("modules: preserve previous release: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(stagedDir, activeDir); err != nil {
		return previousBackupDir, fmt.Errorf("modules: activate staged release: %w", err)
	}
	return previousBackupDir, nil
}
