package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCandidateCatalogCacheRoundTripRetainsCredentialsAndRestoresReadiness(t *testing.T) {
	dir := t.TempDir()
	cache := newCandidateCatalogCache(dir)
	catalog := &CandidateCatalog{}
	catalog.SetDiskCache(cache)

	const (
		secretUser = "candidate-cache-user"
		secretPass = "candidate-cache-password"
	)
	labels := map[string]string{"source-a-id": "Source A"}
	proxy := Proxy{
		IP: "8.8.8.44", Port: "8080", Protocol: "http",
		Country: "JP", City: "Tokyo", Continent: "AS",
		SourceName: "source-a-id", SourceNames: []string{"source-a-id"},
		Username: secretUser, Password: secretPass,
	}
	refresh := catalog.begin([]Proxy{proxy}, labels, nil, 0)
	catalog.complete(refresh, []Proxy{proxy}, nil, nil)

	info, err := os.Stat(cache.path)
	if err != nil {
		t.Fatalf("candidate cache was not saved: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("candidate cache mode = %04o, want 0600", got)
	}
	decoded := readCandidateCacheDecoded(t, cache.path)
	if !bytes.Contains(decoded, []byte(secretUser)) || !bytes.Contains(decoded, []byte(secretPass)) {
		t.Fatal("candidate cache omitted upstream credentials")
	}

	restored := &CandidateCatalog{}
	restored.SetDiskCache(newCandidateCatalogCache(dir))
	loaded, err := restored.LoadDiskCache()
	if err != nil || !loaded {
		t.Fatalf("LoadDiskCache() = (%v, %v), want (true, nil)", loaded, err)
	}
	snapshot := restored.snapshot.Load()
	if snapshot == nil {
		t.Fatal("LoadDiskCache() did not publish a snapshot")
	}
	snapshot.mu.RLock()
	wantGeneration := snapshot.generation
	if snapshot.phase != "complete" || snapshot.revision != 2 || len(snapshot.records) != 1 {
		t.Fatalf("restored snapshot phase=%q revision=%d records=%d", snapshot.phase, snapshot.revision, len(snapshot.records))
	}
	record := snapshot.records[0]
	if !record.hasAuth || record.username != secretUser || record.password != secretPass || snapshot.countries[record.countryID] != "JP" || snapshot.cities[record.cityID] != "Tokyo" {
		t.Fatalf("restored compact record = %#v", record)
	}
	snapshot.mu.RUnlock()

	pool := NewProxyPool()
	pool.candidates = restored
	handler := NewStatusServer(pool, &ConfigStore{}).handler()
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, localTestRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("readiness after cache restore = %d body=%q", ready.Code, ready.Body.String())
	}
	page, raw := getCandidatePage(t, handler, "/api/candidates/page?page_size=100")
	if page.CandidateTotal != 1 || !page.Candidates[0].HasAuth || page.Candidates[0].Country != "JP" || page.Candidates[0].City != "Tokyo" {
		t.Fatalf("restored API page = %#v", page)
	}
	if !strings.Contains(raw, secretUser) || !strings.Contains(raw, secretPass) || page.Candidates[0].Username != secretUser || page.Candidates[0].Password != secretPass || page.Candidates[0].ProxyURL != proxy.ConsumerURL() {
		t.Fatalf("restored candidate API omitted credentials: %#v raw=%s", page.Candidates[0], raw)
	}

	next := restored.begin([]Proxy{proxy}, labels, nil, 0)
	if next.generation != wantGeneration+1 {
		t.Fatalf("next generation = %d, want %d after restore", next.generation, wantGeneration+1)
	}
}

func TestCandidateCatalogCacheRetainsOnlyFailedSourcesAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	labels := map[string]string{"source-a-id": "Source A", "source-b-id": "Source B"}
	oldA := candidateFromSource("8.8.8.60", "source-a-id", "Source A")
	oldB := candidateFromSource("8.8.8.61", "source-b-id", "Source B")
	newB := candidateFromSource("8.8.8.62", "source-b-id", "Source B")

	first := &CandidateCatalog{}
	first.SetDiskCache(newCandidateCatalogCache(dir))
	refresh := first.begin([]Proxy{oldA, oldB}, labels, nil, 0)
	first.complete(refresh, nil, nil, nil)

	restored := &CandidateCatalog{}
	restored.SetDiskCache(newCandidateCatalogCache(dir))
	loaded, err := restored.LoadDiskCache()
	if err != nil || !loaded {
		t.Fatalf("LoadDiskCache() = (%v, %v)", loaded, err)
	}
	partial := restored.begin([]Proxy{newB}, labels, map[string]bool{"source-a-id": true}, 1)
	restored.complete(partial, nil, nil, nil)

	pool := NewProxyPool()
	pool.candidates = restored
	page, _ := getCandidatePage(t, NewStatusServer(pool, &ConfigStore{}).handler(), "/api/candidates/page?page_size=100")
	keys := make(map[string]bool, len(page.Candidates))
	for _, candidate := range page.Candidates {
		keys[candidate.Key] = true
	}
	if page.Phase != "partial" || page.SourceErrors != 1 || page.CandidateTotal != 2 {
		t.Fatalf("partial restored page phase=%q errors=%d total=%d", page.Phase, page.SourceErrors, page.CandidateTotal)
	}
	if !keys[oldA.Key()] || keys[oldB.Key()] || !keys[newB.Key()] {
		t.Fatalf("source-granular restored keys = %v; want failed A retained and successful B replaced", keys)
	}
	if candidateFacetTotal(page.Sources, "Source A") != 1 || candidateFacetTotal(page.Sources, "Source B") != 1 {
		t.Fatalf("restored source facets = %#v", page.Sources)
	}
}

func TestCandidateCatalogCacheDropsNonPublicLiteralsWithoutLosingPublicOrHostnameRows(t *testing.T) {
	dir := t.TempDir()
	cache := newCandidateCatalogCache(dir)
	labels := map[string]string{"source-a-id": "Source A"}
	candidates := []Proxy{
		candidateFromSource("0.103.177.131", "source-a-id", "Source A"),
		candidateFromSource("10.0.0.1", "source-a-id", "Source A"),
		candidateFromSource("192.0.2.1", "source-a-id", "Source A"),
		candidateFromSource("8.8.8.8", "source-a-id", "Source A"),
		candidateFromSource("proxy.example.test", "source-a-id", "Source A"),
		{IP: "203.0.113.1", Port: "443", Protocol: "proxyip", SourceName: "source-a-id", SourceNames: []string{"source-a-id"}},
		{IP: "1.1.1.1", Port: "443", Protocol: "proxyip", SourceName: "source-a-id", SourceNames: []string{"source-a-id"}},
	}
	snapshot := buildCandidateSnapshot(candidates, labels)
	snapshot.generation = 1
	snapshot.revision = 1
	snapshot.phase = "complete"
	if err := cache.save(snapshot); err != nil {
		t.Fatalf("save legacy candidate cache: %v", err)
	}

	restored, err := cache.load()
	if err != nil {
		t.Fatalf("load legacy candidate cache: %v", err)
	}
	got := make(map[string]bool, len(restored.records))
	for _, record := range restored.records {
		got[restored.protocols[record.protocolID]+"://"+record.addr] = true
	}
	if len(got) != 3 || !got["http://8.8.8.8:8080"] || !got["http://proxy.example.test:8080"] || !got["proxyip://1.1.1.1:443"] {
		t.Fatalf("restored filtered records = %v, want public literal, hostname and public ProxyIP only", got)
	}
}

func TestCandidateCatalogCacheCorruptionAndOversizeAreSoftLoadFailures(t *testing.T) {
	for _, test := range []struct {
		name  string
		write func(*testing.T, string)
	}{
		{
			name: "corrupt gzip",
			write: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("not-a-candidate-cache"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "compressed file too large",
			write: func(t *testing.T, path string) {
				t.Helper()
				file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(maxCandidateCacheCompressedBytes + 1); err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			cache := newCandidateCatalogCache(dir)
			test.write(t, cache.path)
			catalog := &CandidateCatalog{}
			catalog.SetDiskCache(cache)
			loaded, err := catalog.LoadDiskCache()
			if err == nil || loaded {
				t.Fatalf("LoadDiskCache() = (%v, %v), want soft error without publication", loaded, err)
			}
			if catalog.snapshot.Load() != nil {
				t.Fatal("invalid cache partially published a snapshot")
			}
		})
	}
}

func TestCandidateCacheDecoderRejectsOversizedCountBeforeAllocation(t *testing.T) {
	var encoded [4]byte
	binary.LittleEndian.PutUint32(encoded[:], uint32(maxCandidateCacheRecords+1))
	decoder := candidateCacheDecoder{reader: bytes.NewReader(encoded[:])}
	if _, err := decoder.count(maxCandidateCacheRecords, "records"); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("oversized count error = %v", err)
	}
}

func TestCandidateCatalogCacheRejectsGenerationThatWouldWrap(t *testing.T) {
	snapshot := buildCandidateSnapshot([]Proxy{candidateFromSource("8.8.8.70", "source-a-id", "Source A")}, map[string]string{"source-a-id": "Source A"})
	snapshot.generation = ^uint64(0)
	snapshot.revision = 1
	snapshot.phase = "complete"
	if err := validateCandidateSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), "invalid generation") {
		t.Fatalf("validateCandidateSnapshot() error = %v, want generation wrap rejection", err)
	}
}

func TestCandidateCatalogCacheNormalizesPersistedGenerationAndKeepsSaving(t *testing.T) {
	dir := t.TempDir()
	cache := newCandidateCatalogCache(dir)
	labels := map[string]string{"source-a-id": "Source A"}
	proxy := candidateFromSource("8.8.8.71", "source-a-id", "Source A")
	snapshot := buildCandidateSnapshot([]Proxy{proxy}, labels)
	snapshot.generation = ^uint64(0) - 1
	snapshot.revision = 1
	snapshot.phase = "complete"
	if err := cache.save(snapshot); err != nil {
		t.Fatalf("save high-generation cache: %v", err)
	}

	catalog := &CandidateCatalog{}
	catalog.SetDiskCache(cache)
	loaded, err := catalog.LoadDiskCache()
	if err != nil || !loaded {
		t.Fatalf("LoadDiskCache() = (%v, %v), want normalized load", loaded, err)
	}
	if got := catalog.snapshot.Load().generation; got != 1 {
		t.Fatalf("restored generation = %d, want process-local generation 1", got)
	}

	for wantGeneration := uint64(2); wantGeneration <= 3; wantGeneration++ {
		refresh := catalog.begin([]Proxy{proxy}, labels, nil, 0)
		if refresh.generation != wantGeneration {
			t.Fatalf("refresh generation = %d, want %d", refresh.generation, wantGeneration)
		}
		catalog.complete(refresh, nil, nil, nil)
	}
	diskSnapshot, err := cache.load()
	if err != nil {
		t.Fatalf("load cache after two refreshes: %v", err)
	}
	if diskSnapshot.generation != 3 {
		t.Fatalf("persisted generation = %d, want latest generation 3", diskSnapshot.generation)
	}
}

func TestCandidateCatalogCacheRejectsDuplicateSourceReferences(t *testing.T) {
	proxy := candidateFromSource("8.8.8.72", "source-a-id", "Source A")
	proxy.SourceNames = []string{"source-a-id", "source-b-id"}
	snapshot := buildCandidateSnapshot([]Proxy{proxy}, map[string]string{"source-a-id": "Source A", "source-b-id": "Source B"})
	snapshot.generation = 1
	snapshot.revision = 1
	snapshot.phase = "complete"
	snapshot.sourceRefs[1] = snapshot.sourceRefs[0]
	if err := validateCandidateSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), "not strictly sorted") {
		t.Fatalf("validateCandidateSnapshot() error = %v, want repeated source reference rejection", err)
	}
}

func TestCandidateCatalogCacheRejectsUnsortedSourceReferences(t *testing.T) {
	proxy := candidateFromSource("8.8.8.73", "source-a-id", "Source A")
	proxy.SourceNames = []string{"source-a-id", "source-b-id"}
	snapshot := buildCandidateSnapshot([]Proxy{proxy}, map[string]string{"source-a-id": "Source A", "source-b-id": "Source B"})
	snapshot.generation = 1
	snapshot.revision = 1
	snapshot.phase = "complete"
	snapshot.sourceRefs[0], snapshot.sourceRefs[1] = snapshot.sourceRefs[1], snapshot.sourceRefs[0]
	if err := validateCandidateSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), "not strictly sorted") {
		t.Fatalf("validateCandidateSnapshot() error = %v, want unsorted source reference rejection", err)
	}
}

func TestCandidateCatalogCacheAllowsEmptyCompletedSnapshot(t *testing.T) {
	dir := t.TempDir()
	catalog := &CandidateCatalog{}
	catalog.SetDiskCache(newCandidateCatalogCache(dir))
	refresh := catalog.begin(nil, nil, nil, 0)
	catalog.complete(refresh, nil, nil, nil)

	restored := &CandidateCatalog{}
	restored.SetDiskCache(newCandidateCatalogCache(dir))
	loaded, err := restored.LoadDiskCache()
	if err != nil || !loaded {
		t.Fatalf("empty cache LoadDiskCache() = (%v, %v)", loaded, err)
	}
	if snapshot := restored.snapshot.Load(); snapshot == nil || len(snapshot.records) != 0 || snapshot.phase != "complete" {
		t.Fatalf("restored empty snapshot = %#v", snapshot)
	}
}

func readCandidateCacheDecoded(t *testing.T, path string) []byte {
	t.Helper()
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxCandidateCacheDecodedBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
