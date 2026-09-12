package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// ArtifactDigest returns a stable SHA-256 for one validated artifact. For a
// directory it binds every relative path and file body in sorted order, so an
// SSH transfer cannot add, remove, rename, or alter a web asset unnoticed.
func ArtifactDigest(path string, allowDirectory bool) (string, error) {
	if err := validateArtifactPath(path, allowDirectory); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	if !info.IsDir() {
		if err := hashFile(hash, path); err != nil {
			return "", err
		}
		return hex.EncodeToString(hash.Sum(nil)), nil
	}
	paths := make([]string, 0)
	err = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current != path && !entry.IsDir() {
			relative, relErr := filepath.Rel(path, current)
			if relErr != nil {
				return relErr
			}
			paths = append(paths, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	for _, relative := range paths {
		if _, err := io.WriteString(hash, "path\x00"+relative+"\x00"); err != nil {
			return "", err
		}
		if err := hashFile(hash, filepath.Join(path, filepath.FromSlash(relative))); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashFile(destination io.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := io.Copy(destination, file); err != nil {
		return fmt.Errorf("hash artifact: %w", err)
	}
	return nil
}
