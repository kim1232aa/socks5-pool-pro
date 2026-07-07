package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// poolCache persists the last known-good node lists so a restart is usable
// immediately instead of blank until the first scrape+check completes
// (which can take a minute). The cache is best-effort: any read/write
// error is logged and ignored, never fatal.
//
// save() is called from more than one goroutine (the periodic refresh
// cycle and, e.g., a dashboard-triggered ClearUnavailable), so mu guards
// against two concurrent writes racing on the same tmp-file-then-rename
// path, which could otherwise let a stale write silently clobber a newer
// one.
type poolCache struct {
	mu   sync.Mutex
	path string
}

type poolCacheFile struct {
	Proxies      []Proxy              `json:"proxies"`
	ProxyIPNodes []Proxy              `json:"proxyip_nodes"`
	Stats        map[string]nodeStats `json:"stats,omitempty"`
}

func newPoolCache(dataDir string) *poolCache {
	return &poolCache{path: filepath.Join(dataDir, "pool_cache.json")}
}

func (c *poolCache) load() (forwarding, proxyip []Proxy, stats map[string]nodeStats) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[cache] read failed: %v", err)
		}
		return nil, nil, nil
	}
	var f poolCacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("[cache] parse failed: %v", err)
		return nil, nil, nil
	}
	return f.Proxies, f.ProxyIPNodes, f.Stats
}

func (c *poolCache) save(forwarding, proxyip []Proxy, stats map[string]nodeStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := poolCacheFile{Proxies: forwarding, ProxyIPNodes: proxyip, Stats: stats}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		log.Printf("[cache] marshal failed: %v", err)
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("[cache] write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("[cache] rename failed: %v", err)
	}
}
