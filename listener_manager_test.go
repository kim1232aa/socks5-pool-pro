package main

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func freeListenerPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func dialListener(t *testing.T, port int, wantOpen bool) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
	if conn != nil {
		_ = conn.Close()
	}
	if wantOpen && err != nil {
		t.Fatalf("listener port %d is closed: %v", port, err)
	}
	if !wantOpen && err == nil {
		t.Fatalf("listener port %d is still open", port)
	}
}

func waitForManagedConnectionSlots(t *testing.T, server *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(server.connSlots) != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(server.connSlots); got != want {
		t.Fatalf("active connection slots = %d, want %d", got, want)
	}
}

func TestListenerManagerAddDisableEnableAndDelete(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})

	port := freeListenerPort(t)
	created, err := manager.Add(ListenerBinding{Name: "rules", Port: port, Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if views := manager.Bindings(); len(views) != 1 || !views[0].Listening || views[0].ListenAddr == "" {
		t.Fatalf("running binding = %#v", views)
	}
	dialListener(t, port, true)
	runningServer := manager.listeners[created.ID].server
	created.Mode = ListenerModeGroup
	created.Group = GroupAny
	updated, err := manager.Update(created)
	if err != nil {
		t.Fatal(err)
	}
	if manager.listeners[created.ID].server != runningServer {
		t.Fatal("route-only update rebound the listener")
	}
	created = updated

	created.Enabled = false
	if _, err := manager.Update(created); err != nil {
		t.Fatal(err)
	}
	dialListener(t, port, false)

	created.Enabled = true
	if _, err := manager.Update(created); err != nil {
		t.Fatal(err)
	}
	dialListener(t, port, true)

	if err := manager.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	dialListener(t, port, false)
	if got := store.Listeners(); len(got) != 0 {
		t.Fatalf("deleted binding persisted: %#v", got)
	}
}

func TestListenerManagerDisableClosesHeldConnectionWithoutBlocking(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	port := freeListenerPort(t)
	created, err := manager.Add(ListenerBinding{Name: "held", Port: port, Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitForManagedConnectionSlots(t, manager.listeners[created.ID].server, 1)

	created.Enabled = false
	started := time.Now()
	if _, err := manager.Update(created); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("disable blocked for %s", elapsed)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("held connection survived listener disable")
	}
}

func TestListenerManagerStopCancelsOldGenerationUpstreamHandshake(t *testing.T) {
	for _, test := range []struct {
		name    string
		restart func(*testing.T, *ListenerManager, ListenerBinding) ListenerBinding
	}{
		{
			name: "disable and enable",
			restart: func(t *testing.T, manager *ListenerManager, binding ListenerBinding) ListenerBinding {
				binding.Enabled = false
				if _, err := manager.Update(binding); err != nil {
					t.Fatal(err)
				}
				binding.Enabled = true
				updated, err := manager.Update(binding)
				if err != nil {
					t.Fatal(err)
				}
				return updated
			},
		},
		{
			name: "restart on new port",
			restart: func(t *testing.T, manager *ListenerManager, binding ListenerBinding) ListenerBinding {
				binding.Port = freeListenerPort(t)
				updated, err := manager.Update(binding)
				if err != nil {
					t.Fatal(err)
				}
				return updated
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			blackhole, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer blackhole.Close()
			upstreamAccepted := make(chan net.Conn, 1)
			go func() {
				conn, acceptErr := blackhole.Accept()
				if acceptErr == nil {
					upstreamAccepted <- conn
				}
			}()

			host, port, err := net.SplitHostPort(blackhole.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			upstream := Proxy{IP: host, Port: port, Protocol: "http", Available: true}
			pool := NewProxyPool()
			pool.Prime([]Proxy{upstream}, nil)
			store, err := NewConfigStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := NewListenerManager("127.0.0.1:1", pool, store, "", "", 1)
			if err := manager.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = manager.Shutdown(ctx)
			}()

			binding, err := manager.Add(ListenerBinding{
				Name: "blackhole", Port: freeListenerPort(t), Mode: ListenerModeFixed,
				NodeKey: upstream.Key(), Enabled: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			oldServer := manager.listeners[binding.ID].server
			oldClient, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(binding.Port)))
			if err != nil {
				t.Fatal(err)
			}
			defer oldClient.Close()
			if _, err := oldClient.Write([]byte{socks5Version, 1, socks5NoAuth}); err != nil {
				t.Fatal(err)
			}
			methodReply := make([]byte, 2)
			if _, err := io.ReadFull(oldClient, methodReply); err != nil {
				t.Fatal(err)
			}
			target := []byte{socks5Version, cmdConnect, 0, atypDomain, byte(len("example.com"))}
			target = append(target, "example.com"...)
			target = append(target, 0x01, 0xbb)
			if _, err := oldClient.Write(target); err != nil {
				t.Fatal(err)
			}

			var blackholeConn net.Conn
			select {
			case blackholeConn = <-upstreamAccepted:
				defer blackholeConn.Close()
			case <-time.After(time.Second):
				t.Fatal("old generation did not enter the upstream handshake")
			}
			waitForManagedConnectionSlots(t, oldServer, 1)

			started := time.Now()
			binding = test.restart(t, manager, binding)
			newServer := manager.listeners[binding.ID].server
			if newServer == oldServer {
				t.Fatal("listener restart reused the old Server generation")
			}
			newClient, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(binding.Port)), time.Second)
			if err != nil {
				t.Fatalf("new generation did not accept promptly: %v", err)
			}
			defer newClient.Close()
			_ = newClient.SetDeadline(time.Now().Add(time.Second))
			if _, err := newClient.Write([]byte{socks5Version, 1, socks5NoAuth}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(newClient, methodReply); err != nil {
				t.Fatalf("old generation retained the only connection slot: %v", err)
			}
			if elapsed := time.Since(started); elapsed >= 2*time.Second {
				t.Fatalf("new generation waited for the 10-second upstream timeout: %s", elapsed)
			}
			_ = newClient.Close()
			waitForManagedConnectionSlots(t, newServer, 0)

			if successes, failures := pool.StatsOf(upstream.Key()); successes != 0 || failures != 0 {
				t.Fatalf("canceled generation changed pool stats: successes=%d failures=%d", successes, failures)
			}
			got, ok := pool.Find(upstream.Key())
			if !ok || !got.Available || got.HealthInvalidated {
				t.Fatalf("canceled generation changed pool health: found=%v proxy=%+v", ok, got)
			}
		})
	}
}

func TestListenerManagerReportsUnexpectedStop(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := store.AddListener(ListenerBinding{Name: "stopped", Port: freeListenerPort(t), Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	server := NewServer("", manager.pool, store)
	manager.listeners[binding.ID] = &managedListener{binding: binding, server: server, listening: true}
	manager.recordStopped(binding.ID, server, errors.New("accept failed"))

	views := manager.Bindings()
	if len(views) != 1 || views[0].Listening || views[0].Error != "accept failed" {
		t.Fatalf("stopped listener status = %#v", views)
	}
}

func TestListenerManagerRejectsPrimaryPort(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:18080", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Add(ListenerBinding{Name: "primary conflict", Port: 18080, Mode: ListenerModeRules, Enabled: true}); err == nil {
		t.Fatal("Add accepted the primary listener port")
	}
	if got := store.Listeners(); len(got) != 0 {
		t.Fatalf("primary-port conflict persisted: %#v", got)
	}
}

func TestListenerManagerBindConflictDoesNotPersist(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	port := occupied.Addr().(*net.TCPAddr).Port
	if _, err := manager.Add(ListenerBinding{Name: "conflict", Port: port, Mode: ListenerModeRules, Enabled: true}); err == nil {
		t.Fatal("Add succeeded on an occupied port")
	}
	if got := store.Listeners(); len(got) != 0 {
		t.Fatalf("failed bind persisted: %#v", got)
	}
}

func TestListenerManagerBuildsIndependentGroupPolicies(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	group, err := store.AddGroup(Group{Name: "jp-auto", Strategy: StrategyRoundRobin, Countries: []string{"JP"}})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	first, err := manager.policyLocked(ListenerBinding{ID: "one", Mode: ListenerModeGroup, Group: group.Name})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.policyLocked(ListenerBinding{ID: "two", Mode: ListenerModeGroup, Group: group.Name})
	if err != nil {
		t.Fatal(err)
	}
	if first.group.Name == second.group.Name || first.group.Strategy != StrategyRoundRobin || second.group.Strategy != StrategyRoundRobin {
		t.Fatalf("listener policies share cursor scope or lost strategy: first=%#v second=%#v", first.group, second.group)
	}
	if first.group.Name != "listener:one" || second.group.Name != "listener:two" {
		t.Fatalf("listener cursor names = %q, %q", first.group.Name, second.group.Name)
	}
	if err := store.SetGroupStrategy(group.ID, StrategySpeed); err != nil {
		t.Fatal(err)
	}
	refreshed := effectiveListenerGroup(first, store.Groups())
	if refreshed.Strategy != StrategySpeed || refreshed.Name != "listener:one" {
		t.Fatalf("listener did not pick up group strategy update: %#v", refreshed)
	}
}

func TestListenerManagerStartsAndMatchesCaseInsensitiveGroupReference(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	group, err := store.AddGroup(Group{Name: "Tokyo-Egress", Strategy: StrategySticky, Countries: []string{"JP"}})
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	jp := testProxy("socks5", "192.0.2.40", "1080", true)
	jp.Country = "JP"
	us := testProxy("socks5", "192.0.2.41", "1080", true)
	us.Country = "US"
	pool.Prime([]Proxy{jp, us}, nil)

	manager := NewListenerManager("127.0.0.1:1", pool, store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	port := freeListenerPort(t)
	created, err := manager.Add(ListenerBinding{Name: "tokyo", Port: port, Mode: ListenerModeGroup, Group: "tokyo-egress", Enabled: true})
	if err != nil {
		t.Fatalf("Add case-insensitive group listener: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	if created.Group != group.Name {
		t.Fatalf("stored listener group = %q, want canonical name %q", created.Group, group.Name)
	}
	policy := manager.listeners[created.ID].server.currentRoutePolicy()
	effective := effectiveListenerGroup(policy, store.Groups())
	picked, ok, direct := pool.Pick(effective.Name, append(store.Groups(), effective))
	if !ok || direct || picked.Key() != jp.Key() {
		t.Fatalf("case-insensitive listener pick = %+v, ok=%v direct=%v; want JP node", picked, ok, direct)
	}
}

func TestListenerManagerExpectedStopDoesNotEmitFatalEvent(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	created, err := manager.Add(ListenerBinding{Name: "expected", Port: freeListenerPort(t), Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	created.Enabled = false
	if _, err := manager.Update(created); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-manager.FatalEvents():
		t.Fatalf("expected stop emitted fatal event: %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestListenerManagerUnexpectedAcceptExitEmitsFatalEvent(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	created, err := manager.Add(ListenerBinding{Name: "unexpected", Port: freeListenerPort(t), Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	runtime := manager.listeners[created.ID]
	runtime.server.stateMu.Lock()
	ln := runtime.server.listener
	runtime.server.stateMu.Unlock()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-manager.FatalEvents():
		if event.ID != created.ID || event.Addr != runtime.addr || !errors.Is(event.Err, net.ErrClosed) {
			t.Fatalf("fatal event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("unexpected accept exit was not observable")
	}
	views := manager.Bindings()
	if len(views) != 1 || views[0].Listening || views[0].Error == "" {
		t.Fatalf("unexpected stop status = %#v", views)
	}
}

func TestListenerManagerStartFailureRollbackIsBounded(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AddListener(ListenerBinding{Name: "first", Port: freeListenerPort(t), Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AddListener(ListenerBinding{Name: "second", Port: freeListenerPort(t), Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	secondStarted := make(chan struct{})
	releaseFailure := make(chan struct{})
	startErr := errors.New("second listener start failed")
	var firstServer *Server
	manager.listen = func(network, addr string) (net.Listener, error) {
		if addr == net.JoinHostPort("127.0.0.1", strconv.Itoa(second.Port)) {
			firstServer = manager.listeners[first.ID].server
			close(secondStarted)
			<-releaseFailure
			return nil, startErr
		}
		return net.Listen(network, addr)
	}
	startDone := make(chan error, 1)
	go func() { startDone <- manager.Start() }()
	<-secondStarted
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(first.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitForManagedConnectionSlots(t, firstServer, 1)
	close(releaseFailure)

	select {
	case err := <-startDone:
		if !errors.Is(err, startErr) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Start error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup rollback blocked on an in-flight handshake")
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("startup rollback did not cancel the old handshake")
	}
}

func TestListenerManagerAddJoinsStartAndRollbackErrors(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	originalPath := store.path
	startErr := errors.New("listener start failed")
	manager.listen = func(string, string) (net.Listener, error) {
		store.path = t.TempDir()
		return nil, startErr
	}
	_, err = manager.Add(ListenerBinding{Name: "rollback", Port: freeListenerPort(t), Mode: ListenerModeRules, Enabled: true})
	store.path = originalPath
	if !errors.Is(err, startErr) {
		t.Fatalf("Add error lost start failure: %v", err)
	}
	var persistenceErr *ConfigPersistenceError
	if !errors.As(err, &persistenceErr) {
		t.Fatalf("Add error lost rollback failure: %v", err)
	}
}

func TestListenerManagerUpdateJoinsStartRollbackAndRestartErrors(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	old, err := manager.Add(ListenerBinding{Name: "old", Port: freeListenerPort(t), Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	originalPath := store.path
	startErr := errors.New("updated listener start failed")
	restartErr := errors.New("old listener restart failed")
	newPort := freeListenerPort(t)
	manager.listen = func(_ string, addr string) (net.Listener, error) {
		if addr == net.JoinHostPort("127.0.0.1", strconv.Itoa(newPort)) {
			store.path = t.TempDir()
			return nil, startErr
		}
		return nil, restartErr
	}
	old.Port = newPort
	_, err = manager.Update(old)
	store.path = originalPath
	if !errors.Is(err, startErr) || !errors.Is(err, restartErr) {
		t.Fatalf("Update error lost start/restart failure: %v", err)
	}
	var persistenceErr *ConfigPersistenceError
	if !errors.As(err, &persistenceErr) {
		t.Fatalf("Update error lost persistence rollback failure: %v", err)
	}
}

func TestListenerManagerDeleteJoinsPersistenceAndRestartErrors(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	created, err := manager.Add(ListenerBinding{Name: "delete", Port: freeListenerPort(t), Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	originalPath := store.path
	store.path = t.TempDir()
	restartErr := errors.New("deleted listener restart failed")
	manager.listen = func(string, string) (net.Listener, error) { return nil, restartErr }
	err = manager.Delete(created.ID)
	store.path = originalPath
	if !errors.Is(err, restartErr) {
		t.Fatalf("Delete error lost restart failure: %v", err)
	}
	var persistenceErr *ConfigPersistenceError
	if !errors.As(err, &persistenceErr) {
		t.Fatalf("Delete error lost persistence failure: %v", err)
	}
}

func injectConfigDirectorySyncFailure(t *testing.T) error {
	t.Helper()
	syncErr := errors.New("injected directory sync failure")
	originalSync := syncPrivateFileDirectory
	syncPrivateFileDirectory = func(string) error { return syncErr }
	t.Cleanup(func() { syncPrivateFileDirectory = originalSync })
	return syncErr
}

func requireDurabilityUncertain(t *testing.T, err, syncErr error) {
	t.Helper()
	if !errors.Is(err, syncErr) {
		t.Fatalf("operation error = %v, want injected sync failure", err)
	}
	var persistenceErr *ConfigPersistenceError
	if !errors.As(err, &persistenceErr) || persistenceErr.Outcome != ConfigPersistenceDurabilityUncertain {
		t.Fatalf("operation error = %T %v, want durability_uncertain", err, err)
	}
}

func TestListenerManagerAddConvergesRuntimeAfterDurabilityUncertain(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})

	port := freeListenerPort(t)
	syncErr := injectConfigDirectorySyncFailure(t)
	_, err = manager.Add(ListenerBinding{Name: "uncertain add", Port: port, Mode: ListenerModeRules, Enabled: true})
	requireDurabilityUncertain(t, err, syncErr)

	bindings := store.Listeners()
	if len(bindings) != 1 || !bindings[0].Enabled || bindings[0].Port != port {
		t.Fatalf("store listeners = %#v, want committed enabled listener", bindings)
	}
	if runtime := manager.listeners[bindings[0].ID]; runtime == nil || runtime.binding != bindings[0] || !runtime.listening {
		t.Fatalf("runtime did not converge to committed add: %#v", runtime)
	}
	dialListener(t, port, true)
}

func TestListenerManagerUpdateConvergesRuntimeAfterDurabilityUncertain(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	old, err := manager.Add(ListenerBinding{Name: "old", Port: freeListenerPort(t), Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	oldPort := old.Port
	updated := old
	updated.Name = "committed update"
	updated.Port = freeListenerPort(t)

	syncErr := injectConfigDirectorySyncFailure(t)
	_, err = manager.Update(updated)
	requireDurabilityUncertain(t, err, syncErr)

	bindings := store.Listeners()
	if len(bindings) != 1 || bindings[0].ID != old.ID || bindings[0].Name != updated.Name || bindings[0].Port != updated.Port {
		t.Fatalf("store listeners = %#v, want committed update %#v", bindings, updated)
	}
	if runtime := manager.listeners[old.ID]; runtime == nil || runtime.binding != bindings[0] || !runtime.listening {
		t.Fatalf("runtime did not converge to committed update: %#v", runtime)
	}
	dialListener(t, oldPort, false)
	dialListener(t, updated.Port, true)
}

func TestListenerManagerDeleteConvergesRuntimeAfterDurabilityUncertain(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewListenerManager("127.0.0.1:1", NewProxyPool(), store, "", "", 8)
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	created, err := manager.Add(ListenerBinding{Name: "delete", Port: freeListenerPort(t), Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	syncErr := injectConfigDirectorySyncFailure(t)
	err = manager.Delete(created.ID)
	requireDurabilityUncertain(t, err, syncErr)

	if bindings := store.Listeners(); len(bindings) != 0 {
		t.Fatalf("store listeners = %#v, want committed deletion", bindings)
	}
	if runtime := manager.listeners[created.ID]; runtime != nil {
		t.Fatalf("deleted listener was restarted after uncertain commit: %#v", runtime)
	}
	dialListener(t, created.Port, false)
}
