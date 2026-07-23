package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRawSourceConfig(t *testing.T, dir, sourceFields string) string {
	t.Helper()
	path := filepath.Join(dir, "pool_config.json")
	data := []byte(`{"sources":[{` + sourceFields + `}],"rules":[],"groups":[]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const autoRefreshTestSource = `"id":"legacy","name":"legacy","url":"https://example.test/list","format":"plain-list","protocol":"http","enabled":true,"builtin":false`

func TestSourceAutoRefreshLegacyConfigDefaultsEnabledAndMigrates(t *testing.T) {
	dir := t.TempDir()
	path := writeRawSourceConfig(t, dir, autoRefreshTestSource)

	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := store.SourceByID("legacy")
	if !ok || !source.AutoRefreshEnabled || source.RefreshIntervalSeconds != 0 {
		t.Fatalf("migrated source = %#v, found = %v", source, ok)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"auto_refresh_enabled": true`) || !strings.Contains(string(data), `"refresh_interval_seconds": 0`) {
		t.Fatalf("migration was not persisted: %s", data)
	}
}

func TestSourceAutoRefreshExplicitFalseSurvivesLoad(t *testing.T) {
	dir := t.TempDir()
	writeRawSourceConfig(t, dir, autoRefreshTestSource+`,"auto_refresh_enabled":false,"refresh_interval_seconds":0`)

	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := store.SourceByID("legacy")
	if !ok || source.AutoRefreshEnabled {
		t.Fatalf("explicit false was lost: %#v, found = %v", source, ok)
	}
}

func TestSetSourceAutoRefreshPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := store.Sources()[0].ID
	if err := store.SetSourceAutoRefresh(id, false, 300); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := restarted.SourceByID(id)
	if !ok || source.AutoRefreshEnabled || source.RefreshIntervalSeconds != 300 {
		t.Fatalf("restarted source = %#v, found = %v", source, ok)
	}
}

func TestSetSourceAutoRefreshRejectsUnknownIDAndInvalidIntervals(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := store.Sources()[0].ID
	if err := store.SetSourceAutoRefresh("missing", false, 60); err == nil || !strings.Contains(err.Error(), "source not found") {
		t.Fatalf("unknown source error = %v", err)
	}
	for _, interval := range []int{-1, 1, 59, 604801} {
		if err := store.SetSourceAutoRefresh(id, false, interval); err == nil {
			t.Errorf("interval %d was accepted", interval)
		}
	}
	for _, interval := range []int{0, 60, 604800} {
		if err := store.SetSourceAutoRefresh(id, true, interval); err != nil {
			t.Errorf("interval %d rejected: %v", interval, err)
		}
	}
}

func TestSetSourceAutoRefreshWriteFailureLeavesMemoryUnchanged(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := store.Sources()[0].ID
	before, _ := store.SourceByID(id)
	store.path = ""

	if err := store.SetSourceAutoRefresh(id, false, 120); err == nil {
		t.Fatal("SetSourceAutoRefresh succeeded despite unwritable path")
	}
	after, _ := store.SourceByID(id)
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(afterJSON) != string(beforeJSON) {
		t.Fatalf("memory changed after persistence failure: before=%s after=%s", beforeJSON, afterJSON)
	}
}

func TestDefaultSourcesEnableAutoRefresh(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range store.Sources() {
		if !source.AutoRefreshEnabled {
			t.Fatalf("default source %q has auto refresh disabled", source.ID)
		}
	}
}
