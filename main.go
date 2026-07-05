package main

import (
	"log"
	"math/rand"
	"sync"
	"time"
)

var (
	lastScrapeTime time.Time
	nextScrapeTime time.Time
	scrapeMu       sync.RWMutex
	refreshChan    = make(chan struct{}, 1) // manual refresh trigger
)

func getScrapeTimes() (last, next time.Time) {
	scrapeMu.RLock()
	defer scrapeMu.RUnlock()
	return lastScrapeTime, nextScrapeTime
}

func main() {
	cfg := ParseConfig()

	store, err := NewConfigStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("[main] failed to load config: %v", err)
	}

	log.Printf("socks5-pool starting...")
	log.Printf("  listen:   %s", cfg.ListenAddr)
	log.Printf("  status:   %s", cfg.StatusAddr)
	log.Printf("  data-dir: %s", cfg.DataDir)
	log.Printf("  scrape:   every %s", cfg.ScrapeInterval)

	pool := NewProxyPool()

	// Seed from the on-disk cache so the pool is usable immediately, then
	// enable write-back so every refresh keeps the cache fresh.
	cache := newPoolCache(cfg.DataDir)
	if fwd, info, stats := cache.load(); len(fwd) > 0 || len(info) > 0 {
		pool.Prime(fwd, info)
		pool.restoreStats(stats)
	}
	pool.SetCache(cache)

	// Measure our own direct egress once, as the baseline for detecting
	// transparent proxies that don't actually change the exit IP.
	InitBaselineExit(cfg.CheckTimeout)

	// Initial scrape + check (runs in the background so the dashboard and
	// cached pool are available right away instead of blocking on it).
	go refreshPool(cfg, store, pool)

	// Background: periodic scrape + manual refresh
	go func() {
		ticker := time.NewTicker(cfg.ScrapeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				refreshPool(cfg, store, pool)
			case <-refreshChan:
				log.Printf("[main] manual refresh triggered")
				refreshPool(cfg, store, pool)
				ticker.Reset(cfg.ScrapeInterval)
			}
		}
	}()

	// Background: random rotation of the default (ANY) group every 3-6
	// minutes. If the pool is empty, trigger an immediate refresh instead.
	go func() {
		for {
			delay := 3*time.Minute + time.Duration(rand.Intn(4))*time.Minute
			time.Sleep(delay)
			if pool.Size() == 0 {
				log.Printf("[main] pool empty, triggering immediate refresh")
				TriggerRefresh()
			} else if pool.Size() > 1 {
				pool.RotateSticky(GroupAny)
			}
		}
	}()

	// Background: periodically re-check alive nodes' latency/health so the
	// latency/score strategies stay current between full refreshes.
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			reCheckAlive(cfg, pool)
		}
	}()

	// Background: status dashboard
	go func() {
		status := NewStatusServer(pool, store)
		log.Printf("[status] dashboard at http://%s", cfg.StatusAddr)
		if err := status.Start(cfg.StatusAddr); err != nil {
			log.Printf("[status] failed to start: %v", err)
		}
	}()

	// Start SOCKS5 server (blocks)
	server := NewServer(cfg.ListenAddr, pool, store)
	log.Fatal(server.Start())
}

// reCheckAlive re-probes connectivity/latency for the nodes already in the
// pool and records the outcome, so quality scores reflect ongoing health
// (not just the state at last scrape). Cheap: only touches live nodes, no
// scraping or geo lookups.
func reCheckAlive(cfg *Config, pool *ProxyPool) {
	nodes := pool.All()
	if len(nodes) == 0 {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.MaxConcurrent)
	for _, px := range nodes {
		if px.Protocol == "proxyip" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(px Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			ok := checkGoogle(px, cfg.CheckTimeout)
			latency := time.Since(start).Milliseconds()
			pool.RecordResult(px.Key(), ok, latency)
			if ok {
				pool.UpdateLatency(px.Key(), latency)
			}
		}(px)
	}
	wg.Wait()
	log.Printf("[recheck] re-probed %d live nodes", len(nodes))
}

// refreshPool fetches every enabled source concurrently, dedups the
// combined candidate list, health-checks it, and installs the result as
// the pool's new live proxy list.
func refreshPool(cfg *Config, store *ConfigStore, pool *ProxyPool) {
	sources := store.EnabledSources()
	if len(sources) == 0 {
		log.Printf("[main] no enabled sources, skipping refresh")
		return
	}

	var (
		mu  sync.Mutex
		all []Proxy
		wg  sync.WaitGroup
	)
	for _, src := range sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			proxies, err := FetchSource(src)
			if err != nil {
				log.Printf("[error] scrape %s failed: %v", src.Name, err)
				return
			}
			mu.Lock()
			all = append(all, proxies...)
			mu.Unlock()
		}(src)
	}
	wg.Wait()

	deduped := dedupeByAddr(all)

	// Some sources (e.g. large community-aggregated lists) return well
	// over 100k raw entries. Checking all of them every cycle would make
	// a refresh take hours and hammer ip-api.com's rate limit. Cap the
	// checked set and sample randomly so repeated cycles eventually cover
	// the whole source instead of always checking the same prefix.
	candidates := deduped
	if len(candidates) > cfg.MaxCandidates {
		rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
		log.Printf("[main] %d candidates exceed max-candidates=%d, sampling a random subset (rest skipped this cycle)",
			len(candidates), cfg.MaxCandidates)
		candidates = candidates[:cfg.MaxCandidates]
	}

	alive := CheckProxies(candidates, cfg.CheckTimeout, cfg.MaxConcurrent, cfg.RequireIPChange)
	pool.Update(alive)

	scrapeMu.Lock()
	lastScrapeTime = time.Now()
	nextScrapeTime = lastScrapeTime.Add(cfg.ScrapeInterval)
	scrapeMu.Unlock()

	log.Printf("[main] pool refreshed: %d alive / %d checked (from %d sources, %d raw, %d deduped)",
		pool.Size(), len(candidates), len(sources), len(all), len(deduped))
}

// dedupeByAddr collapses candidates down to one entry per ip:port,
// regardless of which protocol(s) a source claimed for it. Some
// aggregated lists tag the same physical host under two or three
// different protocols with heavy overlap (e.g. Fyvri's http/https/socks5
// files overlap 84-96% by address) - almost certainly loose upstream
// classification rather than genuinely multi-protocol hosts. Without
// this, the random max-candidates sample wastes multiple slots re-testing
// the same machine instead of spreading across distinct hosts. When a
// host appears under multiple protocols, the more specific one wins:
// socks5 > https > http > proxyip.
func dedupeByAddr(list []Proxy) []Proxy {
	rank := map[string]int{"socks5": 3, "https": 2, "http": 1, "proxyip": 0}

	best := make(map[string]Proxy, len(list))
	var order []string
	for _, px := range list {
		key := px.Addr()
		cur, ok := best[key]
		if !ok {
			best[key] = px
			order = append(order, key)
			continue
		}
		if rank[px.Protocol] > rank[cur.Protocol] {
			best[key] = px
		}
	}

	out := make([]Proxy, 0, len(order))
	for _, key := range order {
		out = append(out, best[key])
	}
	return out
}

// TriggerRefresh sends a manual refresh signal (non-blocking).
func TriggerRefresh() {
	select {
	case refreshChan <- struct{}{}:
	default:
		// already pending
	}
}
