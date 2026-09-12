package porttraffic

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const OwnedTable = "payesh_port_traffic"

// Rule is a declarative classifier consumed by the separately installed
// privileged module. It intentionally does not expose arbitrary nft syntax.
type Rule struct {
	ScopeID string
	Family  string
	Hook    string
	Match   []string
	Comment string
}

// PlanRules creates distinct IPv4 and IPv6 rules. The executor must create
// only OwnedTable and must refuse an existing table without Payesh ownership
// metadata; planning never flushes a system or third-party ruleset.
func PlanRules(scopes []Scope, generation string) ([]Rule, error) {
	if err := ValidateScopes(scopes); err != nil {
		return nil, err
	}
	if generation == "" || len(generation) > 64 || strings.ContainsAny(generation, "\n\r\x00") {
		return nil, errors.New("invalid rule generation")
	}
	rules := make([]Rule, 0, len(scopes)*2)
	for _, scope := range scopes {
		portSelector := "dport"
		hook := "input"
		if scope.Path == ForwardedPath {
			hook = "forward"
		} else if scope.Direction == Outbound {
			// A service reply is attributed by its local source port. This avoids
			// misclassifying an outbound client connecting to a remote same port.
			portSelector = "sport"
			hook = "output"
		}
		for _, family := range []string{"ip", "ip6"} {
			match := []string{"iifname", scope.Interface}
			if scope.Direction == Outbound {
				match[0] = "oifname"
			}
			if scope.Tuple == OriginalTuple {
				// Conntrack original tuples preserve the pre-NAT service identity.
				// The original service port is proto-dst for both request and
				// reply directions; using proto-src on replies would select the
				// remote client port instead.
				match = append(match, "meta", "l4proto", string(scope.Protocol), "ct", "original", "proto-dst", strconv.Itoa(int(scope.LocalPort)))
			} else {
				match = append(match, string(scope.Protocol), portSelector, strconv.Itoa(int(scope.LocalPort)))
			}
			rules = append(rules, Rule{ScopeID: scope.ID, Family: family, Hook: hook, Match: match, Comment: fmt.Sprintf("payesh:%s:%s", generation, scope.ID)})
		}
	}
	return rules, nil
}
