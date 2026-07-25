package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// ListenerRuntimeView combines a persisted binding with its current listener
// state. It intentionally embeds ListenerBinding so API JSON remains flat.
type ListenerRuntimeView struct {
	ListenerBinding
	ListenAddr string `json:"listen_addr"`
	Listening  bool   `json:"listening"`
	Error      string `json:"error,omitempty"`
}

const listenerRollbackTimeout = 250 * time.Millisecond

// ListenerFatalEvent reports an auxiliary listener that stopped without a
// corresponding manager operation. The main process may consume FatalEvents
// and decide whether an auxiliary listener failure is process-fatal.
type ListenerFatalEvent struct {
	ID   string
	Addr string
	Err  error
}

type managedListener struct {
	binding      ListenerBinding
	server       *Server
	addr         string
	err          error
	listening    bool
	expectedStop bool
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
	fatalEvents chan ListenerFatalEvent
	listen      func(network, addr string) (net.Listener, error)

	mu        sync.Mutex
	listeners map[string]*managedListener
	started   bool
}

func NewListenerManager(primaryAddr string, pool *ProxyPool, store *ConfigStore, socksUser, socksPass string, maxConnections int) *ListenerManager {
	if maxConnections <= 0 {
		maxConnections = defaultSOCKSMaxClientConnections
	}
	return &ListenerManager{
		primaryAddr: primaryAddr,
		pool:        pool,
		store:       store,
		socksUser:   socksUser,
		socksPass:   socksPass,
		slots:       make(chan struct{}, maxConnections),
		fatalEvents: make(chan ListenerFatalEvent, maxConfigListeners),
		listen:      net.Listen,
		listeners:   make(map[string]*managedListener),
	}
}

// FatalEvents exposes unexpected auxiliary listener exits. The channel remains
// open for the manager lifetime so process-level integration can select on it.
func (m *ListenerManager) FatalEvents() <-chan ListenerFatalEvent {
	return m.fatalEvents
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
			ctx, cancel := context.WithTimeout(context.Background(), listenerRollbackTimeout)
			rollbackErr := m.stopAllLocked(ctx)
			cancel()
			m.started = false
			return errors.Join(err, rollbackErr)
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
	var errs []error
	for id, listener := range m.listeners {
		listener.expectedStop = true
		if err := listener.server.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop listener %s: %w", listener.addr, err))
		}
		delete(m.listeners, id)
	}
	return errors.Join(errs...)
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
	// Persist first: a bind failure is rolled back before Add returns.
	created, err := m.store.AddListener(b)
	if err != nil {
		if isConfigDurabilityUncertain(err) {
			return ListenerBinding{}, errors.Join(err, m.reconcileRuntimeLocked())
		}
		return ListenerBinding{}, err
	}
	if m.started && created.Enabled {
		if startErr := m.startLocked(created); startErr != nil {
			rollbackErr := m.store.DeleteListener(created.ID)
			return ListenerBinding{}, errors.Join(
				fmt.Errorf("start listener: %w", startErr),
				wrapListenerRollbackError("delete persisted listener", rollbackErr),
			)
		}
	}
	return created, nil
}

func (m *ListenerManager) stopListenerLocked(listener *managedListener) {
	listener.expectedStop = true
	ctx, cancel := context.WithTimeout(context.Background(), listenerRollbackTimeout)
	defer cancel()
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
		if isConfigDurabilityUncertain(err) {
			return ListenerBinding{}, errors.Join(err, m.reconcileRuntimeLocked())
		}
		return ListenerBinding{}, err
	}
	oldRuntime := m.listeners[b.ID]
	if oldRuntime != nil && old.Port == updated.Port && updated.Enabled {
		policy, policyErr := m.policyLocked(updated)
		if policyErr != nil {
			_, rollbackErr := m.store.UpdateListener(old)
			return ListenerBinding{}, errors.Join(
				fmt.Errorf("update listener route: %w", policyErr),
				wrapListenerRollbackError("restore persisted listener", rollbackErr),
			)
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
		if startErr := m.startLocked(updated); startErr != nil {
			// Attempt every rollback step and preserve every failure for callers.
			_, persistErr := m.store.UpdateListener(old)
			var restartErr error
			if old.Enabled {
				restartErr = m.startLocked(old)
			}
			return ListenerBinding{}, errors.Join(
				fmt.Errorf("start listener: %w", startErr),
				wrapListenerRollbackError("restore persisted listener", persistErr),
				wrapListenerRollbackError("restart old listener", restartErr),
			)
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
	if persistErr := m.store.DeleteListener(id); persistErr != nil {
		if isConfigDurabilityUncertain(persistErr) {
			return errors.Join(fmt.Errorf("delete persisted listener: %w", persistErr), m.reconcileRuntimeLocked())
		}
		var restartErr error
		if m.started && old.Enabled {
			restartErr = m.startLocked(old)
		}
		return errors.Join(
			fmt.Errorf("delete persisted listener: %w", persistErr),
			wrapListenerRollbackError("restart deleted listener", restartErr),
		)
	}
	return nil
}

func isConfigDurabilityUncertain(err error) bool {
	var persistenceErr *ConfigPersistenceError
	return errors.As(err, &persistenceErr) && persistenceErr.Outcome == ConfigPersistenceDurabilityUncertain
}

// reconcileRuntimeLocked makes the live listeners match the ConfigStore's
// published snapshot after a post-rename durability error. That outcome is not
// a rollback: memory and the visible path already contain the new config.
func (m *ListenerManager) reconcileRuntimeLocked() error {
	configured := m.store.Snapshot().Listeners
	desired := make(map[string]ListenerBinding, len(configured))
	for _, binding := range configured {
		desired[binding.ID] = binding
	}

	for id, runtime := range m.listeners {
		binding, exists := desired[id]
		if exists && binding.Enabled && runtime.listening && runtime.binding.Port == binding.Port {
			policy, err := m.policyLocked(binding)
			if err != nil {
				return fmt.Errorf("reconcile listener %s route: %w", id, err)
			}
			runtime.server.setRoutePolicy(policy)
			runtime.binding = binding
			continue
		}
		m.stopListenerLocked(runtime)
		delete(m.listeners, id)
	}
	if !m.started {
		return nil
	}
	var errs []error
	for _, binding := range configured {
		if !binding.Enabled || m.listeners[binding.ID] != nil {
			continue
		}
		if err := m.startLocked(binding); err != nil {
			errs = append(errs, fmt.Errorf("reconcile listener %s: %w", binding.ID, err))
		}
	}
	return errors.Join(errs...)
}

func wrapListenerRollbackError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("listener rollback %s: %w", action, err)
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
	ln, err := m.listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s := NewServerWithSharedAdmissionAndPolicy(addr, m.pool, m.store, m.socksUser, m.socksPass, m.slots, policy)
	s.setStopCallback(func(stopErr error) {
		m.recordStopped(b.ID, s, stopErr)
	})
	runtime := &managedListener{binding: b, server: s, addr: addr, listening: true}
	m.listeners[b.ID] = runtime
	if err := s.StartListener(ln); err != nil {
		delete(m.listeners, b.ID)
		_ = ln.Close()
		return err
	}
	return nil
}

func (m *ListenerManager) recordStopped(id string, server *Server, stopErr error) {
	m.mu.Lock()
	runtime := m.listeners[id]
	if runtime == nil || runtime.server != server {
		m.mu.Unlock()
		return
	}
	if stopErr == nil {
		stopErr = errors.New("listener stopped unexpectedly")
	}
	runtime.listening = false
	runtime.err = stopErr
	expected := runtime.expectedStop
	event := ListenerFatalEvent{ID: id, Addr: runtime.addr, Err: stopErr}
	m.mu.Unlock()

	if expected {
		return
	}
	select {
	case m.fatalEvents <- event:
	default:
	}
}

func (m *ListenerManager) policyLocked(b ListenerBinding) (*listenerRoutePolicy, error) {
	if b.Mode == ListenerModeRules {
		return nil, nil
	}
	name := "listener:" + b.ID
	if b.Mode == ListenerModeFixed {
		return &listenerRoutePolicy{mode: b.Mode, group: Group{ID: name, Name: name, Strategy: StrategySticky, Nodes: []string{b.NodeKey}}}, nil
	}
	resolved, ok := resolveGroupReference(b.Group, m.store.Groups())
	if !ok {
		return nil, fmt.Errorf("listener group not found: %s", b.Group)
	}
	switch resolved.kind {
	case groupReferenceDirect:
		return &listenerRoutePolicy{mode: b.Mode, direct: true}, nil
	case groupReferenceAny:
		return &listenerRoutePolicy{mode: b.Mode, targetGroup: resolved.canonical, group: Group{ID: name, Name: name, Strategy: StrategySticky}}, nil
	case groupReferenceCountry:
		return &listenerRoutePolicy{mode: b.Mode, targetGroup: resolved.canonical, group: Group{ID: name, Name: name, Strategy: StrategyLatency, Countries: []string{resolved.country}}}, nil
	default:
		g := *resolved.group
		g.ID, g.Name = name, name
		return &listenerRoutePolicy{mode: b.Mode, targetGroup: resolved.canonical, group: g}, nil
	}
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
