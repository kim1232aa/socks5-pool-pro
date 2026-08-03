package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImportSourceStoresPrivateBytesAndLoadsByKind(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewConfigStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("socks5://alice:private-value@8.8.8.8:1080\nhttp://1.1.1.1:8080\n")

	source, count, err := store.ImportSource(" local list ", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("ImportSource() error = %v", err)
	}
	if count != 2 {
		t.Fatalf("ImportSource() count = %d, want 2", count)
	}
	if source.Name != "local list" || source.Kind != SourceKindUpload || source.URL != "" || source.Format != FormatTextRegex || !source.Enabled {
		t.Fatalf("imported source = %#v", source)
	}
	if !validImportedSourceID(source.ID) {
		t.Fatalf("imported source id = %q", source.ID)
	}

	dirInfo, err := os.Stat(filepath.Join(dataDir, sourceImportDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("source import directory mode = %#o, want 0700", got)
	}
	storedPath := filepath.Join(dataDir, sourceImportDirectoryName, source.ID)
	fileInfo, err := os.Stat(storedPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("source import file mode = %#o, want 0600", got)
	}
	stored, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, payload) {
		t.Fatal("stored source bytes differ from uploaded bytes")
	}

	loaded, err := store.LoadSource(source)
	if err != nil {
		t.Fatalf("LoadSource() error = %v", err)
	}
	if len(loaded) != 2 || loaded[0].Username != "alice" || loaded[0].Password != "private-value" {
		t.Fatalf("LoadSource() returned %d unexpected proxies", len(loaded))
	}
	configBytes, err := os.ReadFile(filepath.Join(dataDir, "pool_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(configBytes, []byte("private-value")) {
		t.Fatal("proxy credentials leaked into pool_config.json")
	}

	reopened, err := NewConfigStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	restored, ok := reopened.SourceByID(source.ID)
	if !ok || restored.Kind != SourceKindUpload || restored.URL != "" {
		t.Fatalf("reopened source = %#v, found = %v", restored, ok)
	}
	if proxies, err := reopened.LoadSource(restored); err != nil || len(proxies) != 2 {
		t.Fatalf("reopened LoadSource() = %d proxies, %v", len(proxies), err)
	}
}

func TestImportSourceRejectsOversizeAndInvalidContentWithoutPersisting(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := len(store.Sources())

	if _, _, err := store.ImportSource("too large", ioByteReader(maxFetchBytes+1)); !errors.Is(err, ErrSourceImportTooLarge) {
		t.Fatalf("oversized ImportSource() error = %v", err)
	}
	if _, _, err := store.ImportSource("empty", strings.NewReader("not a proxy list")); !errors.Is(err, ErrSourceEmpty) {
		t.Fatalf("empty ImportSource() error = %v", err)
	}
	if got := len(store.Sources()); got != before {
		t.Fatalf("failed imports changed source count to %d, want %d", got, before)
	}
	entries, err := os.ReadDir(store.importDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed imports left %d stored files", len(entries))
	}
}

func TestDeleteImportedSourceRemovesPrivateFile(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := store.ImportSource("delete me", strings.NewReader("http://8.8.4.4:8080\n"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.importedSourcePath(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSource(source.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.SourceByID(source.ID); ok {
		t.Fatal("deleted imported source remains configured")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("deleted imported source file stat error = %v", err)
	}
}

func TestUploadedSourceRefreshRetainsInventoryAndSamplesChecks(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewConfigStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	source, count, err := store.ImportSource("sampled", strings.NewReader(strings.Join([]string{
		"socks5://1.1.1.1:1",
		"socks5://8.8.8.8:1",
		"http://9.9.9.9:1",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("imported count = %d, want 3", count)
	}

	pool := NewProxyPool()
	coordinator := newRefreshCoordinator()
	cfg := &Config{
		DataDir:        dataDir,
		CheckTimeout:   10 * time.Millisecond,
		MaxConcurrent:  1,
		MaxCandidates:  1,
		ScrapeInterval: time.Minute,
	}
	result := refreshSource(cfg, store, pool, coordinator, source.ID, "manual", newSourceRefreshRevision(source))
	if result.Status != "complete" {
		t.Fatalf("refreshSource() = %+v", result)
	}
	snapshot := pool.candidates.snapshot.Load()
	if snapshot == nil {
		t.Fatal("uploaded refresh did not publish a candidate snapshot")
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if got := len(snapshot.records) + len(snapshot.failedRecords); got != 3 {
		t.Fatalf("candidate inventory = %d, want all 3 imported candidates", got)
	}
	checked := 0
	for _, record := range snapshot.records {
		if record.checkedUnix != 0 {
			checked++
		}
	}
	for _, failure := range snapshot.failedRecords {
		if failure.checkedUnix != 0 {
			checked++
		}
	}
	if checked != 1 {
		t.Fatalf("checked candidates = %d, want MaxCandidates=1", checked)
	}
}

func TestConfigStoreTreatsLegacyEmptySourceKindAsRemoteWithoutRewrite(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewConfigStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	configBytes, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(configBytes, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, value := range legacy["sources"].([]any) {
		delete(value.(map[string]any), "kind")
	}
	configBytes, err = json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewConfigStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range reopened.Sources() {
		if source.Kind != "" {
			t.Fatalf("legacy source %q kind was rewritten to %q", source.ID, source.Kind)
		}
		if _, err := validateSourceDefinition(source); err != nil {
			t.Fatalf("legacy source %q was not accepted as remote: %v", source.ID, err)
		}
	}
	after, err := os.ReadFile(reopened.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, configBytes) {
		t.Fatal("legacy empty source kinds unexpectedly rewrote the config")
	}
}

func TestValidatePersistedUploadRequiresSafeGeneratedID(t *testing.T) {
	cfg := PoolConfig{Sources: []Source{{
		ID:     "../outside",
		Name:   "unsafe",
		Kind:   SourceKindUpload,
		Format: FormatTextRegex,
	}}}
	if err := validatePersistedPoolConfig(&cfg); err == nil || !strings.Contains(err.Error(), "invalid uploaded source id") {
		t.Fatalf("validatePersistedPoolConfig() error = %v", err)
	}
}

func TestNewConfigStoreRejectsSourceImportDirectorySymlink(t *testing.T) {
	dataDir := t.TempDir()
	target := filepath.Join(dataDir, "external")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dataDir, sourceImportDirectoryName)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConfigStore(dataDir); err == nil {
		t.Fatal("NewConfigStore followed a source_imports directory symlink")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("symlink target mode changed to %#o", got)
	}
}

func TestImportSourceSanitizesConfigPersistenceFailureAndCleansFile(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewConfigStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	before := store.Snapshot()
	blocker := filepath.Join(dataDir, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(blocker, "pool_config.json")

	_, _, err = store.ImportSource("storage failure", strings.NewReader("http://8.8.8.8:8080\n"))
	if !errors.Is(err, ErrSourceImportStorage) {
		t.Fatalf("ImportSource() error = %v, want storage error", err)
	}
	var persistenceErr *ConfigPersistenceError
	if !errors.As(err, &persistenceErr) {
		t.Fatalf("ImportSource() did not preserve ConfigPersistenceError: %T", err)
	}
	if strings.Contains(err.Error(), dataDir) || strings.Contains(err.Error(), blocker) {
		t.Fatalf("ImportSource() leaked a storage path: %v", err)
	}
	if got := store.Snapshot(); len(got.Sources) != len(before.Sources) {
		t.Fatal("failed import changed configured sources")
	}
	entries, err := os.ReadDir(store.importDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed import left %d uploaded source files", len(entries))
	}
}

func ioByteReader(size int) *bytes.Reader {
	return bytes.NewReader(bytes.Repeat([]byte{'x'}, size))
}
