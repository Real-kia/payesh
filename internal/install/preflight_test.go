package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSelectsRoleArtifactsAndDetectsPlatform(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\nVERSION_ID=12\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0700); err != nil {
		t.Fatal(err)
	}
	p, err := Check(root, "node", "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Supported || p.Init != "systemd" {
		t.Fatalf("preflight=%+v", p)
	}
	for _, artifact := range p.Artifacts {
		if artifact == "payesh-server" || artifact == "web-assets" {
			t.Fatal("node includes web artifact")
		}
	}
	if _, err := Check(root, "invalid", ""); err == nil {
		t.Fatal("invalid role accepted")
	}
}

func TestCheckRejectsSupportedDistributionWithoutInitSystem(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\nVERSION_ID=12\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Check(root, "node", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Supported {
		t.Fatalf("preflight unexpectedly supported without init: %+v", p)
	}
	if len(p.Problems) == 0 || p.Problems[len(p.Problems)-1] != "no supported init system was detected (systemd or openrc is required)" {
		t.Fatalf("missing init problem: %+v", p.Problems)
	}
}

func TestSupportedReleaseMatrixIsPinned(t *testing.T) {
	for _, tc := range []struct {
		id, version string
		want        bool
	}{
		{"ubuntu", "22.04", true},
		{"ubuntu", "24.04", true},
		{"ubuntu", "23.10", false},
		{"ubuntu", "25.04", false},
		{"debian", "12", true},
		{"debian", "13", true},
		{"debian", "14", false},
		{"fedora", "42", true},
		{"fedora", "43", true},
		{"fedora", "44", false},
		{"fedora", "39", false},
		{"rocky", "9", true},
		{"rocky", "10.2", true},
		{"rocky", "11", false},
		{"almalinux", "9", true},
		{"almalinux", "10", true},
		{"alpine", "3.22", true},
		{"alpine", "3.23.1", true},
		{"alpine", "3.24", false},
	} {
		if got := supported(tc.id, tc.version); got != tc.want {
			t.Errorf("supported(%q, %q)=%v, want %v", tc.id, tc.version, got, tc.want)
		}
	}
}

func TestSupportedArchitectureMatrix(t *testing.T) {
	if !supportedArchitecture("amd64") || !supportedArchitecture("arm64") {
		t.Fatal("declared release architectures were rejected")
	}
	for _, architecture := range []string{"386", "arm", "ppc64le", "riscv64", "wasm"} {
		if supportedArchitecture(architecture) {
			t.Fatalf("unsupported architecture accepted: %s", architecture)
		}
	}
}
