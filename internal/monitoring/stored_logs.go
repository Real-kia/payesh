package monitoring

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// PackageLogSourceID is the log source for package install, update and
// failure events that Payesh records itself.
const PackageLogSourceID = "package-manager"

var storedLogSourceLabels = map[string]string{PackageLogSourceID: "Package manager"}

// storedLogSequence keeps cursors unique when two events share a timestamp.
var storedLogSequence atomic.Uint64

// IsStoredLogSource reports whether a source holds only entries Payesh writes
// itself. Such sources have no file or journald unit to read, so queries serve
// them from the database alone.
func IsStoredLogSource(id string) bool {
	_, ok := storedLogSourceLabels[id]
	return ok
}

// AppendStoredLog records one entry in a stored log source, registering the
// source on first use so it is listed with the server's other logs.
func (s *Store) AppendStoredLog(ctx context.Context, serverID contracts.ServerID, sourceID, severity, text string, at time.Time) error {
	label, ok := storedLogSourceLabels[sourceID]
	if !ok {
		return errors.New("not a stored log source")
	}
	if err := s.RegisterLogSource(ctx, LogSource{ServerID: serverID, ID: sourceID, Label: label}); err != nil {
		return err
	}
	_, err := s.InsertLogEntries(ctx, []LogEntry{{ServerID: serverID, SourceID: sourceID, Cursor: fmt.Sprintf("%s-%d-%d", sourceID, at.UnixNano(), storedLogSequence.Add(1)), Timestamp: at, Severity: severity, Text: text}})
	return err
}
