package modules

import (
	"fmt"

	"github.com/Real-kia/payesh/internal/contracts"
)

// Event names one lifecycle transition attempt. Events are deliberately
// coarse (not "extract tar entry 3") so the state machine only encodes the
// install-sequence requirement of PLAN.md section 11: install is kept
// separate from activation, and a failed step preserves the previous
// working state instead of leaving the module half-installed.
type Event string

const (
	EventDownloadStart Event = "download_start"
	EventVerifyStart   Event = "verify_start"
	EventVerifyFailed  Event = "verify_failed"
	EventInstallStart  Event = "install_start"
	EventInstallOK     Event = "install_ok"
	EventInstallFailed Event = "install_failed"
	EventEnable        Event = "enable"
	EventDisable       Event = "disable"
	EventUpdateStart   Event = "update_start"
	EventUpdateOK      Event = "update_ok"
	EventUpdateFailed  Event = "update_failed"
	EventRemoveStart   Event = "remove_start"
	EventRemoveOK      Event = "remove_ok"
	EventRemoveFailed  Event = "remove_failed"
)

// transitions maps (current state, event) to next state. Only combinations
// listed here are legal; everything else is a rejected transition. A failed
// update returns to the state it started from (enabled or installed-disabled)
// rather than to "failed", satisfying "preserve the previous working state on
// failed upgrades" — the caller supplies that return state explicitly via
// NextState's fromBeforeUpdate parameter.
var transitions = map[contracts.ModuleState]map[Event]contracts.ModuleState{
	contracts.ModuleUnavailable: {
		EventDownloadStart: contracts.ModuleDownloading,
	},
	contracts.ModuleDownloading: {
		EventVerifyStart: contracts.ModuleVerifying,
	},
	contracts.ModuleVerifying: {
		EventInstallStart: contracts.ModuleInstalling,
		EventVerifyFailed: contracts.ModuleFailed,
	},
	contracts.ModuleInstalling: {
		// A completed install activates to installed-disabled, never
		// directly to enabled: install and activation stay separate.
		EventInstallOK:     contracts.ModuleInstalledDisabled,
		EventInstallFailed: contracts.ModuleFailed,
	},
	contracts.ModuleInstalledDisabled: {
		EventEnable:      contracts.ModuleEnabled,
		EventUpdateStart: contracts.ModuleUpdating,
		// Remove is legal only once disabled: disabling first is what stops
		// userspace work and tears down kernel rules before files are
		// removed (PLAN.md section 11). Removing straight from "enabled" is
		// deliberately not a table entry.
		EventRemoveStart: contracts.ModuleRemoving,
	},
	contracts.ModuleEnabled: {
		EventDisable:     contracts.ModuleInstalledDisabled,
		EventUpdateStart: contracts.ModuleUpdating,
	},
	contracts.ModuleUpdating: {
		EventUpdateOK: contracts.ModuleInstalledDisabled,
		// EventUpdateFailed is handled specially in NextState: it returns to
		// whichever state (enabled or installed-disabled) update_start began
		// from, not a fixed table entry.
	},
	contracts.ModuleRemoving: {
		EventRemoveOK:     contracts.ModuleUnavailable,
		EventRemoveFailed: contracts.ModuleFailed,
	},
	contracts.ModuleFailed: {
		// A failed verify/install can be retried from scratch; a failed
		// remove keeps its recovery executables and can be retried too.
		EventDownloadStart: contracts.ModuleDownloading,
		EventRemoveStart:   contracts.ModuleRemoving,
	},
}

// NextState validates one lifecycle transition. before is the state the
// installation was in immediately prior to an in-progress "updating"
// transition; it is only consulted for EventUpdateFailed/EventUpdateOK and
// ignored otherwise. Passing an unknown current state or an event not legal
// from it is an error, never a best-effort guess.
func NextState(current contracts.ModuleState, event Event, before contracts.ModuleState) (contracts.ModuleState, error) {
	if event == EventUpdateFailed || event == EventUpdateOK {
		if current != contracts.ModuleUpdating {
			return "", fmt.Errorf("modules: %s is only legal from updating, not %s", event, current)
		}
		if before != contracts.ModuleEnabled && before != contracts.ModuleInstalledDisabled {
			return "", fmt.Errorf("modules: update must have started from enabled or installed-disabled, not %s", before)
		}
		return before, nil
	}
	byEvent, ok := transitions[current]
	if !ok {
		return "", fmt.Errorf("modules: %s has no legal transitions", current)
	}
	next, ok := byEvent[event]
	if !ok {
		return "", fmt.Errorf("modules: event %s is not legal from state %s", event, current)
	}
	return next, nil
}
