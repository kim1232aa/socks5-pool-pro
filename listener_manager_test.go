package main

import (
	"context"
	"errors"
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
	waitForConnectionSlots(t, manager.listeners[created.ID].server, 1)

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
