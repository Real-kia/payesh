package webtls

import (
	"net"
	"os"
	"strconv"
	"testing"
)

func TestPortChangeReservesPersistsAndRejectsOccupied(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	_, m.Port, _ = net.SplitHostPort(original.Addr().String())
	var sockets []net.Listener
	defer func() {
		for _, l := range sockets {
			l.Close()
		}
	}()
	if err := m.EnablePortChanges(original.Addr().String(), func(l net.Listener) { sockets = append(sockets, l) }); err != nil {
		t.Fatal(err)
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, p, _ := net.SplitHostPort(occupied.Addr().String())
	n, _ := strconv.Atoi(p)
	before := m.DashboardPort()
	if err := m.portController.Change(n); err == nil {
		t.Fatal("occupied port accepted")
	}
	if m.DashboardPort() != before {
		t.Fatal("failed change altered active port")
	}
	occupied.Close()
	if err := m.portController.Change(n); err != nil {
		t.Fatal(err)
	}
	if m.DashboardPort() != p || len(sockets) != 1 {
		t.Fatal("port was not applied")
	}
	if _, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", p)); err == nil {
		t.Fatal("new port was not reserved")
	}
	if err := m.portController.Change(0); err == nil {
		t.Fatal("invalid port accepted")
	}
	data, err := os.ReadFile(m.path("ports.json"))
	if err != nil || len(data) == 0 {
		t.Fatal("port was not persisted", err)
	}
	sockets[0].Close()
	sockets = nil
	restored, err := NewManager(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.EnablePortChanges(original.Addr().String(), func(l net.Listener) { sockets = append(sockets, l) }); err != nil {
		t.Fatal(err)
	}
	if restored.DashboardPort() != p || len(sockets) != 1 {
		t.Fatal("port was not restored")
	}
}
