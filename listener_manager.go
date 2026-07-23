package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
)

// ListenerRuntimeView combines a persisted binding with its current listener
// state. It intentionally embeds ListenerBinding so API JSON remains flat.
type ListenerRuntimeView struct {
	ListenerBinding
	ListenAddr string `json:"listen_addr"`
	Listening  bool   `json:"listening"`
	Error      string `json:"error,omitempty"`
}

type managedListener struct {
	binding   ListenerBinding
	server    *Server
	addr      string
	err       error
	listening bool
}

// ListenerManager owns all persisted non-primary SOCKS listeners. Its mutex
// serializes mutations, so disk state and runtime state cannot interleave.
type ListenerManager struct {
	primaryAddr string
	pool        *ProxyPool
	store       *ConfigStore
	socksUser   string
	socksPass   string
	slots       chan struct{}

	mu        sync.Mutex
	listeners map[string]*managedListener
	started   bool
}

func NewListenerManager(primaryAddr string, pool *ProxyPool, store *ConfigStore, socksUser, socksPass string, maxConnections int) *ListenerManager {
	if maxConnections <= 0 {
		maxConnections = defaultSOCKSMaxClientConnections
	}
	return &ListenerManager{primaryAddr: primaryAddr, pool: pool, store: store, socksUser: socksUser, socksPass: socksPass, slots: make(chan struct{}, maxConnections), listeners: make(map[string]*managedListener)}
}

func (m *ListenerManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil
	}
	m.started = true
	for _, b := range m.store.Listeners() {
		if !b.Enabled {
			continue
		}
		if err := m.startLocked(b); err != nil {
			m.stopAllLocked(context.Background())
			m.started = false
			return err
		}
	}
	return nil
}

func (m *ListenerManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = false
	return m.stopAllLocked(ctx)
}

func (m *ListenerManager) stopAllLocked(ctx context.Context) error {
	var first error
	for id, listener := range m.listeners {
		if err := listener.server.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
		delete(m.listeners, id)
	}
	return first
}

func (m *ListenerManager) Bindings() []ListenerRuntimeView {
	m.mu.Lock()
	defer m.mu.Unlock()
	bindings := m.store.Listeners()
	out := make([]ListenerRuntimeView, 0, len(bindings))
	for _, b := range bindings {
		addr, _ := listenerAddr(m.primaryAddr, b.Port)
		v := ListenerRuntimeView{ListenerBinding: b, ListenAddr: addr}
		if running := m.listeners[b.ID]; running != nil {
			v.Listening = running.listening
			if running.err != nil {
				v.Error = running.err.Error()
			}
		}
		out = append(out, v)
	}
	return out
}

func (m *ListenerManager) Add(b ListenerBinding) (ListenerBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validatePort(b.Port); err != nil {
		return ListenerBinding{}, err
	}
	// Persist first: a bind failure leaves an intentionally configured but
	// stopped binding rather than an unpersisted live port.
	created, err := m.store.AddListener(b)
	if err != nil {
		return ListenerBinding{}, err
	}
	if m.started && created.Enabled {
		if err := m.startLocked(created); err != nil {
			if rollback := m.store.DeleteListener(created.ID); rollback != nil {
				return ListenerBinding{}, fmt.Errorf("start listener: %w (rollback: %v)", err, rollback)
			}
			return ListenerBinding{}, err
		}
	}
	return created, nil
}

func (m *ListenerManager) stopListenerLocked(listener *managedListener) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = listener.server.Shutdown(ctx)
}

func (m *ListenerManager) Update(b ListenerBinding) (ListenerBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validatePort(b.Port); err != nil {
		return ListenerBinding{}, err
	}
	old, ok := m.listenerBindingLocked(b.ID)
	if !ok {
		return ListenerBinding{}, fmt.Errorf("listener not found: %s", b.ID)
	}
	updated, err := m.store.UpdateListener(b)
	if err != nil {
		return ListenerBinding{}, err
	}
	oldRuntime := m.listeners[b.ID]
	if oldRuntime != nil && old.Port == updated.Port && updated.Enabled {
		policy, policyErr := m.policyLocked(updated)
		if policyErr != nil {
			if _, rollback := m.store.UpdateListener(old); rollback != nil {
				return ListenerBinding{}, fmt.Errorf("update listener route: %w (rollback: %v)", policyErr, rollback)
			}
			return ListenerBinding{}, policyErr
		}
		oldRuntime.server.setRoutePolicy(policy)
		oldRuntime.binding = updated
		return updated, nil
	}
	if oldRuntime != nil {
		m.stopListenerLocked(oldRuntime)
		delete(m.listeners, b.ID)
	}
	if m.started && updated.Enabled {
		if err := m.startLocked(updated); err != nil {
			// Restore the persistent and live old state before reporting failure.
			if _, rollback := m.store.UpdateListener(old); rollback != nil {
				return ListenerBinding{}, fmt.Errorf("start listener: %w (rollback: %v)", err, rollback)
			}
			if old.Enabled {
				_ = m.startLocked(old)
			}
			return ListenerBinding{}, err
		}
	}
	return updated, nil
}

func (m *ListenerManager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.listenerBindingLocked(id)
	if !ok {
		return fmt.Errorf("listener not found: %s", id)
	}
	if running := m.listeners[id]; running != nil {
		m.stopListenerLocked(running)
		delete(m.listeners, id)
	}
	if err := m.store.DeleteListener(id); err != nil {
		if m.started && old.Enabled {
			_ = m.startLocked(old)
		}
		return err
	}
	return nil
}

func (m *ListenerManager) listenerBindingLocked(id string) (ListenerBinding, bool) {
	for _, b := range m.store.Listeners() {
		if b.ID == id {
			return b, true
		}
	}
	return ListenerBinding{}, false
}

func (m *ListenerManager) validatePort(port int) error {
	_, primaryPort, err := net.SplitHostPort(m.primaryAddr)
	if err != nil {
		return fmt.Errorf("invalid primary listen address %q: %w", m.primaryAddr, err)
	}
	if strconv.Itoa(port) == primaryPort {
		return fmt.Errorf("port %d is already used by the primary listener", port)
	}
	return nil
}

func (m *ListenerManager) startLocked(b ListenerBinding) error {
	if err := m.validatePort(b.Port); err != nil {
		return err
	}
	addr, err := listenerAddr(m.primaryAddr, b.Port)
	if err != nil {
		return err
	}
	policy, err := m.policyLocked(b)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s := NewServerWithSharedAdmissionAndPolicy(addr, m.pool, m.store, m.socksUser, m.socksPass, m.slots, policy)
	s.setStopCallback(func(stopErr error) {
		m.recordStopped(b.ID, s, stopErr)
	})
	if err := s.StartListener(ln); err != nil {
		_ = ln.Close()
		return err
	}
	m.listeners[b.ID] = &managedListener{binding: b, server: s, addr: addr, listening: true}
	return nil
}

func (m *ListenerManager) recordStopped(id string, server *Server, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	runtime := m.listeners[id]
	if runtime == nil || runtime.server != server {
		return
	}
	runtime.listening = false
	runtime.err = err
}

func (m *ListenerManager) policyLocked(b ListenerBinding) (*listenerRoutePolicy, error) {
	if b.Mode == ListenerModeRules {
		return nil, nil
	}
	name := "listener:" + b.ID
	if b.Mode == ListenerModeFixed {
		return &listenerRoutePolicy{mode: b.Mode, group: Group{ID: name, Name: name, Strategy: StrategySticky, Nodes: []string{b.NodeKey}}}, nil
	}
	if b.Group == GroupDirect {
		return &listenerRoutePolicy{mode: b.Mode, direct: true}, nil
	}
	for _, g := range m.store.Groups() {
		if g.Name == b.Group || g.ID == b.Group {
			g.ID, g.Name = name, name
			return &listenerRoutePolicy{mode: b.Mode, targetGroup: b.Group, group: g}, nil
		}
	}
	if b.Group == GroupAny {
		return &listenerRoutePolicy{mode: b.Mode, targetGroup: b.Group, group: Group{ID: name, Name: name, Strategy: StrategySticky}}, nil
	}
	if cc, ok := parseCountryGroup(b.Group); ok {
		return &listenerRoutePolicy{mode: b.Mode, targetGroup: b.Group, group: Group{ID: name, Name: name, Strategy: StrategyLatency, Countries: []string{cc}}}, nil
	}
	return nil, fmt.Errorf("listener group not found: %s", b.Group)
}

func listenerAddr(primary string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid listener port: %d", port)
	}
	host, _, err := net.SplitHostPort(primary)
	if err != nil {
		return "", fmt.Errorf("invalid primary listen address %q: %w", primary, err)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
