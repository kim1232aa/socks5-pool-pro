package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestConfigStorePersistsAllowEmptyAndReturnsDeepSnapshots(t *testing.T) {
	dir := t.TempDir()
	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	added, err := store.AddSource(Source{
		Name: "authoritative-empty", URL: "https://example.com/proxies.txt", Format: FormatPlainList,
		Protocol: "http", AllowEmpty: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.mutate(func(cfg *PoolConfig) error {
		cfg.Groups = append(cfg.Groups, Group{ID: "g1", Name: "g1", Strategy: StrategyRandom, Countries: []string{"JP"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, source := range reopened.Sources() {
		if source.ID == added.ID {
			found = true
			if !source.AllowEmpty {
				t.Fatal("allow_empty was not persisted")
			}
		}
	}
	if !found {
		t.Fatal("persisted source was not restored")
	}

	snapshot := reopened.Snapshot()
	snapshot.Sources[0].Name = "mutated"
	snapshot.Groups[0].Countries[0] = "US"
	second := reopened.Snapshot()
	if second.Sources[0].Name == "mutated" || second.Groups[0].Countries[0] != "JP" {
		t.Fatalf("Snapshot retained caller aliases: %#v", second)
	}
}

func TestDefaultCheckURLUsesHTTPSWithoutMigratingSavedHTTPURL(t *testing.T) {
	dir := t.TempDir()
	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.CheckURL(); got != "https://www.google.com/generate_204" {
		t.Fatalf("default check URL = %q", got)
	}
	const customHTTP = "http://example.com/custom-health"
	if err := store.SetCheckURL(customHTTP); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.CheckURL(); got != customHTTP {
		t.Fatalf("saved HTTP check URL was migrated to %q", got)
	}
}

func TestLegacyDefaultCheckURLMigratesToHTTPS(t *testing.T) {
	dir := t.TempDir()
	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCheckURL(legacyDefaultCheckURL); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.CheckURL(); got != defaultCheckURL {
		t.Fatalf("legacy default migrated to %q, want %q", got, defaultCheckURL)
	}
}

func TestConfigStoreMutationIsCopyOnWriteWhenDiskWriteFails(t *testing.T) {
	dir := t.TempDir()
	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := store.Snapshot()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(blocker, "pool_config.json")
	_, err = store.AddSource(Source{
		Name: "must-not-stick", URL: "https://example.net/list", Format: FormatPlainList, Protocol: "http",
	})
	if err == nil {
		t.Fatal("AddSource succeeded despite an unwritable state path")
	}
	var persistenceErr *ConfigPersistenceError
	if !errors.As(err, &persistenceErr) {
		t.Fatalf("disk failure type = %T (%v), want ConfigPersistenceError", err, err)
	}
	if after := store.Snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed write mutated memory:\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestConfigStoreValidationFailureIsCopyOnWrite(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := store.Snapshot()
	err = store.mutate(func(cfg *PoolConfig) error {
		cfg.Rules = make([]Rule, maxConfigRules+1)
		return nil
	})
	if err == nil {
		t.Fatal("oversized config mutation succeeded")
	}
	var persistenceErr *ConfigPersistenceError
	if errors.As(err, &persistenceErr) {
		t.Fatalf("validation failure was misclassified as persistence: %v", err)
	}
	if after := store.Snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatal("validation failure mutated the in-memory config")
	}
}

func TestConfigStoreConflictIsNotPersistenceError(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AddSource(Source{
		Name: "duplicate", URL: "https://socks5-proxy.github.io/", Format: FormatPlainList, Protocol: "http",
	})
	if err == nil {
		t.Fatal("duplicate source URL was accepted")
	}
	var persistenceErr *ConfigPersistenceError
	if errors.As(err, &persistenceErr) {
		t.Fatalf("business conflict was misclassified as persistence: %v", err)
	}
}

func TestConfigStoreRejectsSymlinkAndOversizedStateAndTightensMode(t *testing.T) {
	valid, err := json.Marshal(defaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.json")
		if err := os.WriteFile(target, valid, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "pool_config.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := NewConfigStore(dir); err == nil {
			t.Fatal("ConfigStore followed a state-file symlink")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "pool_config.json")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxPoolConfigBytes + 1); err != nil {
			file.Close()
			t.Fatal(err)
		}
		_ = file.Close()
		if _, err := NewConfigStore(dir); err == nil {
			t.Fatal("ConfigStore accepted oversized state")
		}
	})

	t.Run("legacy mode", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "pool_config.json")
		if err := os.WriteFile(path, valid, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := NewConfigStore(dir); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("legacy config mode = %#o, want 0600", info.Mode().Perm())
		}
	})
}

func TestAtomicStateWritesIgnorePredictableTempSymlink(t *testing.T) {
	dir := t.TempDir()
	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, store.path+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCheckURL("https://example.com/health"); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "unchanged" {
		t.Fatalf("predictable temp symlink target changed to %q", contents)
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("atomic config mode = %#o, want 0600", info.Mode().Perm())
	}
}

func TestBoundedConfigDecodersRejectExpansionDuringDecode(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(`[{},{}]`))
	if _, err := decodeBoundedJSONArray[Source](decoder, 1); err == nil {
		t.Fatal("bounded source decoder accepted too many records")
	}

	var values strings.Builder
	values.WriteByte('[')
	for i := 0; i <= maxConfigListValues; i++ {
		if i != 0 {
			values.WriteByte(',')
		}
		values.WriteString(`""`)
	}
	values.WriteByte(']')
	if _, err := decodeBoundedJSONStringArray(json.NewDecoder(strings.NewReader(values.String()))); err == nil {
		t.Fatal("bounded group-list decoder accepted too many values")
	}
}

func TestPoolCacheSafeReadsValidationAndAtomicWrite(t *testing.T) {
	t.Run("legacy mode", func(t *testing.T) {
		dir := t.TempDir()
		cache := newPoolCache(dir)
		px := testProxy("socks5", "8.8.8.80", "1080", true)
		data, _ := json.Marshal(poolCacheFile{Proxies: []Proxy{px}})
		if err := os.WriteFile(cache.path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cache.path, 0o644); err != nil {
			t.Fatal(err)
		}
		forwarding, _, _ := cache.load()
		if len(forwarding) != 1 {
			t.Fatalf("legacy cache did not load: %#v", forwarding)
		}
		info, _ := os.Stat(cache.path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("legacy cache mode = %#o", info.Mode().Perm())
		}
	})

	t.Run("symlink and oversized", func(t *testing.T) {
		dir := t.TempDir()
		cache := newPoolCache(dir)
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, cache.path); err != nil {
			t.Fatal(err)
		}
		if forwarding, proxyip, stats := cache.load(); forwarding != nil || proxyip != nil || stats != nil {
			t.Fatal("pool cache followed a symlink")
		}
		if err := os.Remove(cache.path); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(cache.path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxPoolCacheBytes + 1); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		if forwarding, proxyip, stats := cache.load(); forwarding != nil || proxyip != nil || stats != nil {
			t.Fatal("pool cache accepted oversized state")
		}
	})

	t.Run("invalid protocol", func(t *testing.T) {
		cache := newPoolCache(t.TempDir())
		data, _ := json.Marshal(poolCacheFile{Proxies: []Proxy{{IP: "8.8.8.8", Port: "21", Protocol: "ftp"}}})
		if err := os.WriteFile(cache.path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if forwarding, _, _ := cache.load(); forwarding != nil {
			t.Fatalf("invalid cached protocol loaded: %#v", forwarding)
		}
	})

	t.Run("predictable temp symlink", func(t *testing.T) {
		dir := t.TempDir()
		cache := newPoolCache(dir)
		victim := filepath.Join(dir, "victim")
		if err := os.WriteFile(victim, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, cache.path+".tmp"); err != nil {
			t.Fatal(err)
		}
		cache.save(1, []Proxy{testProxy("http", "8.8.8.9", "8080", true)}, nil, nil)
		contents, _ := os.ReadFile(victim)
		if string(contents) != "unchanged" {
			t.Fatalf("pool cache temp symlink target changed to %q", contents)
		}
		info, err := os.Stat(cache.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("pool cache state mode = %#o, want 0600", info.Mode().Perm())
		}
	})
}

func TestBoundedPoolCacheDecoderStopsBeforeUnboundedSliceGrowth(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(`[{},{},{}]`))
	if _, err := decodeBoundedProxyArray(decoder, 2); err == nil {
		t.Fatal("bounded pool cache decoder accepted too many nodes")
	}

	var alternates strings.Builder
	alternates.WriteString(`[{"IP":"192.0.2.1","Port":"1080","Protocol":"socks5","credential_alternates":[`)
	for i := 0; i <= maxCredentialAlternates; i++ {
		if i != 0 {
			alternates.WriteByte(',')
		}
		alternates.WriteString(`{}`)
	}
	alternates.WriteString(`]}]`)
	if _, err := decodeBoundedProxyArray(json.NewDecoder(strings.NewReader(alternates.String())), 2); err == nil {
		t.Fatal("pool cache decoder accepted too many credential alternates")
	}
}

func TestCandidateSamplerSafeStateReads(t *testing.T) {
	t.Run("legacy mode", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, candidateSamplerStateFile)
		data := []byte(`{"version":2,"last_bucket":"alpha\u0000http","cursors":{"alpha\u0000http":"192.0.2.1\u00008080"}}`)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		sampler := newCandidateSampler(dir)
		if len(sampler.state.Cursors) != 1 {
			t.Fatalf("valid legacy sampler state was not loaded: %#v", sampler.state)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("legacy sampler mode = %#o", info.Mode().Perm())
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte(`{"version":2,"cursors":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, candidateSamplerStateFile)); err != nil {
			t.Fatal(err)
		}
		sampler := newCandidateSampler(dir)
		if sampler.state.Version != 2 || len(sampler.state.Cursors) != 0 {
			t.Fatalf("symlink sampler state loaded: %#v", sampler.state)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, candidateSamplerStateFile)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxCandidateSamplerBytes + 1); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		sampler := newCandidateSampler(dir)
		if sampler.state.Version != 2 || len(sampler.state.Cursors) != 0 {
			t.Fatalf("oversized sampler state loaded: %#v", sampler.state)
		}
	})
}
