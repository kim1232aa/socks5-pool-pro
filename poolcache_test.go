package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPoolCacheDropsNonPublicLiteralsPerNodeOnUpgrade(t *testing.T) {
	cache := newPoolCache(t.TempDir())
	public := testProxy("http", "8.8.8.8", "8080", true)
	hostname := testProxy("socks5", "proxy.example.test", "1080", true)
	private := testProxy("http", "10.0.0.1", "8080", true)
	documentation := testProxy("socks5", "192.0.2.1", "1080", true)
	unusable := testProxy("http", "1.0.0.187", "80", true)
	completedFailure := testProxy("http", "9.9.9.9", "3128", false)
	completedFailure.HealthInvalidated = true
	completedFailure.PolicyExcluded = true
	publicProxyIP := Proxy{IP: "1.1.1.1", Port: "443", Protocol: "proxyip"}
	reservedProxyIP := Proxy{IP: "203.0.113.1", Port: "443", Protocol: "proxyip"}
	legacy := poolCacheFile{
		Proxies:      []Proxy{public, private, hostname, documentation, unusable, completedFailure},
		ProxyIPNodes: []Proxy{reservedProxyIP, publicProxyIP},
		Stats: map[string]nodeStats{
			public.Key():        {Successes: 1},
			private.Key():       {Successes: 1},
			documentation.Key(): {Successes: 1},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache.path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	forwarding, proxyIP, stats := cache.load()
	if len(forwarding) != 3 || forwarding[0].Key() != public.Key() || forwarding[1].Key() != hostname.Key() || forwarding[2].Key() != completedFailure.Key() {
		t.Fatalf("filtered forwarding cache = %#v, want public literal, hostname and completed failure", forwarding)
	}
	if forwarding[2].Available || forwarding[2].HealthInvalidated || forwarding[2].PolicyExcluded {
		t.Fatalf("completed failed recheck was not normalized to ordinary unavailable: %+v", forwarding[2])
	}
	if len(proxyIP) != 1 || proxyIP[0].Key() != publicProxyIP.Key() {
		t.Fatalf("filtered ProxyIP cache = %#v, want public literal only", proxyIP)
	}
	if _, ok := stats[private.Key()]; ok {
		t.Fatalf("private node stats survived cache migration: %#v", stats)
	}
	if _, ok := stats[documentation.Key()]; ok {
		t.Fatalf("documentation node stats survived cache migration: %#v", stats)
	}
	for _, px := range forwarding {
		if px.IP == "1.0.0.187" {
			t.Fatalf("unusable 1.0.0.0/24 forwarding node survived cache migration: %#v", forwarding)
		}
	}
}

func TestPoolCacheClearsLegacyAnonymityButPreservesCurrentResults(t *testing.T) {
	legacyCache := newPoolCache(t.TempDir())
	legacyProxy := testProxy("http", "8.8.8.57", "8080", true)
	legacyProxy.Anonymity = "elite"
	legacyData, err := json.Marshal(poolCacheFile{Proxies: []Proxy{legacyProxy}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyCache.path, legacyData, 0o600); err != nil {
		t.Fatal(err)
	}
	legacyLoaded, _, _ := legacyCache.load()
	if len(legacyLoaded) != 1 || legacyLoaded[0].Anonymity != "" {
		t.Fatalf("legacy anonymity survived migration: %#v", legacyLoaded)
	}

	currentCache := newPoolCache(t.TempDir())
	currentProxy := testProxy("http", "8.8.8.56", "8080", true)
	currentProxy.Anonymity = "anonymous"
	currentCache.save(1, []Proxy{currentProxy}, nil, nil)
	currentLoaded, _, _ := currentCache.load()
	if len(currentLoaded) != 1 || currentLoaded[0].Anonymity != "anonymous" {
		t.Fatalf("current anonymity was cleared: %#v", currentLoaded)
	}
}

func TestPoolCachePersistsHealthCriterionAndTreatsLegacyAsUnknown(t *testing.T) {
	dir := t.TempDir()
	cache := newPoolCache(dir)
	px := testProxy("socks5", "8.8.8.59", "1080", true)
	cache.saveWithHealthCriterion(1, []Proxy{px}, nil, nil, defaultCheckURL)
	forwarding, _, _, criterion := cache.loadWithHealthCriterion()
	if len(forwarding) != 1 || criterion != defaultCheckURL {
		t.Fatalf("cache load = nodes=%d criterion=%q", len(forwarding), criterion)
	}

	legacy := newPoolCache(t.TempDir())
	legacy.save(1, []Proxy{px}, nil, nil)
	_, _, _, criterion = legacy.loadWithHealthCriterion()
	if criterion != "" {
		t.Fatalf("legacy cache criterion=%q, want unknown", criterion)
	}
}

func TestPoolCachePersistsIncompleteHealthRecheck(t *testing.T) {
	cache := newPoolCache(t.TempDir())
	px := testProxy("http", "8.8.8.58", "8080", false)
	policy := healthPolicyFingerprint(true)
	cache.saveWithHealthState(1, []Proxy{px}, nil, nil, defaultCheckURL, policy, true)
	forwarding, _, _, criterion, loadedPolicy, pending := cache.loadWithHealthState()
	if len(forwarding) != 1 || criterion != defaultCheckURL || loadedPolicy != policy || !pending {
		t.Fatalf("health state = nodes=%d criterion=%q policy=%q pending=%v", len(forwarding), criterion, loadedPolicy, pending)
	}
}

func TestPoolCacheRejectsStaleGeneration(t *testing.T) {
	cache := newPoolCache(t.TempDir())
	newer := testProxy("socks5", "8.8.8.60", "1080", true)
	older := testProxy("http", "8.8.8.61", "8080", true)

	cache.save(2, []Proxy{newer}, nil, map[string]nodeStats{
		newer.Key(): nodeStats{Successes: 2},
	})
	cache.save(1, []Proxy{older}, nil, map[string]nodeStats{
		older.Key(): nodeStats{Failures: 1},
	})

	forwarding, _, stats := cache.load()
	if len(forwarding) != 1 || forwarding[0].Key() != newer.Key() {
		t.Fatalf("stale snapshot overwrote newer cache: %+v", forwarding)
	}
	if stats[newer.Key()].Successes != 2 {
		t.Fatalf("newer stats were overwritten: %+v", stats)
	}
}

func TestPoolCacheSnapshotIsRaceSafeDuringMutations(t *testing.T) {
	cache := newPoolCache(t.TempDir())
	p := NewProxyPool()
	p.persistDebounce = defaultPoolPersistDebounce
	px := testProxy("socks5", "8.8.8.70", "1080", true)
	px.SourceNames = []string{"source-a", "source-b"}
	p.Prime([]Proxy{px}, nil)
	p.SetCache(cache)
	// Prime and read APIs must not retain nested slice aliases either.
	px.SourceNames[0] = "mutated-caller"
	readCopy := p.All()
	readCopy[0].SourceNames[0] = "mutated-reader"
	if got := p.All()[0].SourceNames[0]; got != "source-a" {
		t.Fatalf("pool SourceNames aliased external memory: %q", got)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for n := 1; n <= 100; n++ {
				p.UpdateLatency(px.Key(), int64(offset*1000+n))
				p.RecordResult(px.Key(), n%2 == 0, int64(n))
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			p.FlushCache()
		}
	}()
	wg.Wait()
	p.FlushCache()

	forwarding, _, stats := cache.load()
	if len(forwarding) != 1 {
		t.Fatalf("cached forwarding nodes=%d, want 1", len(forwarding))
	}
	st := stats[px.Key()]
	if st.Successes+st.Failures != 400 {
		t.Fatalf("cached observations=%d, want 400", st.Successes+st.Failures)
	}
}

func TestPoolCacheHealthFailureStreakIsBackwardCompatibleAndPersistent(t *testing.T) {
	dir := t.TempDir()
	cache := newPoolCache(dir)
	key := "socks5://8.8.8.90:1080"
	legacy := `{"stats":{"` + key + `":{"successes":4,"failures":2,"last_latency_ms":91}}}`
	if err := os.WriteFile(cache.path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, stats := cache.load()
	if got := stats[key]; got.Successes != 4 || got.Failures != 2 || got.LastLatencyMs != 91 || got.ConsecutiveHealthFailures != 0 {
		t.Fatalf("legacy stats did not decode with a zero health streak: %+v", got)
	}

	want := nodeStats{Successes: 4, Failures: 4, LastLatencyMs: 91, ConsecutiveHealthFailures: 2}
	cache.save(1, nil, nil, map[string]nodeStats{key: want})
	_, _, stats = cache.load()
	if got := stats[key]; got != want {
		t.Fatalf("persisted health streak = %+v, want %+v", got, want)
	}
}

func TestPoolCacheDropsAutomaticHealthTerminalFailures(t *testing.T) {
	cache := newPoolCache(t.TempDir())
	px := testProxy("socks5", "8.8.8.91", "1080", false)
	px.HealthInvalidated = true
	keep := testProxy("http", "8.8.8.92", "8080", true)
	want := nodeStats{
		LastLatencyMs:             91,
		ConsecutiveHealthFailures: 1,
		LastHealthSuccessAt:       time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC),
		HealthFailureTerminal:     true,
	}
	cache.save(1, []Proxy{px, keep}, nil, map[string]nodeStats{px.Key(): want, keep.Key(): {Successes: 1}})

	forwarding, _, stats := cache.load()
	if len(forwarding) != 1 || forwarding[0].Key() != keep.Key() {
		t.Fatalf("terminal failed node survived cache load: %#v", forwarding)
	}
	if _, ok := stats[px.Key()]; ok {
		t.Fatalf("terminal stats survived cache load: %+v", stats[px.Key()])
	}
}

func TestPoolCacheReadsLegacyGZIPDocument(t *testing.T) {
	cache := newPoolCache(t.TempDir())
	px := testProxy("socks5", "8.8.8.92", "1080", true)
	legacy := poolCacheFile{
		Proxies:        []Proxy{px},
		Stats:          map[string]nodeStats{px.Key(): {Successes: 3, LastLatencyMs: 92}},
		HealthCheckURL: defaultCheckURL,
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache.path, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	forwarding, _, stats, criterion := cache.loadWithHealthCriterion()
	if len(forwarding) != 1 || forwarding[0].Key() != px.Key() {
		t.Fatalf("legacy gzip forwarding cache = %#v", forwarding)
	}
	if got := stats[px.Key()]; got.Successes != 3 || got.LastLatencyMs != 92 {
		t.Fatalf("legacy gzip stats = %+v", got)
	}
	if criterion != defaultCheckURL {
		t.Fatalf("legacy gzip health criterion = %q, want %q", criterion, defaultCheckURL)
	}
}

type boundedPoolCacheTestWriter struct {
	maxWrite int
	written  int64
}

func (w *boundedPoolCacheTestWriter) Write(data []byte) (int, error) {
	if len(data) > w.maxWrite {
		return 0, fmt.Errorf("single write of %d bytes exceeds %d", len(data), w.maxWrite)
	}
	w.written += int64(len(data))
	return len(data), nil
}

func TestEncodePoolCacheGZIPStreamsCompressedOutput(t *testing.T) {
	f := benchmarkPoolCacheFile(10_000)
	decodedOutput := &boundedPoolCacheTestWriter{maxWrite: maxCachedProxyJSONBytes}
	if err := encodePoolCacheJSON(decodedOutput, &f); err != nil {
		t.Fatalf("stream cache JSON: %v", err)
	}
	if decodedOutput.written <= int64(decodedOutput.maxWrite) {
		t.Fatalf("JSON fixture wrote only %d bytes; test did not exercise chunked output", decodedOutput.written)
	}

	output := &boundedPoolCacheTestWriter{maxWrite: 64 << 10}
	if err := encodePoolCacheGZIP(output, &f); err != nil {
		t.Fatalf("stream cache gzip: %v", err)
	}
	if output.written <= int64(output.maxWrite) {
		t.Fatalf("compressed fixture wrote only %d bytes; test did not exercise chunked output", output.written)
	}

	var compressed bytes.Buffer
	if err := encodePoolCacheGZIP(&compressed, &f); err != nil {
		t.Fatalf("encode cache gzip: %v", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, want) {
		t.Fatal("streamed cache JSON differs from the legacy json.Marshal document")
	}
}

func BenchmarkEncodePoolCacheGZIP(b *testing.B) {
	for _, nodes := range []int{100_000, 500_000} {
		b.Run(fmt.Sprintf("nodes-%d", nodes), func(b *testing.B) {
			f := benchmarkPoolCacheFile(nodes)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := encodePoolCacheGZIP(io.Discard, &f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkPoolCacheFile(nodes int) poolCacheFile {
	proxies := make([]Proxy, nodes)
	stats := make(map[string]nodeStats, nodes)
	for i := range proxies {
		proxies[i] = Proxy{
			IP:         fmt.Sprintf("proxy-%06d.example.test", i),
			Port:       "1080",
			Protocol:   "socks5",
			SourceName: fmt.Sprintf("source-%06d", i),
			Available:  true,
		}
		stats[proxies[i].Key()] = nodeStats{Successes: i % 17, LastLatencyMs: int64(i%1000 + 1)}
	}
	return poolCacheFile{Proxies: proxies, Stats: stats}
}
