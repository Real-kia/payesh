package modules

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

const (
	maxManifestAge    = 30 * 24 * time.Hour
	maxManifestFuture = 5 * time.Minute
)

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
var protocolPattern = regexp.MustCompile(`^v([1-9][0-9]*)$`)

func validateManifestPolicy(manifest contracts.ModuleManifest, entry contracts.ModuleCatalogEntry, server contracts.Server, now time.Time) error {
	if manifest.ModuleVersion != entry.LatestVersion {
		return fmt.Errorf("modules: release %s is not the catalog-approved version %s", manifest.ModuleVersion, entry.LatestVersion)
	}
	core, err := parseSemver(server.Version)
	if err != nil {
		return fmt.Errorf("modules: server core version is invalid: %w", err)
	}
	minimum, err := parseSemver(manifest.MinCore)
	if err != nil {
		return fmt.Errorf("modules: min_core is invalid: %w", err)
	}
	if compareVersion(core, minimum) < 0 {
		return errors.New("modules: server core version is below min_core")
	}
	if manifest.MaxCore != "" {
		maximum, err := parseSemver(manifest.MaxCore)
		if err != nil {
			return fmt.Errorf("modules: max_core is invalid: %w", err)
		}
		if compareVersion(minimum, maximum) > 0 || compareVersion(core, maximum) > 0 {
			return errors.New("modules: server core version is outside the manifest range")
		}
	}
	protocol, err := parseProtocol(contracts.ProtocolVersion)
	if err != nil {
		return err
	}
	protocolMin, err := parseProtocol(manifest.ProtocolMin)
	if err != nil {
		return fmt.Errorf("modules: protocol_min is invalid: %w", err)
	}
	protocolMax, err := parseProtocol(manifest.ProtocolMax)
	if err != nil {
		return fmt.Errorf("modules: protocol_max is invalid: %w", err)
	}
	if protocolMin > protocolMax || protocol < protocolMin || protocol > protocolMax {
		return errors.New("modules: core protocol is outside the manifest range")
	}
	if manifest.CreatedAt.After(now.Add(maxManifestFuture)) || now.Sub(manifest.CreatedAt) > maxManifestAge {
		return errors.New("modules: manifest is stale or dated too far in the future")
	}
	if manifest.SHA256 != strings.ToLower(manifest.SHA256) {
		return errors.New("modules: sha256 must be lowercase hexadecimal")
	}
	decoded, err := hex.DecodeString(manifest.SHA256)
	if err != nil || len(decoded) != 32 {
		return errors.New("modules: sha256 is invalid")
	}
	if missing := missingInventory(entry.Dependencies, manifest.Dependencies); missing != "" {
		return fmt.Errorf("modules: signed manifest omits catalog dependency %q", missing)
	}
	if missing := missingInventory(entry.RequiredPrivileges, manifest.RequiredPrivileges); missing != "" {
		return fmt.Errorf("modules: signed manifest omits catalog privilege %q", missing)
	}
	return nil
}

func missingInventory(required, supplied []string) string {
	have := make(map[string]struct{}, len(supplied))
	for _, item := range supplied {
		have[item] = struct{}{}
	}
	for _, item := range required {
		if _, ok := have[item]; !ok {
			return item
		}
	}
	return ""
}

func parseSemver(value string) ([3]uint64, error) {
	match := semverPattern.FindStringSubmatch(value)
	if match == nil {
		return [3]uint64{}, fmt.Errorf("%q is not semantic versioning", value)
	}
	var parsed [3]uint64
	for index := range parsed {
		n, err := strconv.ParseUint(match[index+1], 10, 64)
		if err != nil {
			return [3]uint64{}, err
		}
		parsed[index] = n
	}
	return parsed, nil
}

func compareVersion(a, b [3]uint64) int {
	for index := range a {
		if a[index] < b[index] {
			return -1
		}
		if a[index] > b[index] {
			return 1
		}
	}
	return 0
}

func parseProtocol(value string) (uint64, error) {
	match := protocolPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, fmt.Errorf("protocol %q is invalid", value)
	}
	return strconv.ParseUint(match[1], 10, 64)
}
