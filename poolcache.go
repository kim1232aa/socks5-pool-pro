package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// poolCache persists the last known-good node lists so a restart is usable
// immediately instead of blank until the first scrape+check completes
// (which can take a minute). The cache is best-effort: any read/write
// error is logged and ignored, never fatal.
type poolCache struct {
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
