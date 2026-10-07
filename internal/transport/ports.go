package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const DefaultNodePort = "9797"
const MigrationCapability = "transport-migration"

type endpointObservation struct {
	Port  string `json:"port"`
	Error string `json:"error,omitempty"`
}
type portState struct {
	Ports           []string                                   `json:"ports"`
	LegacyDashboard bool                                       `json:"legacy_dashboard"`
	Nodes           map[contracts.ServerID]endpointObservation `json:"nodes"`
}
type NodePortStatus struct {
	Port            string                `json:"port"`
	URL             string                `json:"url,omitempty"`
	PreviousPorts   []string              `json:"previous_ports"`
	LegacyDashboard bool                  `json:"legacy_dashboard"`
	Nodes           []NodeMigrationStatus `json:"nodes"`
	Pending         int                   `json:"pending"`
	Total           int                   `json:"total"`
	Migrated        int                   `json:"migrated"`
	NextCursor      string                `json:"next_cursor,omitempty"`
}
type NodeMigrationStatus struct {
	ID    contracts.ServerID `json:"id"`
	Name  string             `json:"name"`
	State string             `json:"state"`
	Error string             `json:"error,omitempty"`
}

// NodePorts owns TLS-only listeners. Historical endpoints stay available until
// an operator retires them after every enrolled node confirms the new endpoint.
type NodePorts struct {
	syncFile   func(*os.File) error
	mu         sync.Mutex
	path, host string
	state      portState
	listeners  map[string]net.Listener
	store      *monitoring.Store
	publicURL  func(string) string
	start      func(net.Listener)
}

func NewNodePorts(path, address string, store *monitoring.Store, publicURL func(string) string, start func(net.Listener)) (*NodePorts, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	p := &NodePorts{path: path, host: host, store: store, publicURL: publicURL, start: start, listeners: map[string]net.Listener{}, state: portState{Ports: []string{port}, LegacyDashboard: false, Nodes: map[contracts.ServerID]endpointObservation{}}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &p.state); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		status, statusErr := p.statusLocked(context.Background(), 0, "")
		if statusErr != nil {
			return nil, statusErr
		}
		p.state.LegacyDashboard = status.Pending > 0
	}
	if len(p.state.Ports) == 0 || len(p.state.Ports) > 16 {
		return nil, errors.New("invalid node port history")
	}
	if p.state.Nodes == nil {
		p.state.Nodes = map[contracts.ServerID]endpointObservation{}
	}
	for _, port := range p.state.Ports {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			p.Close()
			return nil, errors.New("invalid node port")
		}
		if _, exists := p.listeners[port]; exists {
			p.Close()
			return nil, errors.New("duplicate node port")
		}
		l, e := net.Listen("tcp", net.JoinHostPort(host, port))
		if e != nil {
			p.Close()
			return nil, fmt.Errorf("node port %s: %w", port, e)
		}
		p.listeners[port] = l
	}
	if err = p.saveLocked(); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}
func (p *NodePorts) Start() {
	for _, l := range p.listeners {
		p.start(l)
	}
}
func (p *NodePorts) saveLocked() error {
	data, err := json.Marshal(p.state)
	if err != nil {
		return err
	}
	if err = makeTransportDirectory(filepath.Dir(p.path), p.syncFile); err != nil {
		return err
	}
	return persistTransportFile(p.path, data, p.syncFile)
}
func (p *NodePorts) currentLocked() string { return p.state.Ports[len(p.state.Ports)-1] }
func (p *NodePorts) URL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publicURL(p.currentLocked())
}
func (p *NodePorts) LegacyDashboard() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.LegacyDashboard
}
func (p *NodePorts) Close() {
	for _, l := range p.listeners {
		_ = l.Close()
	}
}
func (p *NodePorts) Observe(id contracts.ServerID, port string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.state.Nodes[id]
	if old.Port == port && old.Error == "" {
		return nil
	}
	p.state.Nodes[id] = endpointObservation{Port: port}
	if err := p.saveLocked(); err != nil {
		p.state.Nodes[id] = old
		return err
	}
	return nil
}
func (p *NodePorts) ReportFailure(id contracts.ServerID, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(message) > 500 {
		message = message[:500]
	}
	old := p.state.Nodes[id]
	record := old
	record.Error = message
	p.state.Nodes[id] = record
	if p.saveLocked() != nil {
		p.state.Nodes[id] = old
	}
}
func (p *NodePorts) Change(port int) error {
	if port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	next := strconv.Itoa(port)
	if next == p.currentLocked() {
		return nil
	}
	if p.publicURL(next) == "" {
		return errors.New("enable HTTPS or configure a public node transport URL first")
	}
	l, exists := p.listeners[next]
	if !exists {
		if len(p.state.Ports) >= 16 {
			return errors.New("retire previous node ports before adding another")
		}
		var err error
		l, err = net.Listen("tcp", net.JoinHostPort(p.host, next))
		if err != nil {
			return fmt.Errorf("node port unavailable: %w", err)
		}
	}
	old := p.state.Ports
	ports := []string{}
	for _, v := range old {
		if v != next {
			ports = append(ports, v)
		}
	}
	p.state.Ports = append(ports, next)
	if err := p.saveLocked(); err != nil {
		p.state.Ports = old
		if !exists {
			_ = l.Close()
		}
		return err
	}
	if !exists {
		p.listeners[next] = l
		p.start(l)
	}
	return nil
}

var errNodeStatusCursor = errors.New("invalid node migration cursor")

func (p *NodePorts) statusLocked(ctx context.Context, limit int, after string) (NodePortStatus, error) {
	status := NodePortStatus{Port: p.currentLocked(), URL: p.publicURL(p.currentLocked()), PreviousPorts: append([]string{}, p.state.Ports[:len(p.state.Ports)-1]...), LegacyDashboard: p.state.LegacyDashboard, Nodes: []NodeMigrationStatus{}}
	cursor := ""
	collect := after == ""
	var lastID contracts.ServerID
	for {
		page, err := p.store.QueryServerPage(ctx, 1000, cursor)
		if err != nil {
			return status, err
		}
		for _, node := range page.Items {
			if node.Role != "node" || node.ConnectionState == "revoked" {
				continue
			}
			record := p.state.Nodes[node.ID]
			state := "pending"
			if record.Port == status.Port {
				state = "migrated"
			} else {
				supported := false
				for _, cap := range node.Capabilities {
					if cap == MigrationCapability {
						supported = true
					}
				}
				if !supported {
					state = "update-required"
				} else if record.Error != "" {
					state = "failed"
				}
				status.Pending++
			}
			status.Total++
			if state == "migrated" {
				status.Migrated++
			}
			if !collect {
				if string(node.ID) == after {
					collect = true
				}
				continue
			}
			if limit > 0 && len(status.Nodes) < limit {
				status.Nodes = append(status.Nodes, NodeMigrationStatus{ID: node.ID, Name: node.Name, State: state, Error: record.Error})
				lastID = node.ID
			} else if limit > 0 && status.NextCursor == "" {
				status.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(lastID))
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !collect {
		return status, errNodeStatusCursor
	}
	return status, nil
}
func (p *NodePorts) Status(ctx context.Context) (NodePortStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statusLocked(ctx, 200, "")
}

// StatusPage bounds per-node details while retaining totals for the whole fleet.
func (p *NodePorts) StatusPage(ctx context.Context, limit int, cursor string) (NodePortStatus, error) {
	if limit < 1 || limit > 200 || len(cursor) > 256 {
		return NodePortStatus{}, errNodeStatusCursor
	}
	after := ""
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(b) == 0 || len(b) > 128 {
			return NodePortStatus{}, errNodeStatusCursor
		}
		after = string(b)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statusLocked(ctx, limit, after)
}
func (p *NodePorts) Retire(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	status, err := p.statusLocked(ctx, 0, "")
	if err != nil {
		return err
	}
	if status.Pending > 0 {
		return errors.New("previous endpoints are still needed by pending nodes")
	}
	old := p.state
	p.state.Ports = []string{p.currentLocked()}
	p.state.LegacyDashboard = false
	if err = p.saveLocked(); err != nil {
		p.state = old
		return err
	}
	for port, l := range p.listeners {
		if port != status.Port {
			_ = l.Close()
			delete(p.listeners, port)
		}
	}
	return nil
}

// Handler must be wrapped in the browser authentication, permission and CSRF middleware.
func (p *NodePorts) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		limit, cursor := 200, ""
		if r.Method == http.MethodGet {
			cursor = r.URL.Query().Get("cursor")
			if raw := r.URL.Query().Get("limit"); raw != "" {
				limit, err = strconv.Atoi(raw)
				if err != nil || limit < 1 || limit > 200 {
					http.Error(w, "limit must be between 1 and 200", 400)
					return
				}
			}
		}
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			var body struct {
				Port int `json:"port"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&body) != nil {
				http.Error(w, "invalid port request", 400)
				return
			}
			var trailing any
			if decoder.Decode(&trailing) != io.EOF {
				http.Error(w, "invalid port request", 400)
				return
			}
			err = p.Change(body.Port)
		case http.MethodDelete:
			err = p.Retire(r.Context())
		default:
			w.Header().Set("Allow", "GET, PATCH, DELETE")
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "node_port_unavailable", "message": err.Error(), "retryable": false})
			return
		}
		status, err := p.StatusPage(r.Context(), limit, cursor)
		if errors.Is(err, errNodeStatusCursor) {
			http.Error(w, err.Error(), 400)
			return
		}
		if err != nil {
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "could not read node migration status"})
			return
		}
		body, err := json.Marshal(status)
		if err != nil || len(body) >= 1048576 {
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "node migration status exceeds response limit"})
			return
		}
		_, _ = w.Write(append(body, '\n'))
	})
}

func NodeURL(base, port string) string {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil {
		return ""
	}
	u.Host = net.JoinHostPort(u.Hostname(), port)
	u.Path = "/node/v1"
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
