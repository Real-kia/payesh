package bandwidth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Real-kia/payesh/internal/porttraffic"
)

const (
	tcRootHandle = "7a00:"
	tcPrefBase   = 49000
)

var (
	ErrTCUnavailable   = errors.New("traffic_control_unavailable")
	ErrTCMarkerMissing = errors.New("tc_ownership_marker_missing")
)

type tcPrevious struct {
	Interface string `json:"interface"`
	Absent    bool   `json:"absent"`
}

type tcMarker struct {
	Scope       Scope  `json:"scope"`
	Fingerprint string `json:"fingerprint"`
}

// TCBackend is deliberately conservative: it creates a complete Payesh-owned
// tree only on an interface without a conflicting qdisc/filter tree, records
// a fingerprint, and refuses cleanup after an external change.
type TCBackend struct {
	Runner   porttraffic.CommandRunner
	StateDir string
}

func (b TCBackend) runner() porttraffic.CommandRunner {
	if b.Runner != nil {
		return b.Runner
	}
	return porttraffic.ExecRunner{Path: "tc"}
}

func (b TCBackend) Inspect(ctx context.Context, preview Preview) (Ownership, json.RawMessage, error) {
	scope := preview.Request.Scope
	if err := scope.Validate(); err != nil {
		return Ownership{}, nil, err
	}
	if runtime.GOOS != "linux" && b.Runner == nil {
		return Ownership{}, nil, ErrTCUnavailable
	}
	snapshot, err := b.snapshot(ctx, scope.Interface)
	if err != nil {
		return Ownership{}, nil, err
	}
	marker, found, err := b.loadMarker(scope.Interface)
	if err != nil {
		return Ownership{}, nil, err
	}
	compatibleEmpty := snapshot.clsactEmpty
	if scope.Direction == porttraffic.Outbound && preview.Request.Action == Throttle {
		compatibleEmpty = snapshot.rootReplaceable && snapshot.clsactEmpty
	}
	previous, _ := json.Marshal(tcPrevious{Interface: scope.Interface, Absent: compatibleEmpty})
	if found {
		if marker.Fingerprint != snapshot.fingerprint {
			return Ownership{Compatible: false, Owner: "changed-externally", Reason: "the Payesh tc tree changed after installation"}, previous, nil
		}
		return Ownership{Compatible: false, Owner: "payesh", Reason: "an owned policy tree already exists on this interface; reconcile or revert it first"}, previous, nil
	}
	if !compatibleEmpty {
		reason := "the interface already has ingress/egress traffic-control filters"
		if scope.Direction == porttraffic.Outbound && preview.Request.Action == Throttle {
			reason = "outbound shaping would replace an existing root qdisc"
		}
		return Ownership{Compatible: false, Owner: "foreign", Reason: reason}, previous, nil
	}
	return Ownership{Compatible: true}, previous, nil
}

func (b TCBackend) Apply(ctx context.Context, preview Preview) error {
	// Keep the kernel adapter safe even when it is called without Manager. The
	// manager performs this check before arming the rollback guard, but direct
	// module callers must not be able to replace a foreign qdisc/filter tree or
	// race an external change between Inspect and Apply.
	ownership, _, err := b.Inspect(ctx, preview)
	if err != nil {
		return err
	}
	if !ownership.Compatible {
		return fmt.Errorf("%w: %s", ErrForeignNetworkState, ownership.Reason)
	}
	commands, err := planTC(preview)
	if err != nil {
		return err
	}
	for _, args := range commands {
		result, runErr := b.runner().Run(ctx, args, nil)
		if runErr != nil {
			return tcCommandError(args, result, runErr)
		}
	}
	snapshot, err := b.snapshot(ctx, preview.Request.Scope.Interface)
	if err != nil {
		return err
	}
	return b.saveMarker(preview.Request.Scope.Interface, tcMarker{Scope: preview.Request.Scope, Fingerprint: snapshot.fingerprint})
}

func (b TCBackend) Verify(ctx context.Context, preview Preview) error {
	marker, found, err := b.loadMarker(preview.Request.Scope.Interface)
	if err != nil || !found {
		if err == nil {
			err = errors.New("tc ownership marker is missing")
		}
		return err
	}
	if marker.Scope != preview.Request.Scope {
		return errors.New("tc ownership marker scope does not match")
	}
	snapshot, err := b.snapshot(ctx, preview.Request.Scope.Interface)
	if err != nil {
		return err
	}
	if snapshot.fingerprint != marker.Fingerprint {
		return errors.New("effective tc tree does not match the installed tree")
	}
	return nil
}

func (b TCBackend) RevertOwned(ctx context.Context, checkpoint Checkpoint) error {
	var previous tcPrevious
	if err := json.Unmarshal(checkpoint.Previous, &previous); err != nil || previous.Interface != checkpoint.Scope.Interface || !previous.Absent {
		return errors.New("tc recovery checkpoint cannot be restored safely")
	}
	marker, found, err := b.loadMarker(checkpoint.Scope.Interface)
	if err != nil {
		return err
	}
	if !found {
		// A checkpoint without an ownership marker is not proof that the
		// current qdisc belongs to Payesh. In particular, this can happen if
		// marker persistence failed after kernel mutation. Refusing cleanup is
		// safer than deleting a replacement installed by another manager.
		return ErrTCMarkerMissing
	}
	snapshot, err := b.snapshot(ctx, checkpoint.Scope.Interface)
	if err != nil {
		return err
	}
	if snapshot.fingerprint != marker.Fingerprint {
		return errors.New("tc tree changed externally; refusing broad cleanup")
	}
	args := []string{"qdisc", "del", "dev", checkpoint.Scope.Interface}
	if checkpoint.Scope.Direction == porttraffic.Outbound && checkpoint.Action == Throttle {
		// qdisc deletion identifies the attachment point, not the handle. The
		// ownership fingerprint above is what makes deleting that root safe.
		args = append(args, "root")
	} else {
		args = append(args, "clsact")
	}
	result, runErr := b.runner().Run(ctx, args, nil)
	if runErr != nil {
		return tcCommandError(args, result, runErr)
	}
	if b.StateDir != "" {
		if err := os.Remove(b.markerPath(checkpoint.Scope.Interface)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func planTC(preview Preview) ([][]string, error) {
	scope := preview.Request.Scope
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if preview.Request.Action == WarnOnly {
		return nil, errors.New("warn-only policies do not install tc state")
	}
	if scope.Direction == porttraffic.Outbound && preview.Request.Action == Throttle {
		commands := [][]string{
			{"qdisc", "add", "dev", scope.Interface, "root", "handle", tcRootHandle, "htb", "default", "2"},
			{"class", "add", "dev", scope.Interface, "parent", tcRootHandle, "classid", "7a00:1", "htb", "rate", "100000000000bit", "ceil", "100000000000bit"},
			{"class", "add", "dev", scope.Interface, "parent", "7a00:1", "classid", "7a00:2", "htb", "rate", "100000000000bit", "ceil", "100000000000bit"},
			{"class", "add", "dev", scope.Interface, "parent", "7a00:1", "classid", "7a00:10", "htb", "rate", strconv.FormatUint(preview.Request.BitsPerSecond, 10) + "bit", "ceil", strconv.FormatUint(preview.Request.BitsPerSecond, 10) + "bit"},
		}
		commands = append(commands, egressClassFilters(preview)...)
		return commands, nil
	}
	hook := "ingress"
	if scope.Direction == porttraffic.Outbound {
		hook = "egress"
	}
	commands := [][]string{{"qdisc", "add", "dev", scope.Interface, "clsact"}}
	commands = append(commands, managementPassFilters(preview, hook)...)
	for familyIndex, family := range []string{"ip", "ipv6"} {
		filter := []string{"filter", "add", "dev", scope.Interface, hook, "protocol", family, "pref", strconv.Itoa(tcPrefBase + 100 + familyIndex)}
		filter = append(filter, scopeClassifier(scope)...)
		if preview.Request.Action == Block {
			filter = append(filter, "action", "drop")
		} else {
			burst := maxUint64(2048, preview.Request.BitsPerSecond/80)
			filter = append(filter, "action", "police", "rate", strconv.FormatUint(preview.Request.BitsPerSecond, 10)+"bit", "burst", strconv.FormatUint(burst, 10), "conform-exceed", "drop/pipe")
		}
		commands = append(commands, filter)
	}
	return commands, nil
}

func egressClassFilters(preview Preview) [][]string {
	commands := managementClassFilters(preview)
	for index, family := range []string{"ip", "ipv6"} {
		args := []string{"filter", "add", "dev", preview.Request.Scope.Interface, "parent", tcRootHandle, "protocol", family, "pref", strconv.Itoa(tcPrefBase + 100 + index)}
		args = append(args, scopeClassifier(preview.Request.Scope)...)
		args = append(args, "classid", "7a00:10")
		commands = append(commands, args)
	}
	return commands
}

func managementClassFilters(preview Preview) [][]string {
	commands := make([][]string, 0, len(preview.Exclusions)*2)
	for index, flow := range preview.Exclusions {
		for familyIndex, family := range []string{"ip", "ipv6"} {
			portKey := managementPortKey(flow, porttraffic.Outbound)
			args := []string{"filter", "add", "dev", preview.Request.Scope.Interface, "parent", tcRootHandle, "protocol", family, "pref", strconv.Itoa(tcPrefBase + index*2 + familyIndex), "flower", "skip_hw", "ip_proto", string(flow.Protocol), portKey, strconv.Itoa(int(flow.LocalPort)), "classid", "7a00:2"}
			commands = append(commands, args)
		}
	}
	return commands
}

func managementPassFilters(preview Preview, hook string) [][]string {
	commands := make([][]string, 0, len(preview.Exclusions)*2)
	for index, flow := range preview.Exclusions {
		for familyIndex, family := range []string{"ip", "ipv6"} {
			direction := porttraffic.Inbound
			if hook == "egress" {
				direction = porttraffic.Outbound
			}
			portKey := managementPortKey(flow, direction)
			commands = append(commands, []string{"filter", "add", "dev", preview.Request.Scope.Interface, hook, "protocol", family, "pref", strconv.Itoa(tcPrefBase + index*2 + familyIndex), "flower", "skip_hw", "ip_proto", string(flow.Protocol), portKey, strconv.Itoa(int(flow.LocalPort)), "action", "pass"})
		}
	}
	return commands
}

func managementPortKey(flow ManagementFlow, direction porttraffic.Direction) string {
	if flow.PortRole == "local-service" {
		if direction == porttraffic.Inbound {
			return "dst_port"
		}
		return "src_port"
	}
	if direction == porttraffic.Inbound {
		return "src_port"
	}
	return "dst_port"
}

func scopeFlower(scope Scope, family string) []string {
	if scope.TargetKind == "interface" {
		return []string{}
	}
	portKey := "dst_port"
	if scope.Direction == porttraffic.Outbound {
		portKey = "src_port"
	}
	return []string{"ip_proto", string(scope.Protocol), portKey, strconv.Itoa(int(scope.LocalPort))}
}

func scopeClassifier(scope Scope) []string {
	if scope.TargetKind == "interface" {
		return []string{"matchall", "skip_hw"}
	}
	return append([]string{"flower", "skip_hw"}, scopeFlower(scope, "")...)
}

type tcSnapshot struct {
	rootReplaceable bool
	clsactEmpty     bool
	fingerprint     string
}

func (b TCBackend) snapshot(ctx context.Context, iface string) (tcSnapshot, error) {
	commands := [][]string{{"-json", "qdisc", "show", "dev", iface}, {"-json", "filter", "show", "dev", iface, "ingress"}, {"-json", "filter", "show", "dev", iface, "egress"}}
	var combined []byte
	rootReplaceable, clsactEmpty := true, true
	for commandIndex, args := range commands {
		result, err := b.runner().Run(ctx, args, nil)
		if err != nil {
			return tcSnapshot{}, tcCommandError(args, result, err)
		}
		trimmed := strings.TrimSpace(string(result.Stdout))
		if trimmed != "" && trimmed != "[]" {
			var values []map[string]any
			if json.Unmarshal(result.Stdout, &values) != nil {
				return tcSnapshot{}, errors.New("invalid tc JSON output")
			}
			for _, value := range values {
				kind, _ := value["kind"].(string)
				if commandIndex == 0 {
					if kind == "clsact" || kind == "ingress" {
						clsactEmpty = false
					} else if kind != "noqueue" {
						rootReplaceable = false
					}
				} else {
					clsactEmpty = false
				}
			}
		}
		combined = append(combined, result.Stdout...)
		combined = append(combined, '\n')
	}
	digest := sha256.Sum256(combined)
	return tcSnapshot{rootReplaceable: rootReplaceable, clsactEmpty: clsactEmpty, fingerprint: hex.EncodeToString(digest[:])}, nil
}

func (b TCBackend) markerPath(iface string) string {
	return filepath.Join(b.StateDir, "tc-"+iface+".json")
}
func (b TCBackend) loadMarker(iface string) (tcMarker, bool, error) {
	if b.StateDir == "" {
		return tcMarker{}, false, nil
	}
	data, err := os.ReadFile(b.markerPath(iface))
	if os.IsNotExist(err) {
		return tcMarker{}, false, nil
	}
	if err != nil {
		return tcMarker{}, false, err
	}
	var marker tcMarker
	if len(data) > 4096 || json.Unmarshal(data, &marker) != nil {
		return tcMarker{}, false, errors.New("invalid tc ownership marker")
	}
	return marker, true, nil
}
func (b TCBackend) saveMarker(iface string, marker tcMarker) error {
	if b.StateDir == "" {
		return errors.New("tc ownership state directory is not configured")
	}
	if err := os.MkdirAll(b.StateDir, 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(marker)
	temp, err := os.CreateTemp(b.StateDir, ".tc-marker-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if temp.Chmod(0o600) != nil {
		temp.Close()
		return errors.New("restrict tc marker")
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, b.markerPath(iface))
}
func tcCommandError(args []string, result porttraffic.CommandResult, cause error) error {
	message := strings.TrimSpace(string(result.Stderr))
	if len(message) > 512 {
		message = message[:512]
	}
	return fmt.Errorf("tc %s: %s: %w", strings.Join(args, " "), message, cause)
}
func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
