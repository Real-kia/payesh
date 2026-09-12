package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchdogRecoversInterruptedActivation(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "active")
	previous := active + ".previous"
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(previous, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "version"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previous, "version"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "journal.json")
	if err := saveJournal(journal, Journal{Release: "1.2.0", ActiveDir: active, StagedDir: filepath.Join(root, "staged"), PreviousDir: previous, Phase: PhaseActivated, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	w := Watchdog{JournalPath: journal, Interval: time.Second}
	if err := w.CheckOnce(); err != nil {
		t.Fatal(err)
	}
	version, err := os.ReadFile(filepath.Join(active, "version"))
	if err != nil || string(version) != "old" {
		t.Fatalf("active=%q err=%v", version, err)
	}
	j, err := LoadJournal(journal)
	if err != nil || j.Phase != PhaseRolledBack {
		t.Fatalf("journal=%+v err=%v", j, err)
	}
}

func TestWatchdogRejectsUnsafeConfiguration(t *testing.T) {
	if err := (Watchdog{JournalPath: "relative"}).CheckOnce(); err == nil {
		t.Fatal("relative journal path accepted")
	}
	if err := (Watchdog{JournalPath: "/tmp/journal", Interval: 500 * time.Millisecond}).CheckOnce(); err == nil {
		t.Fatal("sub-second watchdog interval accepted")
	}
	if err := (Watchdog{JournalPath: "/tmp/journal"}).CheckOnce(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing journal should be harmless, got %v", err)
	}
}
