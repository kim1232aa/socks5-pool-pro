package main

import (
	"os"
	"sync"
	"testing"
)

func TestPoolCacheRejectsStaleGeneration(t *testing.T) {
	cache := newPoolCache(t.TempDir())
	newer := testProxy("socks5", "192.0.2.60", "1080", true)
	older := testProxy("http", "192.0.2.61", "8080", true)

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
	px := testProxy("socks5", "192.0.2.70", "1080", true)
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
	key := "socks5://192.0.2.90:1080"
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
