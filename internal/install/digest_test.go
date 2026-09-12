package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactDigestBindsFileTree(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "b.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := ArtifactDigest(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "b.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := ArtifactDigest(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("tree content change did not change digest")
	}
	if err := os.Rename(filepath.Join(root, "a.txt"), filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	renamed, err := ArtifactDigest(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if after == renamed {
		t.Fatal("tree rename did not change digest")
	}
}
