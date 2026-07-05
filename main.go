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

	// Initial scrape + check
	refreshPool(cfg, store, pool)

	if pool.Size() == 0 {
		log.Printf("[warn] no alive proxies found, will retry on next scrape cycle")
	}

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

	seen := make(map[string]bool)
	var deduped []Proxy
	for _, px := range all {
		if seen[px.Key()] {
			continue
		}
		seen[px.Key()] = true
		deduped = append(deduped, px)
	}

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

	alive := CheckProxies(candidates, cfg.CheckTimeout, cfg.MaxConcurrent)
	pool.Update(alive)

	scrapeMu.Lock()
	lastScrapeTime = time.Now()
	nextScrapeTime = lastScrapeTime.Add(cfg.ScrapeInterval)
	scrapeMu.Unlock()

	log.Printf("[main] pool refreshed: %d alive / %d checked (from %d sources, %d raw, %d deduped)",
		pool.Size(), len(candidates), len(sources), len(all), len(deduped))
}

// TriggerRefresh sends a manual refresh signal (non-blocking).
func TriggerRefresh() {
	select {
	case refreshChan <- struct{}{}:
	default:
		// already pending
	}
}
