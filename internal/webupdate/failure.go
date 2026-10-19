package webupdate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/Real-kia/payesh/internal/updater"
)

// StageError retains the detailed root-worker cause while the browser receives
// a safe description of the failed step, without URLs, credentials, or paths.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return fmt.Sprintf("%s: %v", e.Stage, e.Err) }
func (e *StageError) Unwrap() error { return e.Err }

func failureMessage(req Request, cause error) string {
	reason := "Update failed"
	var stage *StageError
	var network net.Error
	switch {
	case errors.Is(cause, updater.ErrInsufficientDisk), errors.Is(cause, syscall.ENOSPC):
		reason = "Not enough disk space to stage the update"
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "Update timed out; check the worker log before retrying"
	case errors.As(cause, &network):
		reason = "Could not download the release; check the hub's network connection"
	case errors.As(cause, &stage):
		switch stage.Stage {
		case "preflight":
			reason = "Release verification or compatibility check failed"
		case "installation":
			reason = "Release installation failed"
		case "health":
			reason = "Updated version or service health check failed"
		}
	}
	return fmt.Sprintf("%s. See the payesh-update service log for job %s (target %s).", reason, req.JobID, req.Version)
}
