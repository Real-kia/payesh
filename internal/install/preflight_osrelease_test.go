package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseOSReleaseAcceptsSingleAndDoubleQuotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "os-release")
	if err := os.WriteFile(path, []byte("ID=ubuntu\nVERSION_ID='22.04'\nPRETTY_NAME=\"Ubuntu 22.04\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := parseOSRelease(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["VERSION_ID"] != "22.04" || values["PRETTY_NAME"] != "Ubuntu 22.04" {
		t.Fatalf("unexpected parsed values: %#v", values)
	}
}
