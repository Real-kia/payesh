package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Phase string

const (
	PhasePrepared   Phase = "prepared"
	PhasePreserved  Phase = "preserved"
	PhaseActivated  Phase = "activated"
	PhaseCommitted  Phase = "committed"
	PhaseRolledBack Phase = "rolled_back"
)

type Journal struct {
	Release     string    `json:"release"`
	ActiveDir   string    `json:"active_dir"`
	StagedDir   string    `json:"staged_dir"`
	PreviousDir string    `json:"previous_dir"`
	Phase       Phase     `json:"phase"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Transaction struct {
	JournalPath string
	ActiveDir   string
	StagedDir   string
	Release     string
	Health      func(context.Context, string) error
	Now         func() time.Time
}

// Activate records every rename outside the candidate release directory. A
// failed health check restores the complete previous release. Call Recover at
// process start to finish rollback after interruption between journal phases.
func (t Transaction) Activate(ctx context.Context) error {
	if err := t.validate(); err != nil {
		return err
	}
	now := t.now()
	previous := t.ActiveDir + ".previous"
	j := Journal{Release: t.Release, ActiveDir: t.ActiveDir, StagedDir: t.StagedDir, PreviousDir: previous, Phase: PhasePrepared, UpdatedAt: now}
	if err := saveJournal(t.JournalPath, j); err != nil {
		return err
	}
	if err := removeScoped(previous, filepath.Dir(t.ActiveDir)); err != nil {
		return err
	}
	hadActive := false
	if _, err := os.Stat(t.ActiveDir); err == nil {
		if err := os.Rename(t.ActiveDir, previous); err != nil {
			return fmt.Errorf("updater: preserve active release: %w", err)
		}
		hadActive = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	j.Phase, j.UpdatedAt = PhasePreserved, t.now()
	if err := saveJournal(t.JournalPath, j); err != nil {
		if hadActive {
			_ = os.Rename(previous, t.ActiveDir)
		}
		return err
	}
	if err := os.Rename(t.StagedDir, t.ActiveDir); err != nil {
		if hadActive {
			if restoreErr := os.Rename(previous, t.ActiveDir); restoreErr != nil {
				return fmt.Errorf("updater: activate staged release: %v; restore previous release: %w", err, restoreErr)
			}
			j.Phase, j.UpdatedAt = PhaseRolledBack, t.now()
			if journalErr := saveJournal(t.JournalPath, j); journalErr != nil {
				return fmt.Errorf("updater: activation failed and previous release was restored, but journal update failed: %w", journalErr)
			}
		}
		return fmt.Errorf("updater: activate staged release: %w", err)
	}
	j.Phase, j.UpdatedAt = PhaseActivated, t.now()
	if err := saveJournal(t.JournalPath, j); err != nil {
		return err
	}
	if t.Health == nil {
		return t.rollback(j, errors.New("updater: health verifier is required"))
	}
	if err := t.Health(ctx, t.ActiveDir); err != nil {
		return t.rollback(j, err)
	}
	j.Phase, j.UpdatedAt = PhaseCommitted, t.now()
	if err := saveJournal(t.JournalPath, j); err != nil {
		return err
	}
	return nil
}

func (t Transaction) rollback(j Journal, cause error) error {
	failed := j.ActiveDir + ".failed"
	_ = removeScoped(failed, filepath.Dir(j.ActiveDir))
	if _, err := os.Stat(j.ActiveDir); err == nil {
		if err := os.Rename(j.ActiveDir, failed); err != nil {
			return fmt.Errorf("updater: health failed (%v); preserve failed release: %w", cause, err)
		}
	}
	if _, err := os.Stat(j.PreviousDir); err == nil {
		if err := os.Rename(j.PreviousDir, j.ActiveDir); err != nil {
			return fmt.Errorf("updater: health failed (%v); restore previous release: %w", cause, err)
		}
	}
	j.Phase, j.UpdatedAt = PhaseRolledBack, t.now()
	if err := saveJournal(t.JournalPath, j); err != nil {
		return fmt.Errorf("updater: rollback completed after %v but journal failed: %w", cause, err)
	}
	return fmt.Errorf("updater: activation health failed and previous release was restored: %w", cause)
}

// Recover restores the previous release for any transaction interrupted after
// preservation but before commit. Prepared transactions have not changed the
// active release; committed/rolled-back transactions need no action.
func Recover(journalPath string, now time.Time) error {
	j, err := LoadJournal(journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.Phase == PhasePrepared || j.Phase == PhaseCommitted || j.Phase == PhaseRolledBack {
		return nil
	}
	if j.Phase != PhasePreserved && j.Phase != PhaseActivated {
		return errors.New("updater: unknown recovery phase")
	}
	t := Transaction{JournalPath: journalPath, ActiveDir: j.ActiveDir, StagedDir: j.StagedDir, Release: j.Release, Now: func() time.Time { return now }}
	return t.rollback(j, errors.New("interrupted activation"))
}

func LoadJournal(path string) (Journal, error) {
	var j Journal
	b, err := os.ReadFile(path)
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return Journal{}, errors.New("updater: recovery journal is invalid")
	}
	if !filepath.IsAbs(j.ActiveDir) || !filepath.IsAbs(j.StagedDir) || !filepath.IsAbs(j.PreviousDir) || !validSemver(j.Release) || j.UpdatedAt.IsZero() {
		return Journal{}, errors.New("updater: recovery journal is invalid")
	}
	return j, nil
}

func (t Transaction) validate() error {
	if !filepath.IsAbs(t.JournalPath) || !filepath.IsAbs(t.ActiveDir) || !filepath.IsAbs(t.StagedDir) || !validSemver(t.Release) || filepath.Dir(t.ActiveDir) != filepath.Dir(t.StagedDir) || t.ActiveDir == t.StagedDir {
		return errors.New("updater: invalid activation transaction")
	}
	if _, err := os.Stat(t.StagedDir); err != nil {
		return fmt.Errorf("updater: staged release is unavailable: %w", err)
	}
	return nil
}
func (t Transaction) now() time.Time {
	if t.Now != nil {
		return t.Now().UTC()
	}
	return time.Now().UTC()
}

func saveJournal(path string, j Journal) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("updater: invalid journal path")
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".journal-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func removeScoped(path, parent string) error {
	if filepath.Dir(path) != filepath.Clean(parent) || filepath.Base(path) == "." || filepath.Base(path) == string(filepath.Separator) {
		return errors.New("updater: refusing unscoped removal")
	}
	return os.RemoveAll(path)
}
