package monitoring

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Real-kia/payesh/internal/contracts"
)

const (
	MaxServerTags   = 16
	MaxServerTagLen = 32
)

var ErrInvalidTags = errors.New("tags must be 1–32 printable characters, at most 16 per server")

// NormalizeTags trims, drops empties and case-insensitive duplicates, and
// validates the result.
func NormalizeTags(tags []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, tag := range tags {
		tag = strings.Join(strings.Fields(tag), " ")
		if tag == "" {
			continue
		}
		if utf8.RuneCountInString(tag) > MaxServerTagLen || strings.ContainsFunc(tag, func(r rune) bool { return !unicode.IsPrint(r) || r == ',' }) {
			return nil, ErrInvalidTags
		}
		key := strings.ToLower(tag)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, tag)
	}
	if len(out) > MaxServerTags {
		return nil, ErrInvalidTags
	}
	return out, nil
}

// SetServerTags replaces a server's tags.
func (s *Store) SetServerTags(ctx context.Context, serverID contracts.ServerID, tags []string) ([]string, error) {
	normalized, err := NormalizeTags(tags)
	if err != nil {
		return nil, err
	}
	if !validStoreServerID(serverID) {
		return nil, errors.New("server_id must be a bounded URL-safe identifier")
	}
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM servers WHERE id=?)`, string(serverID)).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return sql.ErrNoRows
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM server_tags WHERE server_id=?`, string(serverID)); err != nil {
			return err
		}
		for _, tag := range normalized {
			if _, err := tx.ExecContext(ctx, `INSERT INTO server_tags(server_id,tag) VALUES(?,?)`, string(serverID), tag); err != nil {
				return err
			}
		}
		return nil
	})
	return normalized, err
}
