package webupdate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/updater"
)

func TestFleetFailureReportsSafeReasonAndCorrelatesWorkerLog(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{fmt.Errorf("staging: %w", updater.ErrInsufficientDisk), "disk space"},
		{&StageError{Stage: "preflight", Err: errors.New("private upstream details")}, "verification"},
		{&StageError{Stage: "installation", Err: errors.New("private upstream details")}, "installation"},
		{&StageError{Stage: "health", Err: errors.New("private upstream details")}, "health"},
	} {
		dir := t.TempDir()
		req := Request{JobID: "failure-log-reference", Version: "1.2.3", Deadline: time.Now().Add(time.Minute)}
		_, err := finishFleet(dir, req, StateFailed, tc.cause)
		if !errors.Is(err, tc.cause) {
			t.Fatalf("root cause lost: %v", err)
		}
		status, err := FleetResult(dir, req.JobID)
		if err != nil || !strings.Contains(status.Message, tc.want) || !strings.Contains(status.Message, req.JobID) || strings.Contains(status.Message, "private upstream") {
			t.Fatalf("message=%q err=%v", status.Message, err)
		}
	}
}
