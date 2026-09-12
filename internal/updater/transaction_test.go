package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func releaseDir(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "version"), []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestActivateCommitsHealthyRelease(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "active")
	staged := filepath.Join(root, "staged")
	releaseDir(t, active, "old")
	releaseDir(t, staged, "new")
	tx := Transaction{JournalPath: filepath.Join(root, "state", "journal.json"), ActiveDir: active, StagedDir: staged, Release: "0.2.0", Health: func(_ context.Context, dir string) error {
		b, err := os.ReadFile(filepath.Join(dir, "version"))
		if err != nil || string(b) != "new" {
			return errors.New("wrong release")
		}
		return nil
	}}
	if err := tx.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, err := LoadJournal(tx.JournalPath)
	if err != nil || j.Phase != PhaseCommitted {
		t.Fatalf("journal=%+v err=%v", j, err)
	}
}

func TestActivateRollsBackFailedHealth(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "active")
	staged := filepath.Join(root, "staged")
	releaseDir(t, active, "old")
	releaseDir(t, staged, "new")
	tx := Transaction{JournalPath: filepath.Join(root, "journal.json"), ActiveDir: active, StagedDir: staged, Release: "0.2.0", Health: func(context.Context, string) error { return errors.New("broken") }}
	err := tx.Activate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("err=%v", err)
	}
	b, readErr := os.ReadFile(filepath.Join(active, "version"))
	if readErr != nil || string(b) != "old" {
		t.Fatalf("active=%q err=%v", b, readErr)
	}
}

func TestRecoverInterruptedActivatedTransaction(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "active")
	previous := active + ".previous"
	releaseDir(t, active, "new")
	releaseDir(t, previous, "old")
	path := filepath.Join(root, "journal.json")
	if err := saveJournal(path, Journal{Release: "0.2.0", ActiveDir: active, StagedDir: filepath.Join(root, "staged"), PreviousDir: previous, Phase: PhaseActivated, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := Recover(path, time.Now().UTC()); err == nil {
		t.Fatal("expected recovery result describing rollback")
	}
	b, err := os.ReadFile(filepath.Join(active, "version"))
	if err != nil || string(b) != "old" {
		t.Fatalf("active=%q err=%v", b, err)
	}
}
