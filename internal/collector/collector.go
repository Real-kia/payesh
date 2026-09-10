// Package collector contains the SQLite-free local metric collectors used by
// payesh-agent. Persistence and API code live in the monitoring package so
// the agent build graph stays small and transport-independent.
package collector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/Real-kia/payesh/internal/contracts"
)

const (
	DefaultServerID = contracts.ServerID("server-local-0001")
	// DefaultEpoch is retained for deterministic fixtures and explicit test
	// overrides. Production constructors generate NewEpoch when omitted.
	DefaultEpoch         = contracts.CollectorEpoch("epoch-local-0001")
	maxNetworkInterfaces = 16
)

// Collector reads kernel-provided host measurements. Root is normally "/";
// tests may point it at a fixture tree containing proc-like files. The agent
// keeps only the previous CPU total, so collection is bounded in memory.
type Collector struct {
	Root                    string
	ServerID                contracts.ServerID
	Epoch                   contracts.CollectorEpoch
	AuthoritativeInterfaces []string

	sequence uint64
	previous cpuCounters
	hasPrev  bool
}

type cpuCounters struct {
	user, nice, system, idle, iowait, irq, softIRQ, steal uint64
}

func NewCollector(root string, serverID contracts.ServerID, epoch contracts.CollectorEpoch) *Collector {
	if root == "" {
		root = "/"
	}
	if serverID == "" {
		serverID = NewServerID()
	}
	if epoch == "" {
		epoch = NewEpoch()
	}
	return &Collector{Root: filepath.Clean(root), ServerID: serverID, Epoch: epoch}
}

// NewServerID returns a URL-safe installation identity. Callers that need the
// identity to survive restarts should persist it with LoadOrCreateServerID.
func NewServerID() contracts.ServerID {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return contracts.ServerID(fmt.Sprintf("server-%x-%x", time.Now().UTC().UnixNano(), uint64(os.Getpid())))
	}
	return contracts.ServerID("server-" + hex.EncodeToString(suffix[:]))
}

// LoadOrCreateServerID loads one installation identity, creating it with
// restrictive permissions on first start. O_EXCL makes concurrent starts
// converge on the same file rather than silently creating two identities.
func LoadOrCreateServerID(path string) (contracts.ServerID, error) {
	if path == "" {
		return "", errors.New("server identity path is required")
	}
	if data, err := os.ReadFile(path); err == nil {
		id := contracts.ServerID(strings.TrimSpace(string(data)))
		if !validServerID(id) {
			return "", errors.New("persisted server identity is invalid")
		}
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read server identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create server identity directory: %w", err)
	}
	id := NewServerID()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		for attempt := 0; attempt < 20; attempt++ {
			data, readErr := os.ReadFile(path)
			if readErr == nil {
				id = contracts.ServerID(strings.TrimSpace(string(data)))
				if validServerID(id) {
					return id, nil
				}
				if id != "" {
					return "", errors.New("persisted server identity is invalid")
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				return "", fmt.Errorf("read concurrently-created server identity: %w", readErr)
			}
			time.Sleep(5 * time.Millisecond)
		}
		return "", errors.New("concurrently-created server identity was not ready")
	}
	if err != nil {
		return "", fmt.Errorf("create server identity: %w", err)
	}
	if _, err := file.WriteString(string(id) + "\n"); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("write server identity: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("sync server identity: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close server identity: %w", err)
	}
	return id, nil
}

func validServerID(id contracts.ServerID) bool {
	if len(id) < 16 || len(id) > 128 {
		return false
	}
	for _, character := range string(id) {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

// NewEpoch returns a fresh collector epoch for one agent process lifetime.
// The epoch is part of the durable deduplication key, so an omitted epoch must
// never silently fall back to a process-independent constant. The random
// suffix also prevents two starts in the same nanosecond from colliding.
func NewEpoch() contracts.CollectorEpoch {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		// crypto/rand failure is exceptionally unusual; retain uniqueness within
		// the host/process rather than reusing a known epoch.
		return contracts.CollectorEpoch(fmt.Sprintf("epoch-%x-%x", time.Now().UTC().UnixNano(), uint64(os.Getpid())))
	}
	return contracts.CollectorEpoch("epoch-" + hex.EncodeToString(suffix[:]))
}

// Collect returns a node-owned sample. ReceivedAt is intentionally absent;
// the hub/storage layer stamps it only when the sample is durably accepted.
func (c *Collector) Collect(ctx context.Context, observedAt time.Time) (contracts.NodeMetricSample, error) {
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	if err := ctx.Err(); err != nil {
		return contracts.NodeMetricSample{}, err
	}
	values := make(map[string]float64)
	counters := make(map[string]string)
	units := make(map[string]string)
	validity := make(map[string]string)

	if raw, err := readRootFile(ctx, c.Root, "proc/stat"); err == nil {
		cpu, parseErr := parseCPUStat(raw)
		if parseErr == nil {
			if c.hasPrev {
				previousTotal, previousOK := c.previous.total()
				currentTotal, currentOK := cpu.total()
				previousIdle, previousIdleOK := addUint64(c.previous.idle, c.previous.iowait)
				currentIdle, currentIdleOK := addUint64(cpu.idle, cpu.iowait)
				if previousOK && currentOK && previousIdleOK && currentIdleOK && cpuCountersMonotonic(c.previous, cpu) {
					totalDelta := currentTotal - previousTotal
					idleDelta := currentIdle - previousIdle
					if totalDelta == 0 || idleDelta > totalDelta {
						setCPUValidity(validity, "uncertain")
					} else {
						values["cpu.utilization"] = 100 * float64(totalDelta-idleDelta) / float64(totalDelta)
						values["cpu.iowait"] = 100 * float64(cpu.iowait-c.previous.iowait) / float64(totalDelta)
						values["cpu.steal"] = 100 * float64(cpu.steal-c.previous.steal) / float64(totalDelta)
						units["cpu.utilization"], units["cpu.iowait"], units["cpu.steal"] = "percent", "percent", "percent"
						validity["cpu.utilization"], validity["cpu.iowait"], validity["cpu.steal"] = "valid", "valid", "valid"
					}
				} else {
					setCPUValidity(validity, "uncertain")
				}
			} else {
				setCPUValidity(validity, "unavailable")
			}
			c.previous, c.hasPrev = cpu, true
		} else {
			setCPUValidity(validity, "unavailable")
		}
	} else {
		setCPUValidity(validity, "unavailable")
	}

	if raw, err := readRootFile(ctx, c.Root, "proc/meminfo"); err == nil {
		collectMemory(raw, counters, values, units, validity)
	} else {
		for _, metric := range []string{"memory.total_bytes", "memory.available_bytes", "memory.used_bytes", "memory.used_percent", "memory.swap_total_bytes", "memory.swap_used_bytes"} {
			validity[metric] = "unavailable"
		}
	}
	if raw, err := readRootFile(ctx, c.Root, "proc/loadavg"); err == nil {
		collectLoad(raw, values, units, validity)
	} else {
		for _, metric := range []string{"load.1m", "load.5m", "load.15m"} {
			validity[metric] = "unavailable"
		}
	}
	if raw, err := readRootFile(ctx, c.Root, "proc/uptime"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) > 0 {
			if uptime, err := strconv.ParseFloat(fields[0], 64); err == nil && uptime >= 0 && math.IsInf(uptime, 0) == false {
				values["uptime.seconds"] = uptime
				units["uptime.seconds"] = "seconds"
				validity["uptime.seconds"] = "valid"
			}
		}
		if validity["uptime.seconds"] == "" {
			validity["uptime.seconds"] = "unavailable"
		}
	} else {
		validity["uptime.seconds"] = "unavailable"
	}
	if raw, err := readRootFile(ctx, c.Root, "proc/net/dev"); err == nil {
		collectNetworkWithRootAndSelection(c.Root, raw, counters, units, validity, c.AuthoritativeInterfaces)
	} else {
		validity["net.billing.rx_bytes"], validity["net.billing.tx_bytes"] = "unavailable", "unavailable"
	}
	if raw, err := readRootFile(ctx, c.Root, "proc/diskstats"); err == nil {
		collectDiskIO(raw, counters, units, validity)
	} else {
		for _, metric := range []string{"disk.io.read_bytes", "disk.io.write_bytes", "disk.io.read_ops", "disk.io.write_ops", "disk.io.read_time_ms", "disk.io.write_time_ms"} {
			validity[metric] = "unavailable"
		}
	}
	collectDisk(c.Root, counters, values, units, validity)

	sample := contracts.NodeMetricSample{
		ServerID: c.ServerID, CollectorEpoch: c.Epoch, Sequence: c.sequence,
		ObservedAt: observedAt.UTC(), Values: values, Counters: counters,
		Units: units, Validity: validity,
	}
	c.sequence++
	if err := sample.Validate(); err != nil {
		return contracts.NodeMetricSample{}, err
	}
	return sample, nil
}

func setCPUValidity(validity map[string]string, state string) {
	for _, metric := range []string{"cpu.utilization", "cpu.iowait", "cpu.steal"} {
		validity[metric] = state
	}
}

// cpuCountersMonotonic rejects a partial kernel counter reset as well as an
// aggregate reset.  Checking only total/idle can hide a user/system/IRQ
// decrease when another component grows enough to keep the aggregate total
// increasing, which would manufacture a false utilization delta.
func cpuCountersMonotonic(previous, current cpuCounters) bool {
	return current.user >= previous.user &&
		current.nice >= previous.nice &&
		current.system >= previous.system &&
		current.idle >= previous.idle &&
		current.iowait >= previous.iowait &&
		current.irq >= previous.irq &&
		current.softIRQ >= previous.softIRQ &&
		current.steal >= previous.steal
}

func readRootFile(ctx context.Context, root, relative string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func parseCPUStat(data []byte) (cpuCounters, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 || fields[0] != "cpu" {
			continue
		}
		values := make([]uint64, 8)
		for i := range values {
			parsed, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return cpuCounters{}, fmt.Errorf("parse cpu counter %q: %w", fields[i+1], err)
			}
			values[i] = parsed
		}
		return cpuCounters{user: values[0], nice: values[1], system: values[2], idle: values[3], iowait: values[4], irq: values[5], softIRQ: values[6], steal: values[7]}, nil
	}
	return cpuCounters{}, errors.New("aggregate cpu line not found")
}

func (c cpuCounters) total() (uint64, bool) {
	total := uint64(0)
	for _, value := range []uint64{c.user, c.nice, c.system, c.idle, c.iowait, c.irq, c.softIRQ, c.steal} {
		var ok bool
		total, ok = addUint64(total, value)
		if !ok {
			return 0, false
		}
	}
	return total, true
}

func collectMemory(data []byte, counters map[string]string, values map[string]float64, units map[string]string, validity map[string]string) {
	fields := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		value, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			continue
		}
		if len(parts) > 2 && parts[2] == "kB" {
			if value > ^uint64(0)/1024 {
				continue
			}
			value *= 1024
		}
		fields[strings.TrimSuffix(parts[0], ":")] = value
	}
	total, totalOK := fields["MemTotal"]
	available, availableOK := fields["MemAvailable"]
	if totalOK {
		counters["memory.total_bytes"] = strconv.FormatUint(total, 10)
		units["memory.total_bytes"] = "bytes"
		validity["memory.total_bytes"] = "valid"
	} else {
		validity["memory.total_bytes"] = "unavailable"
	}
	if totalOK && availableOK && total >= available && total > 0 {
		counters["memory.available_bytes"] = strconv.FormatUint(available, 10)
		counters["memory.used_bytes"] = strconv.FormatUint(total-available, 10)
		values["memory.used_percent"] = 100 * float64(total-available) / float64(total)
		units["memory.available_bytes"], units["memory.used_bytes"] = "bytes", "bytes"
		units["memory.used_percent"] = "percent"
		validity["memory.available_bytes"], validity["memory.used_bytes"], validity["memory.used_percent"] = "valid", "valid", "valid"
	} else {
		for _, metric := range []string{"memory.available_bytes", "memory.used_bytes", "memory.used_percent"} {
			validity[metric] = "unavailable"
		}
	}
	if swapTotal, ok := fields["SwapTotal"]; ok {
		free, freeOK := fields["SwapFree"]
		if freeOK && swapTotal >= free {
			counters["memory.swap_total_bytes"] = strconv.FormatUint(swapTotal, 10)
			counters["memory.swap_used_bytes"] = strconv.FormatUint(swapTotal-free, 10)
			units["memory.swap_total_bytes"], units["memory.swap_used_bytes"] = "bytes", "bytes"
			validity["memory.swap_total_bytes"], validity["memory.swap_used_bytes"] = "valid", "valid"
		} else {
			validity["memory.swap_total_bytes"], validity["memory.swap_used_bytes"] = "unavailable", "unavailable"
		}
	} else {
		validity["memory.swap_total_bytes"], validity["memory.swap_used_bytes"] = "unavailable", "unavailable"
	}
}

func collectLoad(data []byte, values map[string]float64, units map[string]string, validity map[string]string) {
	fields := strings.Fields(string(data))
	for i, name := range []string{"load.1m", "load.5m", "load.15m"} {
		validity[name] = "unavailable"
		if i >= len(fields) {
			continue
		}
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil || value < 0 || math.IsInf(value, 0) || math.IsNaN(value) {
			validity[name] = "unavailable"
			continue
		}
		values[name], units[name], validity[name] = value, "count", "valid"
	}
}

func collectNetwork(data []byte, counters map[string]string, units map[string]string, validity map[string]string) {
	collectNetworkWithRoot("", data, counters, units, validity)
}

func collectNetworkWithRoot(root string, data []byte, counters map[string]string, units map[string]string, validity map[string]string) {
	collectNetworkWithRootAndSelection(root, data, counters, units, validity, nil)
}

func collectNetworkWithRootAndSelection(root string, data []byte, counters map[string]string, units map[string]string, validity map[string]string, authoritative []string) {
	selected := make(map[string]struct{}, len(authoritative))
	for _, name := range authoritative {
		name = strings.TrimSpace(name)
		if name != "" {
			selected[name] = struct{}{}
		}
	}
	var billingRX, billingTX uint64
	externalFound, billingOverflow := false, false
	interfaces := 0
	for _, line := range strings.Split(string(data), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		fields := strings.Fields(line[colon+1:])
		if name == "" || len(fields) < 12 {
			continue
		}
		values := make([]uint64, 8)
		valid := true
		for i, index := range []int{0, 1, 2, 3, 8, 9, 10, 11} {
			parsed, err := strconv.ParseUint(fields[index], 10, 64)
			if err != nil {
				valid = false
				break
			}
			values[i] = parsed
		}
		if !valid {
			continue
		}
		// Billing is a host-level baseline and must scan every interface, even
		// when the per-interface display is capped. Otherwise a host with many
		// veth/bridge devices can hide its real external interface after the
		// first sixteen entries.
		_, explicitlySelected := selected[name]
		if (len(selected) > 0 && explicitlySelected) || (len(selected) == 0 && !isVirtualInterfaceWithRoot(root, name)) {
			externalFound = true
			var ok bool
			billingRX, ok = addUint64(billingRX, values[0])
			if !ok {
				billingOverflow = true
			}
			billingTX, ok = addUint64(billingTX, values[4])
			if !ok {
				billingOverflow = true
			}
		}
		if interfaces >= maxNetworkInterfaces && !explicitlySelected {
			continue
		}
		interfaces++
		key := sanitizeName(name)
		for suffix, value := range map[string]uint64{"rx_bytes": values[0], "rx_packets": values[1], "rx_errors": values[2], "rx_drops": values[3], "tx_bytes": values[4], "tx_packets": values[5], "tx_errors": values[6], "tx_drops": values[7]} {
			metric := "net." + key + "." + suffix
			unit := "count"
			if strings.HasSuffix(suffix, "_bytes") {
				unit = "bytes"
			}
			counters[metric], units[metric], validity[metric] = strconv.FormatUint(value, 10), unit, "valid"
		}
	}
	if externalFound && !billingOverflow {
		counters["net.billing.rx_bytes"] = strconv.FormatUint(billingRX, 10)
		counters["net.billing.tx_bytes"] = strconv.FormatUint(billingTX, 10)
		units["net.billing.rx_bytes"], units["net.billing.tx_bytes"] = "bytes", "bytes"
		validity["net.billing.rx_bytes"], validity["net.billing.tx_bytes"] = "valid", "valid"
	} else {
		validity["net.billing.rx_bytes"], validity["net.billing.tx_bytes"] = "unavailable", "unavailable"
	}
}

func collectDiskIO(data []byte, counters map[string]string, units map[string]string, validity map[string]string) {
	var reads, readSectors, readMillis, writes, writeSectors, writeMillis uint64
	found, overflow := false, false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 11 || isPartitionDevice(fields[2]) {
			continue
		}
		values := make([]uint64, 6)
		for i, index := range []int{3, 5, 6, 7, 9, 10} {
			parsed, err := strconv.ParseUint(fields[index], 10, 64)
			if err != nil {
				values = nil
				break
			}
			values[i] = parsed
		}
		if values == nil {
			continue
		}
		if _, readOK := multiplyUint64(values[1], 512); !readOK {
			overflow = true
			break
		}
		if _, writeOK := multiplyUint64(values[4], 512); !writeOK {
			overflow = true
			break
		}
		nextReads, ok := addUint64(reads, values[0])
		if !ok {
			overflow = true
			break
		}
		nextReadSectors, ok := addUint64(readSectors, values[1])
		if !ok {
			overflow = true
			break
		}
		nextReadMillis, ok := addUint64(readMillis, values[2])
		if !ok {
			overflow = true
			break
		}
		nextWrites, ok := addUint64(writes, values[3])
		if !ok {
			overflow = true
			break
		}
		nextWriteSectors, ok := addUint64(writeSectors, values[4])
		if !ok {
			overflow = true
			break
		}
		nextWriteMillis, ok := addUint64(writeMillis, values[5])
		if !ok {
			overflow = true
			break
		}
		reads, readSectors, readMillis = nextReads, nextReadSectors, nextReadMillis
		writes, writeSectors, writeMillis = nextWrites, nextWriteSectors, nextWriteMillis
		found = true
	}
	if !found || overflow {
		for _, metric := range []string{"disk.io.read_bytes", "disk.io.write_bytes", "disk.io.read_ops", "disk.io.write_ops", "disk.io.read_time_ms", "disk.io.write_time_ms"} {
			validity[metric] = "unavailable"
		}
		return
	}
	readBytes, readOK := multiplyUint64(readSectors, 512)
	writeBytes, writeOK := multiplyUint64(writeSectors, 512)
	if !readOK || !writeOK {
		validity["disk.io.read_bytes"], validity["disk.io.write_bytes"] = "unavailable", "unavailable"
		return
	}
	for name, value := range map[string]uint64{"disk.io.read_bytes": readBytes, "disk.io.write_bytes": writeBytes, "disk.io.read_ops": reads, "disk.io.write_ops": writes, "disk.io.read_time_ms": readMillis, "disk.io.write_time_ms": writeMillis} {
		counters[name], validity[name] = strconv.FormatUint(value, 10), "valid"
	}
	units["disk.io.read_bytes"], units["disk.io.write_bytes"] = "bytes", "bytes"
	units["disk.io.read_ops"], units["disk.io.write_ops"] = "count", "count"
	units["disk.io.read_time_ms"], units["disk.io.write_time_ms"] = "milliseconds", "milliseconds"
}

func isPartitionDevice(name string) bool {
	if name == "" {
		return true
	}
	for _, prefix := range []string{"loop", "ram", "dm-", "md"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	if strings.HasPrefix(name, "nvme") || strings.HasPrefix(name, "mmcblk") {
		if index := strings.LastIndexByte(name, 'p'); index >= 0 && index+1 < len(name) {
			allDigits := true
			for _, character := range name[index+1:] {
				if character < '0' || character > '9' {
					allDigits = false
					break
				}
			}
			return allDigits
		}
	}
	for _, prefix := range []string{"sd", "vd", "xvd", "hd"} {
		if strings.HasPrefix(name, prefix) && name[len(prefix):] != "" {
			last := name[len(name)-1]
			return last >= '0' && last <= '9'
		}
	}
	return false
}

func multiplyUint64(left, right uint64) (uint64, bool) {
	if right != 0 && left > ^uint64(0)/right {
		return 0, false
	}
	return left * right, true
}

func addUint64(left, right uint64) (uint64, bool) {
	if ^uint64(0)-left < right {
		return 0, false
	}
	return left + right, true
}

func collectDisk(root string, counters map[string]string, values map[string]float64, units map[string]string, validity map[string]string) {
	markUnavailable := func() {
		for _, metric := range []string{"disk.root.total_bytes", "disk.root.free_bytes", "disk.root.used_bytes", "disk.root.used_percent", "disk.root.inodes_total", "disk.root.inodes_free", "disk.root.inodes_used_percent"} {
			validity[metric] = "unavailable"
		}
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		markUnavailable()
		return
	}
	blockSize := uint64(stat.Bsize)
	if blockSize == 0 {
		markUnavailable()
		return
	}
	if uint64(stat.Blocks) > ^uint64(0)/blockSize || uint64(stat.Bavail) > ^uint64(0)/blockSize {
		markUnavailable()
		return
	}
	total := uint64(stat.Blocks) * blockSize
	free := uint64(stat.Bavail) * blockSize
	for name, value := range map[string]uint64{"disk.root.total_bytes": total, "disk.root.free_bytes": free, "disk.root.inodes_total": uint64(stat.Files), "disk.root.inodes_free": uint64(stat.Ffree)} {
		counters[name], validity[name] = strconv.FormatUint(value, 10), "valid"
	}
	units["disk.root.total_bytes"], units["disk.root.free_bytes"] = "bytes", "bytes"
	units["disk.root.inodes_total"], units["disk.root.inodes_free"] = "count", "count"
	if total >= free {
		counters["disk.root.used_bytes"] = strconv.FormatUint(total-free, 10)
		units["disk.root.used_bytes"], validity["disk.root.used_bytes"] = "bytes", "valid"
		if total > 0 {
			values["disk.root.used_percent"] = 100 * float64(total-free) / float64(total)
			units["disk.root.used_percent"], validity["disk.root.used_percent"] = "percent", "valid"
		}
	} else {
		validity["disk.root.used_bytes"] = "unavailable"
	}
	if stat.Files > 0 && stat.Ffree <= stat.Files {
		values["disk.root.inodes_used_percent"] = 100 * float64(stat.Files-stat.Ffree) / float64(stat.Files)
		units["disk.root.inodes_used_percent"], validity["disk.root.inodes_used_percent"] = "percent", "valid"
	}
}

func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func isVirtualInterface(name string) bool {
	return isVirtualInterfaceWithRoot("", name)
}

func isVirtualInterfaceWithRoot(root, name string) bool {
	if name == "" || name == "lo" || strings.ContainsAny(name, ".@") {
		return true
	}
	for _, prefix := range []string{"br", "bridge", "bond", "docker", "veth", "virbr", "tun", "tap", "ppp", "wg", "cni", "vlan", "ifb", "dummy", "macvlan", "ipvlan", "flannel", "cali", "tailscale", "zt", "sit", "gre", "gretap", "erspan", "ip6tnl", "vxlan", "geneve"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	// On Linux, a physical interface exposes a kernel device link. A netdev
	// without one is a logical interface even when its name is unfamiliar.
	if root != "" && filepath.Base(name) == name {
		netPath := filepath.Join(root, "sys", "class", "net", name)
		if _, err := os.Stat(netPath); err == nil {
			if _, err := os.Stat(filepath.Join(netPath, "device")); errors.Is(err, os.ErrNotExist) {
				return true
			}
		}
	}
	return false
}
