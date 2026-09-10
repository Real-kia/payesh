package modules

import (
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestNextStateHappyPathKeepsInstallSeparateFromActivation(t *testing.T) {
	state := contracts.ModuleUnavailable
	steps := []struct {
		event Event
		want  contracts.ModuleState
	}{
		{EventDownloadStart, contracts.ModuleDownloading},
		{EventVerifyStart, contracts.ModuleVerifying},
		{EventInstallStart, contracts.ModuleInstalling},
		{EventInstallOK, contracts.ModuleInstalledDisabled},
	}
	for _, step := range steps {
		next, err := NextState(state, step.event, "")
		if err != nil {
			t.Fatalf("%s from %s: %v", step.event, state, err)
		}
		if next != step.want {
			t.Fatalf("%s from %s: got %s want %s", step.event, state, next, step.want)
		}
		state = next
	}
	if state != contracts.ModuleInstalledDisabled {
		t.Fatalf("a completed install must land on installed-disabled, not enabled; got %s", state)
	}
	next, err := NextState(state, EventEnable, "")
	if err != nil || next != contracts.ModuleEnabled {
		t.Fatalf("enable from installed-disabled: got %s, %v", next, err)
	}
}

func TestNextStateVerifyAndInstallFailuresLandOnFailed(t *testing.T) {
	next, err := NextState(contracts.ModuleVerifying, EventVerifyFailed, "")
	if err != nil || next != contracts.ModuleFailed {
		t.Fatalf("verify_failed: got %s, %v", next, err)
	}
	next, err = NextState(contracts.ModuleInstalling, EventInstallFailed, "")
	if err != nil || next != contracts.ModuleFailed {
		t.Fatalf("install_failed: got %s, %v", next, err)
	}
}

func TestNextStateFailedUpdatePreservesPreviousWorkingState(t *testing.T) {
	next, err := NextState(contracts.ModuleUpdating, EventUpdateFailed, contracts.ModuleEnabled)
	if err != nil {
		t.Fatal(err)
	}
	if next != contracts.ModuleEnabled {
		t.Fatalf("a failed upgrade must preserve the previous working state; got %s", next)
	}
	next, err = NextState(contracts.ModuleUpdating, EventUpdateFailed, contracts.ModuleInstalledDisabled)
	if err != nil || next != contracts.ModuleInstalledDisabled {
		t.Fatalf("got %s, %v", next, err)
	}
}

func TestNextStateRejectsIllegalTransitions(t *testing.T) {
	cases := []struct {
		current contracts.ModuleState
		event   Event
	}{
		{contracts.ModuleUnavailable, EventEnable},
		{contracts.ModuleUnavailable, EventRemoveStart},
		{contracts.ModuleEnabled, EventInstallStart},
		{contracts.ModuleRemoving, EventEnable},
		{contracts.ModuleUpdating, EventEnable},
	}
	for _, c := range cases {
		if _, err := NextState(c.current, c.event, ""); err == nil {
			t.Fatalf("expected %s to be illegal from %s", c.event, c.current)
		}
	}
	// update_failed/update_ok must only be reachable from "updating", and
	// only with a legal prior activation state.
	if _, err := NextState(contracts.ModuleEnabled, EventUpdateFailed, contracts.ModuleEnabled); err == nil {
		t.Fatal("expected update_failed to require the updating state")
	}
	if _, err := NextState(contracts.ModuleUpdating, EventUpdateFailed, contracts.ModuleFailed); err == nil {
		t.Fatal("expected update_failed to require a legal prior activation state")
	}
}

func TestNextStateAllowsRetryAfterFailure(t *testing.T) {
	next, err := NextState(contracts.ModuleFailed, EventDownloadStart, "")
	if err != nil || next != contracts.ModuleDownloading {
		t.Fatalf("expected a fresh retry from failed, got %s, %v", next, err)
	}
	// A remove that fails keeps recovery executables and can be retried.
	next, err = NextState(contracts.ModuleFailed, EventRemoveStart, "")
	if err != nil || next != contracts.ModuleRemoving {
		t.Fatalf("expected remove retry from failed, got %s, %v", next, err)
	}
}
