package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunRefusesExistingReleaseWithoutChangingIt(t *testing.T) {
	output := t.TempDir()
	release := filepath.Join(output, "1.2.3")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(release, "keep-me")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run("1.2.3", output, "", "2026-01-01T00:00:00Z", false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("run error = %v, want existing-release refusal", err)
	}
	body, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(body) != "preserve" {
		t.Fatalf("existing release changed or disappeared: read error=%v body=%q", readErr, body)
	}
}

func TestResolveOutputDirRejectsFilesystemRootAndSymlink(t *testing.T) {
	if _, err := resolveOutputDir(string(filepath.Separator)); err == nil {
		t.Fatal("filesystem root was accepted as release output")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveOutputDir(link); err == nil {
		t.Fatal("symlink output was accepted")
	}
}

func TestArchiveBinaryIsDeterministic(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "payesh-agent")
	if err := os.WriteFile(source, []byte("deterministic binary bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	when := time.Unix(1768089600, 0).UTC()
	first := filepath.Join(root, "first.tar.gz")
	second := filepath.Join(root, "second.tar.gz")
	if _, err := archiveBinary(source, "payesh-agent", first, when); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveBinary(source, "payesh-agent", second, when); err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatal("same source and timestamp produced different archive bytes")
	}
}
