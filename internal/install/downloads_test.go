package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func downloadFixture(t *testing.T) map[string]string {
	t.Helper()
	paths := map[string]string{}
	root := t.TempDir()
	for _, name := range append([]string{"payesh-install"}, requiredArtifacts("node")...) {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name+"-verified-binary"), 0755); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	return paths
}

func TestHubDownloadsHaveChecksumAndExpire(t *testing.T) {
	registry := &DownloadRegistry{}
	server := httptest.NewServer(registry)
	defer server.Close()
	paths := downloadFixture(t)
	download, err := registry.Publish(context.Background(), server.URL, "node", paths)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Close()
	response, err := http.Get(download.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != download.SHA256 {
		t.Fatal("bundle checksum mismatch")
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	count := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, _ := io.ReadAll(tarReader)
		expected, _ := os.ReadFile(paths[header.Name])
		if !bytes.Equal(content, expected) {
			t.Fatalf("wrong file %s", header.Name)
		}
		count++
	}
	if count != 4 {
		t.Fatalf("files=%d", count)
	}
	bad := httptest.NewRecorder()
	registry.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/install-artifacts/"+strings.Repeat("0", 64)+"/bundle.tar.gz", nil))
	if bad.Code != http.StatusNotFound {
		t.Fatal("invalid token accepted")
	}
	registry.Now = func() time.Time { return time.Now().Add(time.Hour) }
	expired := httptest.NewRecorder()
	registry.ServeHTTP(expired, httptest.NewRequest(http.MethodGet, download.URL, nil))
	if expired.Code != http.StatusNotFound {
		t.Fatal("expired token accepted")
	}
	download.Close()
	registry.Now = nil
	closed := httptest.NewRecorder()
	registry.ServeHTTP(closed, httptest.NewRequest(http.MethodGet, download.URL, nil))
	if closed.Code != http.StatusNotFound {
		t.Fatal("closed token accepted")
	}
}

func TestNodeDownloadsHubBundleOverHTTP(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	registry := &DownloadRegistry{}
	server := httptest.NewServer(registry)
	defer server.Close()
	paths := downloadFixture(t)
	download, err := registry.Publish(context.Background(), server.URL, "node", paths)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Close()
	dest := t.TempDir()
	script, err := artifactDownloadScript(dest, "node", "dev", "amd64", download)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("download failed %v: %s", err, output)
	}
	if !strings.Contains(string(output), "download_source=hub") {
		t.Fatalf("source=%s", output)
	}
	for name, path := range paths {
		want, _ := os.ReadFile(path)
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("downloaded %s differs: %v", name, err)
		}
	}
}

func TestNodeRejectsTamperedHubBundle(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	registry := &DownloadRegistry{}
	server := httptest.NewServer(registry)
	defer server.Close()
	paths := downloadFixture(t)
	download, err := registry.Publish(context.Background(), server.URL, "node", paths)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Close()
	download.SHA256 = strings.Repeat("0", 64)
	dest := t.TempDir()
	script, err := artifactDownloadScript(dest, "node", "dev", "amd64", download)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-c", script).CombinedOutput(); err == nil {
		t.Fatalf("tampered bundle accepted: %s", output)
	}
	if _, err := os.Stat(filepath.Join(dest, "payesh-install")); !os.IsNotExist(err) {
		t.Fatal("tampered bundle was extracted")
	}
}

func writeELFFixture(t *testing.T, path string, machine elf.Machine) {
	t.Helper()
	header := make([]byte, 64)
	copy(header, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(header[16:], 2)
	binary.LittleEndian.PutUint16(header[18:], uint16(machine))
	binary.LittleEndian.PutUint32(header[20:], 1)
	binary.LittleEndian.PutUint16(header[52:], 64)
	if err := os.WriteFile(path, header, 0755); err != nil {
		t.Fatal(err)
	}
}

func TestHubSelectsNodeArchitectureAndRejectsWrongNativeBuild(t *testing.T) {
	paths := downloadFixture(t)
	for _, path := range paths {
		writeELFFixture(t, path, elf.EM_X86_64)
	}
	matrix := t.TempDir()
	if _, err := SelectLinuxArtifacts(paths["payesh-install"], paths, matrix, "arm64", "node", acceptArtifact); err == nil {
		t.Fatal("amd64 binary selected for arm64 node")
	}
	for name := range paths {
		writeELFFixture(t, filepath.Join(matrix, name+"-linux-arm64"), elf.EM_AARCH64)
	}
	selected, err := SelectLinuxArtifacts(paths["payesh-install"], paths, matrix, "arm64", "node", acceptArtifact)
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range selected {
		if path != filepath.Join(matrix, name+"-linux-arm64") {
			t.Fatalf("wrong artifact for %s", name)
		}
	}
}

func TestNodeFallsBackForAnyGitHubAcquisitionFailure(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl required")
	}
	for _, failure := range []string{"unavailable", "invalid-archive"} {
		t.Run(failure, func(t *testing.T) {
			registry := &DownloadRegistry{}
			server := httptest.NewServer(registry)
			defer server.Close()
			paths := downloadFixture(t)
			download, err := registry.Publish(context.Background(), server.URL, "node", paths)
			if err != nil {
				t.Fatal(err)
			}
			defer download.Close()
			tools := t.TempDir()
			shim := "#!/bin/sh\ncase \"$*\" in\n *https://github.com/*) "
			if failure == "unavailable" {
				shim += "exit 22"
			} else {
				shim += "while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then shift; printf 'invalid archive' > \"$1\"; exit 0; fi; shift; done; exit 1"
			}
			shim += ";;\n *) exec " + shellQuote(curl) + " \"$@\";;\nesac\n"
			if err := os.WriteFile(filepath.Join(tools, "curl"), []byte(shim), 0755); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			script, err := artifactDownloadScript(dest, "node", "0.2.6", "amd64", download)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(output), "download_source=hub") {
				t.Fatalf("fallback %v: %s", err, output)
			}
		})
	}
}
