package monitoring

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// MaxNodeAddresses bounds what one node may report.
const MaxNodeAddresses = 32

// NormalizeNodeAddresses keeps valid unicast addresses, without loopback or
// link-local ones, public addresses first.
func NormalizeNodeAddresses(addresses []string) []string {
	seen := map[netip.Addr]bool{}
	var public, private []netip.Addr
	for _, raw := range addresses {
		addr, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		addr = addr.Unmap().WithZone("")
		if seen[addr] || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
			continue
		}
		seen[addr] = true
		if addr.IsPrivate() {
			private = append(private, addr)
		} else {
			public = append(public, addr)
		}
	}
	sortAddrs := func(list []netip.Addr) {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Is4() != list[j].Is4() {
				return list[i].Is4()
			}
			return list[i].Less(list[j])
		})
	}
	sortAddrs(public)
	sortAddrs(private)
	out := make([]string, 0, len(public)+len(private))
	for _, addr := range append(public, private...) {
		if len(out) == MaxNodeAddresses {
			break
		}
		out = append(out, addr.String())
	}
	return out
}

// RecordServerAddresses stores the addresses a node reports. It only writes
// when they changed.
func (s *Store) RecordServerAddresses(ctx context.Context, serverID contracts.ServerID, addresses []string) error {
	if !validStoreServerID(serverID) {
		return errors.New("a valid server id is required")
	}
	joined := strings.Join(NormalizeNodeAddresses(addresses), "\x1f")
	now := FormatPersistedTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `INSERT INTO server_locations(server_id,addresses,updated_at) SELECT id,?,? FROM servers WHERE id=?
ON CONFLICT(server_id) DO UPDATE SET addresses=excluded.addresses WHERE server_locations.addresses<>excluded.addresses`, joined, now, string(serverID))
	return err
}

// serverAddresses merges the configured address, the connection source and
// the reported addresses, in that order, without duplicates.
func serverAddresses(configured, peerIP, reported string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		key := value
		if addr, err := netip.ParseAddr(value); err == nil {
			key = addr.Unmap().String()
			value = key
		}
		if value == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, value)
	}
	add(configured)
	add(peerIP)
	if reported != "" {
		for _, value := range strings.Split(reported, "\x1f") {
			add(value)
		}
	}
	return out
}
