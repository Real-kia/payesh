package modules

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzWriter)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Size: int64(len(entry.body)), Mode: mode, Linkname: entry.linkname}
		if entry.typeflag == tar.TypeDir {
			header.Mode = 0o755
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) > 0 {
			if _, err := tarWriter.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	name     string
	typeflag byte
	body     []byte
	linkname string
	mode     int64
}

func TestStageArchiveRejectsUnexpectedExecutable(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "bin/surprise", typeflag: tar.TypeReg, body: []byte("binary"), mode: 0o755}})
	dest := filepath.Join(t.TempDir(), "staged")
	if err := StageArchive(archive, uint64(len(archive)), 64, sha256Hex(archive), dest, "bin/expected"); err == nil {
		t.Fatal("expected an undeclared executable to be rejected")
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestStageArchiveHappyPath(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{
		{name: "bin/module", typeflag: tar.TypeReg, body: []byte("hello")},
		{name: "manifest.json", typeflag: tar.TypeReg, body: []byte(`{}`)},
	})
	dest := filepath.Join(t.TempDir(), "staged")
	if err := StageArchive(archive, uint64(len(archive)), 64, sha256Hex(archive), dest); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dest, "bin/module"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatalf("got %q", content)
	}
}

func TestStageArchiveRejectsChecksumMismatch(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "f", typeflag: tar.TypeReg, body: []byte("x")}})
	dest := filepath.Join(t.TempDir(), "staged")
	err := StageArchive(archive, uint64(len(archive)), 64, strings.Repeat("0", 64), dest)
	if err == nil {
		t.Fatal("expected checksum mismatch to be rejected")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a rejected archive must not leave a partial staging directory")
	}
}

func TestStageArchiveRejectsDeclaredSizeMismatch(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "f", typeflag: tar.TypeReg, body: []byte("x")}})
	dest := filepath.Join(t.TempDir(), "staged")
	if err := StageArchive(archive, uint64(len(archive))+1, 64, sha256Hex(archive), dest); err == nil {
		t.Fatal("expected declared compressed size mismatch to be rejected")
	}
}

func TestStageArchiveRejectsPathTraversal(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "../../etc/passwd", typeflag: tar.TypeReg, body: []byte("x")}})
	dest := filepath.Join(t.TempDir(), "staged")
	if err := StageArchive(archive, uint64(len(archive)), 64, sha256Hex(archive), dest); err == nil {
		t.Fatal("expected path traversal entry to be rejected")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a rejected archive must not leave a partial staging directory")
	}
}

func TestStageArchiveRejectsAbsolutePath(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "/etc/passwd", typeflag: tar.TypeReg, body: []byte("x")}})
	dest := filepath.Join(t.TempDir(), "staged")
	if err := StageArchive(archive, uint64(len(archive)), 64, sha256Hex(archive), dest); err == nil {
		t.Fatal("expected absolute path entry to be rejected")
	}
}

func TestStageArchiveRejectsSymlinkEscape(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}})
	dest := filepath.Join(t.TempDir(), "staged")
	if err := StageArchive(archive, uint64(len(archive)), 64, sha256Hex(archive), dest); err == nil {
		t.Fatal("expected symlink entry to be rejected")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a rejected archive must not leave a partial staging directory")
	}
}

func TestStageArchiveRejectsHardlink(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "link", typeflag: tar.TypeLink, linkname: "some-other-file"}})
	dest := filepath.Join(t.TempDir(), "staged")
	if err := StageArchive(archive, uint64(len(archive)), 64, sha256Hex(archive), dest); err == nil {
		t.Fatal("expected hardlink entry to be rejected")
	}
}

func TestStageArchiveRejectsOversizedExtraction(t *testing.T) {
	body := bytes.Repeat([]byte{'a'}, 1024)
	archive := buildTarGz(t, []tarEntry{{name: "big", typeflag: tar.TypeReg, body: body}})
	dest := filepath.Join(t.TempDir(), "staged")
	if err := StageArchive(archive, uint64(len(archive)), 16, sha256Hex(archive), dest); err == nil {
		t.Fatal("expected oversized extraction to be rejected")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a rejected archive must not leave a partial staging directory")
	}
}

func TestStageArchiveRejectsExistingDestination(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "f", typeflag: tar.TypeReg, body: []byte("x")}})
	dest := filepath.Join(t.TempDir(), "staged")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := StageArchive(archive, uint64(len(archive)), 64, sha256Hex(archive), dest); err == nil {
		t.Fatal("expected an already-existing destination to be rejected")
	}
}

func TestActivateAtomicPreservesPreviousUntilSuccess(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "active")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(root, "staged")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous, err := ActivateAtomic(staged, active)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(active, "new.txt")); err != nil {
		t.Fatal("expected new content to be active")
	}
	if _, err := os.Stat(filepath.Join(previous, "old.txt")); err != nil {
		t.Fatal("expected previous release to be preserved for rollback")
	}
}
