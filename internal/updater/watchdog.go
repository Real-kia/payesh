package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const DefaultWatchdogInterval = 5 * time.Second

// Watchdog is deliberately independent from the application process. It
// observes only the external activation journal and invokes the updater's
// recovery primitive when activation was interrupted before commit.
type Watchdog struct {
	JournalPath string
	Interval    time.Duration
	Now         func() time.Time
	Logf        func(format string, args ...any)
}

func (w Watchdog) validate() error {
	if !filepath.IsAbs(w.JournalPath) || filepath.Clean(w.JournalPath) != w.JournalPath || w.JournalPath == string(filepath.Separator) {
		return errors.New("updater: watchdog journal path must be a clean absolute path")
	}
	if w.Interval == 0 {
		return nil
	}
	if w.Interval < time.Second || w.Interval > time.Hour {
		return errors.New("updater: watchdog interval must be between 1s and 1h")
	}
	return nil
}

func (w Watchdog) interval() time.Duration {
	if w.Interval == 0 {
		return DefaultWatchdogInterval
	}
	return w.Interval
}

func (w Watchdog) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w Watchdog) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// CheckOnce performs one recovery scan. A successful rollback is reported as
// nil even though Recover returns a descriptive error after restoring the old
// release; callers can inspect the now durable rolled_back journal if needed.
func (w Watchdog) CheckOnce() error {
	if err := w.validate(); err != nil {
		return err
	}
	err := Recover(w.JournalPath, w.now())
	if err == nil {
		return nil
	}
	j, loadErr := LoadJournal(w.JournalPath)
	if loadErr == nil && j.Phase == PhaseRolledBack {
		w.logf("recovered interrupted release %s: %v", j.Release, err)
		return nil
	}
	return err
}

// Run keeps the recovery monitor alive until ctx is cancelled. Missing
// journals are normal during steady state; malformed journals fail closed and
// stop the watchdog so an operator cannot mistake an unreadable journal for a
// healthy installation.
func (w Watchdog) Run(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}
	if err := w.CheckOnce(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("updater: initial watchdog recovery: %w", err)
	}
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.CheckOnce(); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("updater: watchdog recovery: %w", err)
			}
		}
	}
}
