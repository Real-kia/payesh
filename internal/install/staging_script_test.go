package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The staging check must not depend on copying /bin/true: on busybox systems such
// as Alpine it is a symlink to the busybox binary, and a copy run under another
// name fails with "applet not found", rejecting a perfectly usable directory.
func TestExecutableStagingDoesNotCopySystemBinaries(t *testing.T) {
	script := executableStagingScript("0123456789abcdef")
	for _, forbidden := range []string{"cp /bin/true", "/bin/true", "/bin/false"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("the staging check relies on %q, which fails on busybox targets", forbidden)
		}
	}
	if !strings.Contains(script, "# payesh-executable-staging") {
		t.Fatal("the staging script lost its identifying marker")
	}
}

func TestExecutableStagingCreatesAnExecutableOwnerOnlyDirectory(t *testing.T) {
	home := t.TempDir()
	id := "stagingtest" + filepath.Base(home)
	command := exec.Command("sh", "-c", executableStagingScript(id))
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("staging failed on a normal system: %v", err)
	}
	dir := strings.TrimSpace(string(output))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if dir == "" || !strings.HasSuffix(dir, "/payesh-install-"+id) {
		t.Fatalf("unexpected staging directory %q", dir)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("staging directory mode/exists: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".exec-check")); !os.IsNotExist(err) {
		t.Fatalf("the executability probe was left behind: %v", err)
	}
}

// An unwritable HOME and unusable bases must fail with the documented message
// and leave nothing behind, so the hub can report a precise error.
func TestExecutableStagingFailsCleanlyWhenNothingIsUsable(t *testing.T) {
	script := strings.NewReplacer("/var/tmp", "/nonexistent-payesh-a", "/tmp", "/nonexistent-payesh-b").Replace(executableStagingScript("fail0123456789ab"))
	command := exec.Command("sh", "-c", script)
	command.Env = append(os.Environ(), "HOME=/nonexistent-payesh-c")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "No writable executable staging directory is available") {
		t.Fatalf("err=%v output=%q", err, output)
	}
}
