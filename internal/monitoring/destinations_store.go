package monitoring

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MaxAlertDestinations bounds how many notification destinations an owner can
// configure, so one outage cannot fan out to an unbounded number of endpoints.
const MaxAlertDestinations = 20

var (
	// ErrDestinationLimit reports that MaxAlertDestinations is already stored.
	ErrDestinationLimit = errors.New("too many notification destinations")
	// ErrDestinationNotFound reports an unknown destination ID.
	ErrDestinationNotFound = errors.New("notification destination not found")
)

// AlertDestinationRow is the persisted form of a notification destination.
// ConfigJSON is non-secret; SecretBlob is an opaque encrypted value owned by
// the alerts package. Neither is interpreted here.
type AlertDestinationRow struct {
	ID            string
	Name          string
	ConfigJSON    string
	SecretBlob    []byte
	Enabled       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastAttemptAt time.Time
	LastOK        bool
	LastStatus    string
	LastError     string
}

const destinationColumns = `id,name,config_json,secret_blob,enabled,created_at,updated_at,last_attempt_at,last_ok,last_status,last_error`

func scanDestination(scanner interface{ Scan(...any) error }) (AlertDestinationRow, error) {
	var row AlertDestinationRow
	var enabled, lastOK int
	var created, updated, attempt string
	if err := scanner.Scan(&row.ID, &row.Name, &row.ConfigJSON, &row.SecretBlob, &enabled, &created, &updated, &attempt, &lastOK, &row.LastStatus, &row.LastError); err != nil {
		return AlertDestinationRow{}, err
	}
	row.Enabled, row.LastOK = enabled != 0, lastOK != 0
	row.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	row.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if attempt != "" {
		row.LastAttemptAt, _ = time.Parse(time.RFC3339Nano, attempt)
	}
	return row, nil
}

// ListAlertDestinations returns every stored destination, oldest first.
func (s *Store) ListAlertDestinations(ctx context.Context) ([]AlertDestinationRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+destinationColumns+` FROM alert_destinations ORDER BY created_at, id LIMIT ?`, MaxAlertDestinations+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AlertDestinationRow{}
	for rows.Next() {
		row, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// GetAlertDestination returns one destination.
func (s *Store) GetAlertDestination(ctx context.Context, id string) (AlertDestinationRow, error) {
	row, err := scanDestination(s.db.QueryRowContext(ctx, `SELECT `+destinationColumns+` FROM alert_destinations WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AlertDestinationRow{}, ErrDestinationNotFound
	}
	return row, err
}

// CreateAlertDestination inserts a destination, enforcing MaxAlertDestinations.
func (s *Store) CreateAlertDestination(ctx context.Context, row AlertDestinationRow) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_destinations`).Scan(&count); err != nil {
			return err
		}
		if count >= MaxAlertDestinations {
			return ErrDestinationLimit
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO alert_destinations(id,name,config_json,secret_blob,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
			row.ID, row.Name, row.ConfigJSON, row.SecretBlob, destinationBool(row.Enabled), row.CreatedAt.UTC().Format(time.RFC3339Nano), row.UpdatedAt.UTC().Format(time.RFC3339Nano))
		return err
	})
}

// UpdateAlertDestination replaces the editable fields of a destination and
// keeps its delivery status.
func (s *Store) UpdateAlertDestination(ctx context.Context, row AlertDestinationRow) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE alert_destinations SET name=?,config_json=?,secret_blob=?,enabled=?,updated_at=? WHERE id=?`,
			row.Name, row.ConfigJSON, row.SecretBlob, destinationBool(row.Enabled), row.UpdatedAt.UTC().Format(time.RFC3339Nano), row.ID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrDestinationNotFound
		}
		return nil
	})
}

// DeleteAlertDestination removes a destination.
func (s *Store) DeleteAlertDestination(ctx context.Context, id string) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM alert_destinations WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrDestinationNotFound
		}
		return nil
	})
}

// RecordAlertDestinationDelivery stores the outcome of the latest delivery.
// status and detail are short, secret-free strings.
func (s *Store) RecordAlertDestinationDelivery(ctx context.Context, id string, at time.Time, ok bool, status, detail string) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE alert_destinations SET last_attempt_at=?,last_ok=?,last_status=?,last_error=? WHERE id=?`,
			at.UTC().Format(time.RFC3339Nano), destinationBool(ok), status, detail, id)
		return err
	})
}

func destinationBool(value bool) int {
	if value {
		return 1
	}
	return 0
}
