package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTestPoolConfig(t *testing.T, dir string, cfg PoolConfig) string {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pool_config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTestPoolConfig(t *testing.T, path string) PoolConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg PoolConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNewConfigStoreMigratesOnlyLegacyProxyIPMetadataAndPersists(t *testing.T) {
	dir := t.TempDir()
	legacy := Source{
		ID: builtinProxyIPSourceID, Name: legacyProxyIPSourceName,
		URL: "https://mirror.example.test/custom.json?region=jp", Format: FormatProxyIPJSON,
		Protocol: "custom-protocol-marker", Enabled: true, Builtin: true,
		Note: legacyProxyIPSourceNote,
	}
	other := Source{
		ID: "custom-keep", Name: legacyProxyIPSourceName,
		URL: "https://example.test/list", Format: FormatPlainList, Protocol: "http",
		Enabled: false, Builtin: false, Note: legacyProxyIPSourceNote,
	}
	rules := []Rule{{ID: "keep-rule", Type: RuleDomain, Value: "example.test", Group: "keep-group"}}
	groups := []Group{{
		ID: "keep-group-id", Name: "keep-group", Strategy: StrategyLatency,
		Countries: []string{"JP"}, Protocols: []string{"socks5"}, Sources: []string{"custom-keep"},
		Nodes: []string{"socks5://192.0.2.1:1080"},
	}}
	path := writeTestPoolConfig(t, dir, PoolConfig{
		Sources:  []Source{legacy, other},
		Rules:    rules,
		Groups:   groups,
		CheckURL: "https://health.example.test/check",
	})

	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := store.Snapshot()
	if len(got.Sources) != 2 {
		t.Fatalf("source count = %d, want 2", len(got.Sources))
	}

	wantMigrated := legacy
	wantMigrated.Name = currentProxyIPSourceName
	wantMigrated.Note = currentProxyIPSourceNote
	if !reflect.DeepEqual(got.Sources[0], wantMigrated) {
		t.Fatalf("migrated source = %#v, want %#v", got.Sources[0], wantMigrated)
	}
	if !reflect.DeepEqual(got.Sources[1], other) {
		t.Fatalf("unrelated source changed: got %#v, want %#v", got.Sources[1], other)
	}
	if store.CheckURL() != "https://health.example.test/check" {
		t.Fatalf("check URL = %q, want preserved value", store.CheckURL())
	}
	if !reflect.DeepEqual(got.Rules, rules) || !reflect.DeepEqual(got.Groups, groups) {
		t.Fatalf("rules/groups changed: rules=%#v groups=%#v", got.Rules, got.Groups)
	}

	onDisk := readTestPoolConfig(t, path)
	if !reflect.DeepEqual(onDisk.Sources, got.Sources) ||
		!reflect.DeepEqual(onDisk.Rules, got.Rules) ||
		!reflect.DeepEqual(onDisk.Groups, got.Groups) ||
		onDisk.CheckURL != store.CheckURL() {
		t.Fatalf("persisted migration = %#v, in-memory sources = %#v", onDisk, got.Sources)
	}
}

func TestNewConfigStoreMigratesPreBuiltinLegacyRecord(t *testing.T) {
	dir := t.TempDir()
	legacy := Source{
		ID: builtinProxyIPSourceID, Name: legacyProxyIPSourceName,
		URL: "https://zip.cm.edu.kg/all.json", Format: FormatProxyIPJSON,
		Enabled: true, Builtin: false, Note: legacyProxyIPSourceNote,
	}
	path := writeTestPoolConfig(t, dir, PoolConfig{Sources: []Source{legacy}})

	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := store.Sources()[0]
	if got.Name != currentProxyIPSourceName || got.Note != currentProxyIPSourceNote {
		t.Fatalf("pre-Builtin legacy metadata was not migrated: %#v", got)
	}
	if got.Enabled != legacy.Enabled || got.Builtin != legacy.Builtin || got.URL != legacy.URL || got.Format != legacy.Format {
		t.Fatalf("non-metadata fields changed: got %#v, original %#v", got, legacy)
	}
	if persisted := readTestPoolConfig(t, path).Sources[0]; !reflect.DeepEqual(persisted, got) {
		t.Fatalf("pre-Builtin migration not persisted: %#v vs %#v", persisted, got)
	}
}

func TestNewConfigStorePreservesCustomizedProxyIPMetadata(t *testing.T) {
	dir := t.TempDir()
	custom := Source{
		ID: builtinProxyIPSourceID, Name: "我的专用反代源",
		URL: "https://example.test/proxyip.json", Format: FormatProxyIPJSON,
		Enabled: true, Builtin: true, Note: "保留我的说明",
	}
	path := writeTestPoolConfig(t, dir, PoolConfig{Sources: []Source{custom}})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Sources()[0]; !reflect.DeepEqual(got, custom) {
		t.Fatalf("custom metadata changed: got %#v, want %#v", got, custom)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("config without legacy metadata was unexpectedly rewritten")
	}
}

func TestNewConfigStoreDoesNotReAddDeletedBuiltinProxyIPSource(t *testing.T) {
	dir := t.TempDir()
	onlySource := Source{
		ID: "custom-only", Name: "only", URL: "https://example.test/list",
		Format: FormatPlainList, Protocol: "http", Enabled: true,
	}
	path := writeTestPoolConfig(t, dir, PoolConfig{Sources: []Source{onlySource}})

	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Sources(); len(got) != 1 || !reflect.DeepEqual(got[0], onlySource) {
		t.Fatalf("deleted built-in was re-added or source changed: %#v", got)
	}
	if got := readTestPoolConfig(t, path).Sources; len(got) != 1 || !reflect.DeepEqual(got[0], onlySource) {
		t.Fatalf("persisted sources changed: %#v", got)
	}
}
