package bandwidth

import (
	"context"
	"errors"
	"testing"

	"github.com/Real-kia/payesh/internal/porttraffic"
)

type tcFakeRunner struct {
	results []porttraffic.CommandResult
	errors  []error
	index   int
}

func (r *tcFakeRunner) Run(context.Context, []string, []byte) (porttraffic.CommandResult, error) {
	if r.index >= len(r.results) {
		return porttraffic.CommandResult{}, errors.New("unexpected tc call")
	}
	result, err := r.results[r.index], r.errors[r.index]
	r.index++
	return result, err
}

func qdiscInspectionRunner(qdisc string) *tcFakeRunner {
	return &tcFakeRunner{results: []porttraffic.CommandResult{{Stdout: []byte(qdisc)}, {Stdout: []byte(`[]`)}, {Stdout: []byte(`[]`)}}, errors: []error{nil, nil, nil}}
}

func TestTCInspectionAllowsClsactPolicyBesideForeignRootButRefusesShaper(t *testing.T) {
	root := `[{"kind":"fq_codel","root":true,"handle":"0:"}]`
	inbound := Preview{Request: Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Inbound}, Action: Block}, AffectsNetwork: true}
	backend := TCBackend{Runner: qdiscInspectionRunner(root), StateDir: t.TempDir()}
	ownership, _, err := backend.Inspect(context.Background(), inbound)
	if err != nil || !ownership.Compatible {
		t.Fatalf("inbound ownership=%+v err=%v", ownership, err)
	}

	outbound := Preview{Request: Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound}, Action: Throttle, BitsPerSecond: 10_000_000}, AffectsNetwork: true}
	backend.Runner = qdiscInspectionRunner(root)
	ownership, _, err = backend.Inspect(context.Background(), outbound)
	if err != nil || ownership.Compatible || ownership.Owner != "foreign" {
		t.Fatalf("outbound ownership=%+v err=%v", ownership, err)
	}
}

func TestTCInspectionRefusesExistingClsactFilters(t *testing.T) {
	runner := &tcFakeRunner{results: []porttraffic.CommandResult{{Stdout: []byte(`[{"kind":"noqueue","root":true},{"kind":"clsact","handle":"ffff:"}]`)}, {Stdout: []byte(`[{"kind":"flower","pref":10}]`)}, {Stdout: []byte(`[]`)}}, errors: []error{nil, nil, nil}}
	preview := Preview{Request: Request{Scope: Scope{TargetKind: "local-port", Interface: "eth0", Protocol: porttraffic.TCP, LocalPort: 443, Direction: porttraffic.Inbound}, Action: Block}, AffectsNetwork: true}
	ownership, _, err := (TCBackend{Runner: runner, StateDir: t.TempDir()}).Inspect(context.Background(), preview)
	if err != nil || ownership.Compatible {
		t.Fatalf("ownership=%+v err=%v", ownership, err)
	}
}

func TestTCApplyRechecksForeignOwnershipBeforeMutation(t *testing.T) {
	preview := Preview{Request: Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound}, Action: Throttle, BitsPerSecond: 8_000_000}, AffectsNetwork: true}
	runner := qdiscInspectionRunner(`[{"kind":"fq_codel","root":true,"handle":"0:"}]`)
	backend := TCBackend{Runner: runner, StateDir: t.TempDir()}
	err := backend.Apply(context.Background(), preview)
	if !errors.Is(err, ErrForeignNetworkState) {
		t.Fatalf("foreign direct apply error=%v", err)
	}
	if runner.index != 3 {
		t.Fatalf("foreign direct apply issued kernel mutations: calls=%d", runner.index)
	}
}

func TestTCRevertRefusesMissingOwnershipMarker(t *testing.T) {
	preview := Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Inbound}
	runner := qdiscInspectionRunner(`[]`)
	backend := TCBackend{Runner: runner, StateDir: t.TempDir()}
	err := backend.RevertOwned(context.Background(), Checkpoint{
		ID: "checkpoint", Scope: preview, Action: Block,
		Previous: []byte(`{"interface":"eth0","absent":true}`),
	})
	if !errors.Is(err, ErrTCMarkerMissing) {
		t.Fatalf("missing marker error=%v", err)
	}
	if runner.index != 0 {
		t.Fatalf("missing marker triggered tc commands: calls=%d", runner.index)
	}
}
