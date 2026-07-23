package main

import (
	"encoding/json"
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
	completedFailure := testProxy("http", "9.9.9.9", "3128", false)
	completedFailure.HealthInvalidated = true
	completedFailure.PolicyExcluded = true
	publicProxyIP := Proxy{IP: "1.1.1.1", Port: "443", Protocol: "proxyip"}
	reservedProxyIP := Proxy{IP: "203.0.113.1", Port: "443", Protocol: "proxyip"}
	legacy := poolCacheFile{
		Proxies:      []Proxy{public, private, hostname, documentation, completedFailure},
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

func TestPoolCachePersistsAutomaticHealthTerminalAndCooldown(t *testing.T) {
	cache := newPoolCache(t.TempDir())
	px := testProxy("socks5", "8.8.8.91", "1080", false)
	px.HealthInvalidated = true
	want := nodeStats{
		LastLatencyMs:             91,
		ConsecutiveHealthFailures: 1,
		LastHealthSuccessAt:       time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC),
		HealthFailureTerminal:     true,
	}
	cache.save(1, []Proxy{px}, nil, map[string]nodeStats{px.Key(): want})

	forwarding, _, stats := cache.load()
	if len(forwarding) != 1 || forwarding[0].Available || !forwarding[0].HealthInvalidated || forwarding[0].PolicyExcluded {
		t.Fatalf("terminal proxy round trip = %#v", forwarding)
	}
	if got := stats[px.Key()]; got != want {
		t.Fatalf("terminal stats round trip = %+v, want %+v", got, want)
	}
}
