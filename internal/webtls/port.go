package webtls

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
)

// PortController reserves sockets before persisting and publishing a change.
// Original sockets remain available for enrolled nodes across restarts.
type PortController struct {
	mu      sync.Mutex
	manager *Manager
	host    string
	ports   []string
	start   func(net.Listener)
}

func (m *Manager) DashboardPort() string { m.mu.Lock(); defer m.mu.Unlock(); return m.Port }

func (m *Manager) EnablePortChanges(address string, start func(net.Listener)) error {
	host, original, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	c := &PortController{manager: m, host: host, ports: []string{original}, start: start}
	data, err := os.ReadFile(m.path("ports.json"))
	if err == nil {
		var saved []string
		if err = json.Unmarshal(data, &saved); err != nil {
			return err
		}
		for _, port := range saved {
			if err := validPort(port); err != nil {
				return err
			}
			if port != original {
				l, err := net.Listen("tcp", net.JoinHostPort(host, port))
				if err != nil {
					return fmt.Errorf("restore dashboard port %s: %w", port, err)
				}
				c.ports = append(c.ports, port)
				start(l)
			}
		}
		if len(saved) > 0 {
			m.mu.Lock()
			m.Port = saved[len(saved)-1]
			m.mu.Unlock()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.portController = c
	return nil
}
func validPort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}
func (c *PortController) Change(port int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := strconv.Itoa(port)
	if err := validPort(p); err != nil {
		return err
	}
	if p == c.manager.DashboardPort() {
		return nil
	}
	var listener net.Listener
	exists := false
	for _, old := range c.ports {
		if old == p {
			exists = true
		}
	}
	if !exists {
		if len(c.ports) >= 16 {
			return errors.New("dashboard port history is full")
		}
		var err error
		listener, err = net.Listen("tcp", net.JoinHostPort(c.host, p))
		if err != nil {
			return fmt.Errorf("port %d is unavailable: %w", port, err)
		}
	}
	next := make([]string, 0, len(c.ports)+1)
	for _, old := range c.ports {
		if old != p {
			next = append(next, old)
		}
	}
	next = append(next, p)
	data, _ := json.Marshal(next)
	if err := os.MkdirAll(c.manager.Dir, 0700); err != nil {
		if listener != nil {
			listener.Close()
		}
		return err
	}
	tmp := c.manager.path("ports.json.tmp")
	err := os.WriteFile(tmp, data, 0600)
	if err == nil {
		err = os.Rename(tmp, c.manager.path("ports.json"))
	}
	if err != nil {
		if listener != nil {
			listener.Close()
		}
		return err
	}
	c.ports = next
	if listener != nil {
		c.start(listener)
	}
	c.manager.mu.Lock()
	c.manager.Port = p
	c.manager.mu.Unlock()
	return nil
}
