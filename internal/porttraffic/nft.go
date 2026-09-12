package porttraffic

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ownershipComment = "payesh-owned:port-traffic:v1"

var (
	ErrNftablesUnavailable = errors.New("nftables_unavailable")
	ErrNftablesUnsupported = errors.New("nftables_unsupported")
	ErrForeignTable        = errors.New("port_traffic_foreign_table")
	ErrOwnedTableMissing   = errors.New("port_traffic_owned_table_missing")
)

type CommandResult struct {
	Stdout []byte
	Stderr []byte
}

type CommandRunner interface {
	Run(context.Context, []string, []byte) (CommandResult, error)
}

type ExecRunner struct{ Path string }

func (r ExecRunner) Run(ctx context.Context, args []string, input []byte) (CommandResult, error) {
	path := r.Path
	if path == "" {
		path = "nft"
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
}

type NftBackend struct {
	Runner CommandRunner
	Now    func() time.Time
}

func (b NftBackend) runner() CommandRunner {
	if b.Runner != nil {
		return b.Runner
	}
	return ExecRunner{}
}

// Apply atomically replaces only a table that is absent or already carries
// Payesh's exact ownership marker. It never emits flush-ruleset or addresses a
// foreign table.
func (b NftBackend) Apply(ctx context.Context, scopes []Scope, generation string) error {
	if runtime.GOOS != "linux" && b.Runner == nil {
		return ErrNftablesUnavailable
	}
	rules, err := PlanRules(scopes, generation)
	if err != nil {
		return err
	}
	exists, owned, err := b.inspectOwnership(ctx)
	if err != nil {
		return err
	}
	if exists && !owned {
		return ErrForeignTable
	}
	script := renderRuleset(scopes, rules, generation, exists)
	result, err := b.runner().Run(ctx, []string{"--check", "-f", "-"}, script)
	if err != nil {
		if nftablesOwnershipCommentsUnsupported(result) {
			return fmt.Errorf("%w: nftables userspace does not support ownership comments", ErrNftablesUnsupported)
		}
		return commandError("validate owned nftables rules", result, err)
	}
	result, err = b.runner().Run(ctx, []string{"-f", "-"}, script)
	if err != nil {
		return commandError("apply owned nftables rules", result, err)
	}
	return nil
}

func nftablesOwnershipCommentsUnsupported(result CommandResult) bool {
	message := strings.ToLower(string(result.Stderr))
	return strings.Contains(message, "unexpected comment") ||
		strings.Contains(message, "unknown keyword") && strings.Contains(message, "comment")
}

func (b NftBackend) Remove(ctx context.Context) error {
	if runtime.GOOS != "linux" && b.Runner == nil {
		return ErrNftablesUnavailable
	}
	exists, owned, err := b.inspectOwnership(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !owned {
		return ErrForeignTable
	}
	result, err := b.runner().Run(ctx, []string{"delete", "table", "inet", OwnedTable}, nil)
	if err != nil {
		return commandError("remove owned nftables table", result, err)
	}
	return nil
}

func (b NftBackend) Snapshot(ctx context.Context) ([]Counter, error) {
	exists, owned, err := b.inspectOwnership(ctx)
	if err != nil {
		return nil, err
	}
	if !exists || !owned {
		if exists {
			return nil, ErrForeignTable
		}
		return nil, ErrOwnedTableMissing
	}
	result, err := b.runner().Run(ctx, []string{"--json", "list", "counters", "table", "inet", OwnedTable}, nil)
	if err != nil {
		return nil, commandError("read owned nftables counters", result, err)
	}
	now := time.Now().UTC()
	if b.Now != nil {
		now = b.Now().UTC()
	}
	counters, parseErr := parseCounters(result.Stdout, now)
	if parseErr == nil || !strings.Contains(parseErr.Error(), "lacks valid Payesh ownership metadata") {
		return counters, parseErr
	}
	// nftables 1.0.2 also omits named-counter comments from JSON even
	// though its text output retains them. Fall back to the same narrowly
	// scoped table query and apply the identical ownership validation.
	text, err := b.runner().Run(ctx, []string{"list", "counters", "table", "inet", OwnedTable}, nil)
	if err != nil {
		return nil, commandError("read owned nftables counter comments", text, err)
	}
	return parseTextCounters(text.Stdout, now)
}

func (b NftBackend) inspectOwnership(ctx context.Context) (bool, bool, error) {
	result, err := b.runner().Run(ctx, []string{"--json", "list", "table", "inet", OwnedTable}, nil)
	if err != nil {
		message := strings.ToLower(string(result.Stderr))
		if strings.Contains(message, "no such file") || strings.Contains(message, "does not exist") {
			return false, false, nil
		}
		return false, false, commandError("inspect nftables ownership", result, err)
	}
	var document struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(result.Stdout, &document); err != nil {
		return false, false, fmt.Errorf("decode nftables table: %w", err)
	}
	for _, item := range document.Nftables {
		var table struct {
			Family  string `json:"family"`
			Name    string `json:"name"`
			Comment string `json:"comment"`
		}
		if raw, ok := item["table"]; ok && json.Unmarshal(raw, &table) == nil && table.Family == "inet" && table.Name == OwnedTable {
			if table.Comment != "" {
				return true, table.Comment == ownershipComment, nil
			}
			// nftables 1.0.2 (Ubuntu 22.04) accepts and preserves table
			// comments but omits them from its JSON representation. Query the
			// same single table in text mode before treating it as foreign. This
			// remains fail-closed: only the exact table-level marker is accepted.
			text, textErr := b.runner().Run(ctx, []string{"list", "table", "inet", OwnedTable}, nil)
			if textErr != nil {
				return true, false, commandError("inspect nftables ownership comment", text, textErr)
			}
			return true, hasExactTableOwnershipComment(text.Stdout), nil
		}
	}
	return false, false, errors.New("nftables returned no requested table")
}

func hasExactTableOwnershipComment(output []byte) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == `comment "`+ownershipComment+`"` {
			return true
		}
	}
	return false
}

func renderRuleset(scopes []Scope, rules []Rule, generation string, replace bool) []byte {
	var output strings.Builder
	if replace {
		fmt.Fprintf(&output, "delete table inet %s\n", OwnedTable)
	}
	fmt.Fprintf(&output, "add table inet %s { comment %s; }\n", OwnedTable, nftQuote(ownershipComment))
	fmt.Fprintf(&output, "add chain inet %s payesh_input { type filter hook input priority filter; policy accept; }\n", OwnedTable)
	fmt.Fprintf(&output, "add chain inet %s payesh_output { type filter hook output priority filter; policy accept; }\n", OwnedTable)
	fmt.Fprintf(&output, "add chain inet %s payesh_forward { type filter hook forward priority filter; policy accept; }\n", OwnedTable)
	ordered := append([]Scope(nil), scopes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, scope := range ordered {
		fmt.Fprintf(&output, "add counter inet %s %s { comment %s; }\n", OwnedTable, counterName(scope.ID), nftQuote("payesh-counter:"+generation+":"+scope.ID))
	}
	byID := make(map[string]Scope, len(scopes))
	for _, scope := range scopes {
		byID[scope.ID] = scope
	}
	for _, rule := range rules {
		chain := "payesh_" + rule.Hook
		scope := byID[rule.ScopeID]
		interfaceSelector, portSelector := "iifname", "dport"
		if scope.Direction == Outbound {
			interfaceSelector, portSelector = "oifname", "sport"
		}
		fmt.Fprintf(&output, "add rule inet %s %s meta nfproto %s %s %s ", OwnedTable, chain, map[string]string{"ip": "ipv4", "ip6": "ipv6"}[rule.Family], interfaceSelector, nftQuote(scope.Interface))
		if scope.Tuple == OriginalTuple {
			fmt.Fprintf(&output, "meta l4proto %s ct original proto-dst %d ", scope.Protocol, scope.LocalPort)
		} else {
			fmt.Fprintf(&output, "%s %s %d ", scope.Protocol, portSelector, scope.LocalPort)
		}
		fmt.Fprintf(&output, "counter name %s comment %s\n", counterName(rule.ScopeID), nftQuote(rule.Comment))
	}
	return []byte(output.String())
}

func counterName(scopeID string) string {
	digest := sha256.Sum256([]byte(scopeID))
	return "c_" + hex.EncodeToString(digest[:8])
}

func nftQuote(value string) string { return strconv.Quote(value) }

func parseCounters(data []byte, observedAt time.Time) ([]Counter, error) {
	var document struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode nftables counters: %w", err)
	}
	result := make([]Counter, 0)
	seen := make(map[string]struct{})
	for _, item := range document.Nftables {
		var value struct {
			Packets uint64 `json:"packets"`
			Bytes   uint64 `json:"bytes"`
			Comment string `json:"comment"`
		}
		if raw, ok := item["counter"]; !ok || json.Unmarshal(raw, &value) != nil {
			continue
		}
		// Generation and scope are encoded after the fixed two-field prefix.
		// Scope IDs currently permit ':'; recover using the first separator.
		if !strings.HasPrefix(value.Comment, "payesh-counter:") {
			return nil, errors.New("counter lacks valid Payesh ownership metadata")
		}
		generationAndScope := strings.TrimPrefix(value.Comment, "payesh-counter:")
		separator := strings.IndexByte(generationAndScope, ':')
		if separator < 1 || separator == len(generationAndScope)-1 {
			return nil, errors.New("counter ownership metadata is malformed")
		}
		generation, scopeID := generationAndScope[:separator], generationAndScope[separator+1:]
		if !interfacePattern.MatchString(scopeID) || generation == "" || len(generation) > 64 || strings.ContainsAny(generation, "\n\r\x00") {
			return nil, errors.New("counter ownership metadata is invalid")
		}
		if _, exists := seen[scopeID]; exists {
			return nil, fmt.Errorf("duplicate nftables counter for scope %q", scopeID)
		}
		seen[scopeID] = struct{}{}
		result = append(result, Counter{ScopeID: scopeID, Bytes: value.Bytes, Packets: value.Packets, Generation: generation, ObservedAt: observedAt})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ScopeID < result[j].ScopeID })
	if len(result) == 0 || len(result) > 256 {
		return nil, errors.New("nftables counter result is empty or exceeds limit")
	}
	return result, nil
}

func parseTextCounters(data []byte, observedAt time.Time) ([]Counter, error) {
	type textCounter struct {
		name, comment  string
		packets, bytes uint64
		hasValues      bool
	}
	var current *textCounter
	result := make([]Counter, 0)
	seen := make(map[string]struct{})
	finish := func() error {
		if current == nil {
			return nil
		}
		if !strings.HasPrefix(current.name, "c_") || current.comment == "" || !current.hasValues {
			return errors.New("counter lacks valid Payesh ownership metadata")
		}
		generationAndScope := strings.TrimPrefix(current.comment, "payesh-counter:")
		separator := strings.IndexByte(generationAndScope, ':')
		if !strings.HasPrefix(current.comment, "payesh-counter:") || separator < 1 || separator == len(generationAndScope)-1 {
			return errors.New("counter ownership metadata is malformed")
		}
		generation, scopeID := generationAndScope[:separator], generationAndScope[separator+1:]
		if !interfacePattern.MatchString(scopeID) || generation == "" || len(generation) > 64 || strings.ContainsAny(generation, "\n\r\x00") {
			return errors.New("counter ownership metadata is invalid")
		}
		if current.name != counterName(scopeID) {
			return errors.New("counter name does not match its owned scope")
		}
		if _, exists := seen[scopeID]; exists {
			return fmt.Errorf("duplicate nftables counter for scope %q", scopeID)
		}
		seen[scopeID] = struct{}{}
		result = append(result, Counter{ScopeID: scopeID, Bytes: current.bytes, Packets: current.packets, Generation: generation, ObservedAt: observedAt})
		current = nil
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "counter" && fields[2] == "{" {
			if err := finish(); err != nil {
				return nil, err
			}
			current = &textCounter{name: fields[1]}
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(line, "comment ") {
			comment, err := strconv.Unquote(strings.TrimSpace(strings.TrimPrefix(line, "comment ")))
			if err != nil {
				return nil, errors.New("counter ownership metadata is malformed")
			}
			current.comment = comment
		} else if len(fields) == 4 && fields[0] == "packets" && fields[2] == "bytes" {
			packets, pErr := strconv.ParseUint(fields[1], 10, 64)
			byteCount, bErr := strconv.ParseUint(fields[3], 10, 64)
			if pErr != nil || bErr != nil {
				return nil, errors.New("nftables counter values are invalid")
			}
			current.packets, current.bytes, current.hasValues = packets, byteCount, true
		} else if line == "}" {
			if err := finish(); err != nil {
				return nil, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := finish(); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ScopeID < result[j].ScopeID })
	if len(result) == 0 || len(result) > 256 {
		return nil, errors.New("nftables counter result is empty or exceeds limit")
	}
	return result, nil
}

func commandError(operation string, result CommandResult, cause error) error {
	message := strings.TrimSpace(string(result.Stderr))
	if len(message) > 512 {
		message = message[:512]
	}
	if message == "" {
		return fmt.Errorf("%s: %w", operation, cause)
	}
	return fmt.Errorf("%s: %s: %w", operation, message, cause)
}
