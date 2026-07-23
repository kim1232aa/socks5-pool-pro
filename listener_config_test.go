package main

import (
	"strings"
	"testing"
)

func TestListenerRoundtripPersistsAllModes(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	custom, err := store.AddGroup(Group{Name: "tokyo-egress", Strategy: StrategySticky})
	if err != nil {
		t.Fatal(err)
	}

	group, err := store.AddListener(ListenerBinding{Name: "group port", Port: 11080, Mode: ListenerModeGroup, Group: custom.Name, Enabled: true})
	if err != nil {
		t.Fatalf("AddListener group mode: %v", err)
	}
	if group.ID == "" || group.Port != 11080 || group.Mode != ListenerModeGroup || group.Group != custom.Name || !group.Enabled {
		t.Fatalf("unexpected group binding: %#v", group)
	}

	fixed, err := store.AddListener(ListenerBinding{Name: "fixed port", Port: 11081, Mode: ListenerModeFixed, NodeKey: "socks5://1.2.3.4:1080", Enabled: false})
	if err != nil {
		t.Fatalf("AddListener fixed mode: %v", err)
	}
	if fixed.NodeKey != "socks5://1.2.3.4:1080" || fixed.Enabled != false {
		t.Fatalf("unexpected fixed binding: %#v", fixed)
	}

	rules, err := store.AddListener(ListenerBinding{Name: "rules port", Port: 11082, Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatalf("AddListener rules mode: %v", err)
	}
	if rules.Group != "" || rules.NodeKey != "" {
		t.Fatalf("rules listener retained routing fields: %#v", rules)
	}
	reload, err := NewConfigStore(store.path[:strings.LastIndex(store.path, "/")])
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	got := reload.Listeners()
	if len(got) != 3 {
		t.Fatalf("expected 3 listeners after reload, got %d", len(got))
	}
	byPort := map[int]ListenerBinding{}
	for _, b := range got {
		byPort[b.Port] = b
	}
	if byPort[11080].Group != custom.Name || byPort[11080].Mode != ListenerModeGroup {
		t.Fatalf("group listener did not roundtrip: %#v", byPort[11080])
	}
	if byPort[11081].NodeKey != "socks5://1.2.3.4:1080" || byPort[11081].Enabled {
		t.Fatalf("fixed listener did not roundtrip enabled=false: %#v", byPort[11081])
	}
	if byPort[11082].Mode != ListenerModeRules || byPort[11082].Group != "" || byPort[11082].NodeKey != "" {
		t.Fatalf("rules listener did not roundtrip: %#v", byPort[11082])
	}
}

func TestAddListenerRejectsDuplicatePortAndInvalidInput(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddListener(ListenerBinding{Name: "first", Port: 12080, Mode: ListenerModeRules, Enabled: true}); err != nil {
		t.Fatalf("AddListener first: %v", err)
	}
	if _, err := store.AddListener(ListenerBinding{Name: "dup", Port: 12080, Mode: ListenerModeRules, Enabled: true}); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("duplicate port error = %v", err)
	}
	for _, bad := range []ListenerBinding{
		{Name: "", Port: 12081, Mode: ListenerModeRules},
		{Name: "no-port", Port: 0, Mode: ListenerModeRules},
		{Name: "big-port", Port: 70000, Mode: ListenerModeRules},
		{Name: "bad-mode", Port: 12083, Mode: "weird"},
		{Name: "no-group", Port: 12084, Mode: ListenerModeGroup},
		{Name: "no-node", Port: 12085, Mode: ListenerModeFixed},
		{Name: "rules-with-group", Port: 12086, Mode: ListenerModeRules, Group: GroupAny},
		{Name: "fixed-with-group", Port: 12087, Mode: ListenerModeFixed, NodeKey: "socks5://1.2.3.4:1080", Group: GroupAny},
		{Name: "missing-group", Port: 12088, Mode: ListenerModeGroup, Group: "does-not-exist"},
		{Name: "ctrl\nname", Port: 12089, Mode: ListenerModeRules},
	} {
		if _, err := store.AddListener(bad); err == nil {
			t.Errorf("AddListener accepted invalid binding %#v", bad)
		}
	}
	if got := len(store.Listeners()); got != 1 {
		t.Fatalf("invalid bindings mutated store: %d listeners", got)
	}
}

func TestUpdateAndDeleteListener(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	custom, err := store.AddGroup(Group{Name: "eu-egress", Strategy: StrategyRoundRobin})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AddListener(ListenerBinding{Name: "first", Port: 13080, Mode: ListenerModeGroup, Group: custom.Name, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AddListener(ListenerBinding{Name: "second", Port: 13081, Mode: ListenerModeRules, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	// Update name + disable; port unchanged.
	updated, err := store.UpdateListener(ListenerBinding{ID: first.ID, Name: "renamed", Port: 13080, Mode: ListenerModeGroup, Group: custom.Name, Enabled: false})
	if err != nil {
		t.Fatalf("UpdateListener: %v", err)
	}
	if updated.Name != "renamed" || updated.Enabled {
		t.Fatalf("update did not apply: %#v", updated)
	}

	// Port collision with the other listener.
	if _, err := store.UpdateListener(ListenerBinding{ID: first.ID, Name: "renamed", Port: 13081, Mode: ListenerModeGroup, Group: custom.Name, Enabled: false}); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("update port collision error = %v", err)
	}

	// Mode switch group -> fixed requires node_key.
	if _, err := store.UpdateListener(ListenerBinding{ID: first.ID, Name: "renamed", Port: 13080, Mode: ListenerModeFixed}); err == nil {
		t.Fatal("UpdateListener accepted fixed mode without node_key")
	}

	// Unknown id.
	if _, err := store.UpdateListener(ListenerBinding{ID: "nope", Name: "x", Port: 13099, Mode: ListenerModeRules}); err == nil {
		t.Fatal("UpdateListener accepted unknown id")
	}

	if err := store.DeleteListener(second.ID); err != nil {
		t.Fatalf("DeleteListener: %v", err)
	}
	if err := store.DeleteListener(second.ID); err == nil {
		t.Fatal("DeleteListener accepted missing id")
	}
	if got := len(store.Listeners()); got != 1 {
		t.Fatalf("expected 1 listener after delete, got %d", got)
	}
	if store.Listeners()[0].ID != first.ID {
		t.Fatal("DeleteListener removed the wrong binding")
	}
}

func TestDeleteGroupRejectedWhenReferencedByListener(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	custom, err := store.AddGroup(Group{Name: "pinned-egress", Strategy: StrategySticky})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddListener(ListenerBinding{Name: "uses group", Port: 14080, Mode: ListenerModeGroup, Group: custom.Name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGroup(custom.ID); err == nil || !strings.Contains(err.Error(), "listener") {
		t.Fatalf("DeleteGroup with listener reference error = %v", err)
	}
	// Switching the listener away from group mode frees the group.
	if _, err := store.UpdateListener(ListenerBinding{ID: store.Listeners()[0].ID, Name: "uses group", Port: 14080, Mode: ListenerModeRules, Enabled: true}); err != nil {
		t.Fatalf("UpdateListener to rules mode: %v", err)
	}
	if err := store.DeleteGroup(custom.ID); err != nil {
		t.Fatalf("DeleteGroup after freeing listener: %v", err)
	}
}

func TestReplaceListenersRollsBackAtomically(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	custom, err := store.AddGroup(Group{Name: "valid", Strategy: StrategySticky})
	if err != nil {
		t.Fatal(err)
	}
	good := ListenerBinding{ID: "lst-keep", Name: "keep", Port: 15080, Mode: ListenerModeGroup, Group: custom.Name, Enabled: true}
	if err := store.ReplaceListeners([]ListenerBinding{good}); err != nil {
		t.Fatalf("ReplaceListeners good: %v", err)
	}

	// A replacement with a duplicate port must fail and leave the store intact.
	bad := []ListenerBinding{
		{ID: "lst-a", Name: "a", Port: 15090, Mode: ListenerModeRules, Enabled: true},
		{ID: "lst-b", Name: "b", Port: 15090, Mode: ListenerModeRules, Enabled: true},
	}
	if err := store.ReplaceListeners(bad); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("ReplaceListeners duplicate error = %v", err)
	}
	got := store.Listeners()
	if len(got) != 1 || got[0].ID != "lst-keep" {
		t.Fatalf("ReplaceListeners failure mutated store: %#v", got)
	}
}

func TestListenerFixedNodeKeyRejectsAnyFallback(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddListener(ListenerBinding{Name: "fake any", Port: 16080, Mode: ListenerModeFixed, NodeKey: GroupAny, Enabled: true}); err == nil {
		t.Fatal("fixed listener accepted a non-proxy node key")
	}
	// group mode pointing at ANY is allowed (ANY is a built-in), but the
	// contract says group mode may not fall back to ANY on pick failure - that
	// is enforced by the listener manager, not the store. Here we only verify
	// the store accepts the reserved name.
	if _, err := store.AddListener(ListenerBinding{Name: "any group", Port: 16081, Mode: ListenerModeGroup, Group: GroupAny, Enabled: true}); err != nil {
		t.Fatalf("AddListener group ANY rejected: %v", err)
	}
}
