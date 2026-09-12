package porttraffic

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type runnerCall struct {
	args  []string
	input string
}

type fakeRunner struct {
	results []CommandResult
	errors  []error
	calls   []runnerCall
}

func (r *fakeRunner) Run(_ context.Context, args []string, input []byte) (CommandResult, error) {
	r.calls = append(r.calls, runnerCall{args: append([]string(nil), args...), input: string(input)})
	index := len(r.calls) - 1
	return r.results[index], r.errors[index]
}

func TestNftApplyChecksThenAtomicallyInstallsOnlyOwnedTable(t *testing.T) {
	runner := &fakeRunner{
		results: []CommandResult{{Stderr: []byte("No such file or directory")}, {}, {}},
		errors:  []error{errors.New("exit 1"), nil, nil},
	}
	backend := NftBackend{Runner: runner}
	scopes := []Scope{{ID: "https", Protocol: TCP, Interface: "eth0", LocalPort: 443, Direction: Inbound, Tuple: OriginalTuple}}
	if err := backend.Apply(context.Background(), scopes, "g1"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 || strings.Join(runner.calls[1].args, " ") != "--check -f -" || strings.Join(runner.calls[2].args, " ") != "-f -" {
		t.Fatalf("unexpected command sequence: %+v", runner.calls)
	}
	script := runner.calls[2].input
	for _, required := range []string{
		`add table inet payesh_port_traffic { comment "payesh-owned:port-traffic:v1"; }`,
		`add chain inet payesh_port_traffic payesh_forward { type filter hook forward priority filter; policy accept; }`,
		`meta nfproto ipv4 iifname "eth0" meta l4proto tcp ct original proto-dst 443`,
		`meta nfproto ipv6 iifname "eth0" meta l4proto tcp ct original proto-dst 443`,
		`counter name c_`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("ruleset lacks %q:\n%s", required, script)
		}
	}
	if strings.Contains(script, "flush ruleset") || strings.Contains(script, "delete table inet filter") {
		t.Fatalf("ruleset contains broad destructive command:\n%s", script)
	}
}

func TestNftApplyAndRemoveRefuseForeignSameNameTable(t *testing.T) {
	foreign := []byte(`{"nftables":[{"table":{"family":"inet","name":"payesh_port_traffic","comment":"owned-by-someone-else"}}]}`)
	runner := &fakeRunner{results: []CommandResult{{Stdout: foreign}, {Stdout: foreign}}, errors: []error{nil, nil}}
	backend := NftBackend{Runner: runner}
	scope := Scope{ID: "dns", Protocol: UDP, Interface: "ens3", LocalPort: 53, Direction: Outbound, Tuple: TranslatedTuple}
	if err := backend.Apply(context.Background(), []Scope{scope}, "g1"); !errors.Is(err, ErrForeignTable) {
		t.Fatalf("foreign apply error=%v", err)
	}
	if err := backend.Remove(context.Background()); !errors.Is(err, ErrForeignTable) {
		t.Fatalf("foreign remove error=%v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("foreign table caused mutation: %+v", runner.calls)
	}
}

func TestNftOwnershipFallsBackToTextWhenJSONOmitsTableComment(t *testing.T) {
	runner := &fakeRunner{
		results: []CommandResult{
			{Stdout: []byte(`{"nftables":[{"table":{"family":"inet","name":"payesh_port_traffic","handle":8}}]}`)},
			{Stdout: []byte("table inet payesh_port_traffic { # handle 8\n\tcomment \"payesh-owned:port-traffic:v1\"\n}\n")},
		},
		errors: []error{nil, nil},
	}
	exists, owned, err := (NftBackend{Runner: runner}).inspectOwnership(context.Background())
	if err != nil || !exists || !owned {
		t.Fatalf("exists=%v owned=%v err=%v", exists, owned, err)
	}
}

func TestNftOwnershipTextFallbackRequiresExactMarker(t *testing.T) {
	runner := &fakeRunner{
		results: []CommandResult{
			{Stdout: []byte(`{"nftables":[{"table":{"family":"inet","name":"payesh_port_traffic"}}]}`)},
			{Stdout: []byte("table inet payesh_port_traffic {\n\tcomment \"payesh-owned:port-traffic:v1-extra\"\n}\n")},
		},
		errors: []error{nil, nil},
	}
	exists, owned, err := (NftBackend{Runner: runner}).inspectOwnership(context.Background())
	if err != nil || !exists || owned {
		t.Fatalf("exists=%v owned=%v err=%v", exists, owned, err)
	}
}

func TestNftSnapshotReadsOwnedNamedCounters(t *testing.T) {
	owned := []byte(`{"nftables":[{"table":{"family":"inet","name":"payesh_port_traffic","comment":"payesh-owned:port-traffic:v1"}}]}`)
	counters := []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}},{"counter":{"family":"inet","table":"payesh_port_traffic","name":"c_1","packets":12,"bytes":9007199254740993,"comment":"payesh-counter:g1:https-in"}}]}`)
	runner := &fakeRunner{results: []CommandResult{{Stdout: owned}, {Stdout: counters}}, errors: []error{nil, nil}}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	values, err := (NftBackend{Runner: runner, Now: func() time.Time { return now }}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].ScopeID != "https-in" || values[0].Generation != "g1" || values[0].Bytes != 9007199254740993 || !values[0].ObservedAt.Equal(now) {
		t.Fatalf("snapshot=%+v", values)
	}
}

func TestNftSnapshotFallsBackToTextCounterComments(t *testing.T) {
	ownedWithoutJSONComment := []byte(`{"nftables":[{"table":{"family":"inet","name":"payesh_port_traffic"}}]}`)
	ownedText := []byte("table inet payesh_port_traffic {\n\tcomment \"payesh-owned:port-traffic:v1\"\n}\n")
	countersWithoutJSONComments := []byte(`{"nftables":[{"counter":{"family":"inet","table":"payesh_port_traffic","name":"c_1","packets":12,"bytes":34}}]}`)
	scope := "https-in"
	countersText := []byte("table inet payesh_port_traffic {\n\tcounter " + counterName(scope) + " {\n\t\tcomment \"payesh-counter:g1:https-in\"\n\t\tpackets 12 bytes 34\n\t}\n}\n")
	runner := &fakeRunner{
		results: []CommandResult{{Stdout: ownedWithoutJSONComment}, {Stdout: ownedText}, {Stdout: countersWithoutJSONComments}, {Stdout: countersText}},
		errors:  []error{nil, nil, nil, nil},
	}
	values, err := (NftBackend{Runner: runner}).Snapshot(context.Background())
	if err != nil || len(values) != 1 || values[0].ScopeID != scope || values[0].Packets != 12 || values[0].Bytes != 34 {
		t.Fatalf("values=%+v err=%v", values, err)
	}
}

func TestNftCommandErrorsAreBounded(t *testing.T) {
	runner := &fakeRunner{results: []CommandResult{{Stderr: []byte(strings.Repeat("x", 2000))}}, errors: []error{errors.New("exit 1")}}
	_, _, err := (NftBackend{Runner: runner}).inspectOwnership(context.Background())
	if err == nil || len(err.Error()) > 600 {
		t.Fatalf("unbounded or missing error: %v", err)
	}
}

func TestNftUnsupportedOwnershipSyntaxIsTyped(t *testing.T) {
	runner := &fakeRunner{
		results: []CommandResult{{Stderr: []byte("No such file or directory")}, {Stderr: []byte("syntax error, unexpected comment")}},
		errors:  []error{errors.New("exit 1"), errors.New("exit 1")},
	}
	scope := Scope{ID: "https", Protocol: TCP, Interface: "lo", LocalPort: 443, Direction: Inbound, Tuple: TranslatedTuple}
	err := (NftBackend{Runner: runner}).Apply(context.Background(), []Scope{scope}, "g1")
	if !errors.Is(err, ErrNftablesUnsupported) {
		t.Fatalf("expected typed unsupported error, got %v", err)
	}
}

func TestNftSnapshotRejectsDuplicateOwnedScope(t *testing.T) {
	owned := []byte(`{"nftables":[{"table":{"family":"inet","name":"payesh_port_traffic","comment":"payesh-owned:port-traffic:v1"}}]}`)
	counters := []byte(`{"nftables":[{"counter":{"packets":1,"bytes":2,"comment":"payesh-counter:g1:dup"}},{"counter":{"packets":3,"bytes":4,"comment":"payesh-counter:g1:dup"}}]}`)
	runner := &fakeRunner{results: []CommandResult{{Stdout: owned}, {Stdout: counters}}, errors: []error{nil, nil}}
	_, err := (NftBackend{Runner: runner}).Snapshot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "duplicate nftables counter") {
		t.Fatalf("duplicate counter was accepted: %v", err)
	}
}

func TestNftSnapshotRejectsUnboundedGeneration(t *testing.T) {
	owned := []byte(`{"nftables":[{"table":{"family":"inet","name":"payesh_port_traffic","comment":"payesh-owned:port-traffic:v1"}}]}`)
	counters := []byte(`{"nftables":[{"counter":{"packets":1,"bytes":2,"comment":"payesh-counter:` + strings.Repeat("g", 65) + `:scope"}}]}`)
	runner := &fakeRunner{results: []CommandResult{{Stdout: owned}, {Stdout: counters}}, errors: []error{nil, nil}}
	_, err := (NftBackend{Runner: runner}).Snapshot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ownership metadata is invalid") {
		t.Fatalf("unbounded generation was accepted: %v", err)
	}
}
