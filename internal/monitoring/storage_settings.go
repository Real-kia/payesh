package monitoring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

const DefaultDatabaseLimit int64 = 1000000000

var ErrSettingsConflict = errors.New("storage settings changed; reload and retry")

type StorageSettings struct {
	MaxDatabaseBytes      int64  `json:"max_database_bytes"`
	SampleSeconds         int    `json:"sample_seconds"`
	PressureSampleSeconds int    `json:"pressure_sample_seconds"`
	AdaptiveSampling      bool   `json:"adaptive_sampling"`
	NotificationsEnabled  bool   `json:"notifications_enabled"`
	Revision              int64  `json:"revision,string"`
	PressureState         string `json:"pressure_state"`
}
type StorageStatus struct {
	Settings               StorageSettings `json:"settings"`
	DatabaseBytes          int64           `json:"database_bytes"`
	EffectiveSampleSeconds int             `json:"effective_sample_seconds"`
}
type StorageNotification struct {
	ID        int64  `json:"id,string"`
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
	Read      bool   `json:"read"`
}

func DefaultStorageSettings() StorageSettings {
	return StorageSettings{MaxDatabaseBytes: DefaultDatabaseLimit, SampleSeconds: 15, PressureSampleSeconds: 60, AdaptiveSampling: true, NotificationsEnabled: true, PressureState: "normal"}
}
func (p StorageSettings) Validate() error {
	if p.MaxDatabaseBytes < 128000000 || p.MaxDatabaseBytes > 64000000000 {
		return errors.New("database limit must be between 0.128 and 64 GB")
	}
	if p.SampleSeconds < 5 || p.SampleSeconds > 3600 || p.PressureSampleSeconds < p.SampleSeconds || p.PressureSampleSeconds > 3600 {
		return errors.New("sampling must be 5–3600 seconds; storage-saving interval must be at least the normal interval")
	}
	return nil
}
func (s *Store) initStorageSettings(ctx context.Context) error {
	p := DefaultStorageSettings()
	_, err := s.db.ExecContext(ctx, `INSERT INTO storage_settings(singleton,max_bytes,sample_seconds,pressure_seconds,adaptive,notifications,revision,pressure_state) VALUES(1,?,?,?,?,?,1,'normal') ON CONFLICT DO NOTHING`, p.MaxDatabaseBytes, p.SampleSeconds, p.PressureSampleSeconds, p.AdaptiveSampling, p.NotificationsEnabled)
	return err
}
func (s *Store) StorageSettings(ctx context.Context) (StorageSettings, bool, error) {
	p := DefaultStorageSettings()
	err := s.db.QueryRowContext(ctx, `SELECT max_bytes,sample_seconds,pressure_seconds,adaptive,notifications,revision,pressure_state FROM storage_settings WHERE singleton=1`).Scan(&p.MaxDatabaseBytes, &p.SampleSeconds, &p.PressureSampleSeconds, &p.AdaptiveSampling, &p.NotificationsEnabled, &p.Revision, &p.PressureState)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}
func (s *Store) SaveStorageSettings(ctx context.Context, p StorageSettings) error {
	if err := p.Validate(); err != nil {
		return err
	}
	_, configured, err := s.StorageSettings(ctx)
	if err != nil {
		return err
	}
	if !configured {
		if p.Revision != 0 {
			return ErrSettingsConflict
		}
		if err := s.initStorageSettings(ctx); err != nil {
			return err
		}
		p.Revision = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE storage_settings SET max_bytes=?,sample_seconds=?,pressure_seconds=?,adaptive=?,notifications=?,revision=revision+1 WHERE singleton=1 AND revision=?`, p.MaxDatabaseBytes, p.SampleSeconds, p.PressureSampleSeconds, p.AdaptiveSampling, p.NotificationsEnabled, p.Revision)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrSettingsConflict
	}
	return nil
}
func (s *Store) DatabaseBytes() (int64, error) {
	if s.path == ":memory:" {
		return 0, nil
	}
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Stat(s.path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}
func (p StorageSettings) EffectiveSampleSeconds() int {
	if p.AdaptiveSampling && p.PressureState == "saving" {
		return p.PressureSampleSeconds
	}
	return p.SampleSeconds
}
func (s *Store) StorageStatus(ctx context.Context) (StorageStatus, error) {
	p, _, err := s.StorageSettings(ctx)
	if err != nil {
		return StorageStatus{}, err
	}
	usage, err := s.DatabaseBytes()
	return StorageStatus{p, usage, p.EffectiveSampleSeconds()}, err
}
func (s *Store) SamplingSeconds(ctx context.Context) int {
	p, _, err := s.StorageSettings(ctx)
	if err != nil {
		return 15
	}
	return p.EffectiveSampleSeconds()
}

// ReadLocalSamplingSeconds uses a read-only handle without opening/migrating
// a Store. It never creates a database on nodes that only stream telemetry.
func ReadLocalSamplingSeconds(ctx context.Context, path string) int {
	if _, err := os.Stat(path); err != nil {
		return 0
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(1000)")
	if err != nil {
		return 0
	}
	defer db.Close()
	var normal, pressure int
	var adaptive bool
	var state string
	if err := db.QueryRowContext(ctx, `SELECT sample_seconds,pressure_seconds,adaptive,pressure_state FROM storage_settings WHERE singleton=1`).Scan(&normal, &pressure, &adaptive, &state); err != nil {
		return 0
	}
	if adaptive && state == "saving" {
		normal = pressure
	}
	if normal < 5 || normal > 3600 {
		return 0
	}
	return normal
}
func (s *Store) storageBudget(ctx context.Context) (int64, bool, error) {
	p, configured, err := s.StorageSettings(ctx)
	if err != nil {
		return 0, false, err
	}
	if configured {
		return p.MaxDatabaseBytes, true, nil
	}
	return s.maxBytes, false, nil
}
func (s *Store) budgetUsage(databaseOnly bool) (int64, error) {
	if databaseOnly {
		return s.DatabaseBytes()
	}
	return s.StorageBytes()
}

func (s *Store) updateStoragePressure(ctx context.Context, usage int64) error {
	p, configured, err := s.StorageSettings(ctx)
	if err != nil || !configured {
		return err
	}
	state := "normal"
	if usage >= p.MaxDatabaseBytes*90/100 || (p.PressureState == "saving" && usage >= p.MaxDatabaseBytes*75/100) {
		state = "saving"
	} else if usage >= p.MaxDatabaseBytes*80/100 {
		state = "warning"
	}
	if state == p.PressureState {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE storage_settings SET pressure_state=? WHERE singleton=1 AND pressure_state=? AND revision=?`, state, p.PressureState, p.Revision)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n > 0 && p.NotificationsEnabled {
		message := "Database pressure has cleared."
		if p.AdaptiveSampling {
			message += " Normal sampling has resumed."
		}
		if state == "warning" {
			message = "Database usage is above 80% of its configured limit."
		}
		if state == "saving" {
			message = "Database is near its limit. Old history cleanup is starting."
			if p.AdaptiveSampling {
				message += fmt.Sprintf(" New samples will be collected every %d seconds to reduce growth.", p.PressureSampleSeconds)
			}
		}
		if err := insertStorageNotificationTx(ctx, tx, "storage_"+state, message, time.Now().UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func insertStorageNotificationTx(ctx context.Context, tx *sql.Tx, kind, message string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO storage_notifications(kind,message,created_at,is_read,event_key) VALUES(?,?,?,0,?) ON CONFLICT(event_key) DO NOTHING`, kind, message, FormatPersistedTime(now), kind+":"+strconv.FormatInt(now.Unix()/600, 10))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM storage_notifications WHERE id NOT IN (SELECT id FROM storage_notifications ORDER BY id DESC LIMIT 100)`)
	return err
}
func (s *Store) notifyStorageCleanup(ctx context.Context, removed int64, before, after int64) error {
	p, configured, err := s.StorageSettings(ctx)
	if err != nil || !configured || !p.NotificationsEnabled || removed == 0 {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	message := fmt.Sprintf("Removed %d old history records to control database size. Database usage: %.1f → %.1f MB. Older date ranges may now have missing history.", removed, float64(before)/1000000, float64(after)/1000000)
	if err := insertStorageNotificationTx(ctx, tx, "storage_cleanup", message, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) StorageNotifications(ctx context.Context) ([]StorageNotification, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,message,created_at,is_read FROM storage_notifications ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []StorageNotification{}
	for rows.Next() {
		var n StorageNotification
		if err := rows.Scan(&n.ID, &n.Kind, &n.Message, &n.CreatedAt, &n.Read); err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	return items, rows.Err()
}
func (s *Store) MarkStorageNotificationsRead(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE storage_notifications SET is_read=1 WHERE is_read=0`)
	return err
}
func (s *Store) EnforceStorageLimit(ctx context.Context) error { return s.ensureWritable(ctx) }

func (s *Store) notifyStorageLimitReached(ctx context.Context) error {
	p, configured, err := s.StorageSettings(ctx)
	if err != nil || !configured || !p.NotificationsEnabled {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertStorageNotificationTx(ctx, tx, "storage_full", "The database reached its size limit. New history writes are paused while old data is removed; server identities and billing totals are preserved.", time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}
