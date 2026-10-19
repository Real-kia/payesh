//go:build unix

package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestArtifactPermissionsUnderRestrictiveUmask(t *testing.T) {
	if os.Getenv("PAYESH_TEST_RESTRICTIVE_UMASK") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestArtifactPermissionsUnderRestrictiveUmask$")
		cmd.Env = append(os.Environ(), "PAYESH_TEST_RESTRICTIVE_UMASK=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restrictive umask: %v: %s", err, out)
		}
		return
	}
	syscall.Umask(0077)
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.WriteFile(source, []byte("verified executable"), 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "installed", "payesh-agent")
	if err := installArtifact(source, binary); err != nil {
		t.Fatal(err)
	}
	webSource := filepath.Join(dir, "web", "assets")
	if err := os.MkdirAll(webSource, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webSource, "app.js"), []byte("verified web asset"), 0600); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(dir, "web-assets")
	if err := installArtifact(filepath.Dir(webSource), web); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{binary: 0755, web: 0755, filepath.Join(web, "assets"): 0755, filepath.Join(web, "assets", "app.js"): 0644} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode=%o want=%o", path, info.Mode().Perm(), mode)
		}
	}
}
