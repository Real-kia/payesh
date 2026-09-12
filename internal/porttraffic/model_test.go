package porttraffic

import (
	"strings"
	"testing"
	"time"
)

func TestScopesRejectRemotePortAmbiguityAndDuplicateCounters(t *testing.T) {
	valid := Scope{ID: "https-in", Protocol: TCP, Interface: "eth0", LocalPort: 443, Direction: Inbound, Tuple: TranslatedTuple}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	duplicate := valid
	duplicate.ID = "same-counter"
	if err := ValidateScopes([]Scope{valid, duplicate}); err == nil || !strings.Contains(err.Error(), "same counter") {
		t.Fatalf("duplicate counter was accepted: %v", err)
	}
	invalid := valid
	invalid.Direction = "remote-destination"
	if err := invalid.Validate(); err == nil {
		t.Fatal("remote destination port was accepted as a local service direction")
	}
	invalid = valid
	invalid.Path = "bridge-or-magic"
	if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "path must be local or forwarded") {
		t.Fatalf("unknown forwarded path was accepted: %v", err)
	}
}

func TestForwardedAndLocalScopesAreDistinctCounters(t *testing.T) {
	local := Scope{ID: "local", Protocol: TCP, Interface: "eth0", LocalPort: 443, Direction: Inbound, Tuple: TranslatedTuple}
	forwarded := local
	forwarded.ID = "forwarded"
	forwarded.Path = ForwardedPath
	if err := ValidateScopes([]Scope{local, forwarded}); err != nil {
		t.Fatalf("forwarded path collided with local scope: %v", err)
	}
}

func TestTrackerMarksCoverageStartReloadAndResetUncertain(t *testing.T) {
	tracker := NewTracker()
	t0 := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	assertDelta := func(counter Counter, continuity, reason string, bytes uint64) {
		t.Helper()
		deltas, err := tracker.Observe([]Counter{counter})
		if err != nil {
			t.Fatal(err)
		}
		if len(deltas) != 1 || deltas[0].Continuity != continuity || deltas[0].Reason != reason || deltas[0].Bytes != bytes {
			t.Fatalf("unexpected delta: %+v", deltas)
		}
	}
	assertDelta(Counter{ScopeID: "https-in", Bytes: 1000, Packets: 10, Generation: "g1", ObservedAt: t0}, "uncertain", "coverage-start", 0)
	assertDelta(Counter{ScopeID: "https-in", Bytes: 1600, Packets: 14, Generation: "g1", ObservedAt: t0.Add(time.Second)}, "complete", "", 600)
	assertDelta(Counter{ScopeID: "https-in", Bytes: 50, Packets: 1, Generation: "g2", ObservedAt: t0.Add(2 * time.Second)}, "uncertain", "rule-reload", 0)
	assertDelta(Counter{ScopeID: "https-in", Bytes: 25, Packets: 1, Generation: "g2", ObservedAt: t0.Add(3 * time.Second)}, "uncertain", "counter-reset", 0)
}

func TestPlanRulesSeparatesInboundOutboundAndTupleViews(t *testing.T) {
	scopes := []Scope{
		{ID: "web-in-original", Protocol: TCP, Interface: "eth0", LocalPort: 443, Direction: Inbound, Tuple: OriginalTuple},
		{ID: "dns-out-translated", Protocol: UDP, Interface: "eth0", LocalPort: 53, Direction: Outbound, Tuple: TranslatedTuple},
	}
	rules, err := PlanRules(scopes, "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 {
		t.Fatalf("rule count=%d", len(rules))
	}
	if rules[0].Hook != "input" || strings.Join(rules[0].Match, " ") != "iifname eth0 meta l4proto tcp ct original proto-dst 443" {
		t.Fatalf("inbound original rule=%+v", rules[0])
	}
	if rules[2].Hook != "output" || strings.Join(rules[2].Match, " ") != "oifname eth0 udp sport 53" {
		t.Fatalf("outbound translated rule=%+v", rules[2])
	}
	for _, rule := range rules {
		if !strings.HasPrefix(rule.Comment, "payesh:generation-1:") {
			t.Fatalf("rule lacks ownership marker: %+v", rule)
		}
	}
}

func TestPlanRulesUsesForwardHookForForwardedTraffic(t *testing.T) {
	scope := Scope{ID: "container-web", Protocol: TCP, Interface: "veth0", LocalPort: 8080, Direction: Inbound, Tuple: TranslatedTuple, Path: ForwardedPath}
	rules, err := PlanRules([]Scope{scope}, "generation-forwarded")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].Hook != "forward" || strings.Join(rules[0].Match, " ") != "iifname veth0 tcp dport 8080" {
		t.Fatalf("forwarded inbound rules=%+v", rules)
	}
	if rules[1].Hook != "forward" {
		t.Fatalf("IPv6 forwarded rule hook=%q", rules[1].Hook)
	}
}
