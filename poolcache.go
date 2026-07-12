package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	maxPoolCacheBytes        = 128 << 20
	maxPoolCacheDecodedBytes = 256 << 20
	maxPoolCacheNodes        = 500_000
	maxPoolCacheStats        = 500_000
	maxCachedProxyJSONBytes  = 1 << 20
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

	// lastGeneration prevents a snapshot captured earlier from overwriting a
	// newer one merely because its goroutine reached the filesystem later.
	// Generations are process-local; the cache file remains backward
	// compatible and does not need to persist this bookkeeping value.
	lastGeneration    uint64
	hasLastGeneration bool
}

type poolCacheFile struct {
	Proxies              []Proxy              `json:"proxies"`
	ProxyIPNodes         []Proxy              `json:"proxyip_nodes"`
	Stats                map[string]nodeStats `json:"stats,omitempty"`
	HealthCheckURL       string               `json:"health_check_url,omitempty"`
	HealthPolicy         string               `json:"health_policy,omitempty"`
	HealthRecheckPending bool                 `json:"health_recheck_pending,omitempty"`
}

func newPoolCache(dataDir string) *poolCache {
	return &poolCache{path: filepath.Join(dataDir, "pool_cache.json")}
}

func (c *poolCache) load() (forwarding, proxyip []Proxy, stats map[string]nodeStats) {
	forwarding, proxyip, stats, _ = c.loadWithHealthCriterion()
	return forwarding, proxyip, stats
}

func (c *poolCache) loadWithHealthCriterion() (forwarding, proxyip []Proxy, stats map[string]nodeStats, healthCheckURL string) {
	forwarding, proxyip, stats, healthCheckURL, _, _ = c.loadWithHealthState()
	return forwarding, proxyip, stats, healthCheckURL
}

func (c *poolCache) loadWithHealthState() (forwarding, proxyip []Proxy, stats map[string]nodeStats, healthCheckURL, healthPolicy string, healthRecheckPending bool) {
	data, err := readPrivateRegularFile(c.path, maxPoolCacheBytes)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[cache] read failed: %v", err)
		}
		return nil, nil, nil, "", "", false
	}
	data, err = decodePoolCacheBytes(data)
	if err != nil {
		log.Printf("[cache] decompress failed: %v", err)
		return nil, nil, nil, "", "", false
	}
	var f poolCacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("[cache] parse failed: %v", err)
		return nil, nil, nil, "", "", false
	}
	removedForwarding, removedProxyIP := filterNonPublicPoolCacheNodes(&f)
	if removedForwarding+removedProxyIP > 0 {
		log.Printf("[cache] skipped %d non-public forwarding and %d invalid ProxyIP endpoint(s)", removedForwarding, removedProxyIP)
	}
	if reset := normalizeCompletedCachedHealthState(&f); reset > 0 {
		log.Printf("[cache] normalized %d completed health-recheck state(s)", reset)
	}
	if err := validatePoolCacheFile(&f); err != nil {
		log.Printf("[cache] validation failed: %v", err)
		return nil, nil, nil, "", "", false
	}
	return f.Proxies, f.ProxyIPNodes, f.Stats, f.HealthCheckURL, f.HealthPolicy, f.HealthRecheckPending
}

// Older builds could persist HealthInvalidated=true after an exhaustive pass
// had already completed unsuccessfully for that node. With no recheck pending,
// that bit no longer means "waiting"; retain the failed node and its history,
// but migrate it to the ordinary unavailable state. A genuine policy exclusion
// produced by a completed current-generation check has HealthInvalidated=false
// and is deliberately left untouched.
func normalizeCompletedCachedHealthState(f *poolCacheFile) int {
	if f == nil || f.HealthRecheckPending {
		return 0
	}
	reset := 0
	for i := range f.Proxies {
		if !f.Proxies[i].HealthInvalidated {
			continue
		}
		f.Proxies[i].HealthInvalidated = false
		f.Proxies[i].PolicyExcluded = false
		f.Proxies[i].Available = false
		reset++
	}
	return reset
}

// filterNonPublicPoolCacheNodes prevents pre-upgrade private or special-use
// literals from becoming routable immediately after restart. It is deliberately
// a per-node migration: one stale row must not discard the rest of a usable
// last-known pool, and hostname upstreams retain their existing behavior.
func filterNonPublicPoolCacheNodes(f *poolCacheFile) (removedForwarding, removedProxyIP int) {
	filter := func(nodes []Proxy, proxyIP bool) ([]Proxy, int) {
		retained := nodes[:0]
		removed := 0
		for _, px := range nodes {
			ip := net.ParseIP(strings.TrimSpace(px.IP))
			drop := ip != nil && !isPublicInternetIP(ip)
			if proxyIP {
				drop = drop || ip == nil || strings.TrimSpace(px.Port) != "443"
			}
			if drop {
				removed++
				if f.Stats != nil {
					delete(f.Stats, px.Key())
				}
				continue
			}
			retained = append(retained, px)
		}
		return retained, removed
	}
	f.Proxies, removedForwarding = filter(f.Proxies, false)
	f.ProxyIPNodes, removedProxyIP = filter(f.ProxyIPNodes, true)
	return removedForwarding, removedProxyIP
}

func (c *poolCache) save(generation uint64, forwarding, proxyip []Proxy, stats map[string]nodeStats) {
	c.saveWithHealthCriterion(generation, forwarding, proxyip, stats, "")
}

func (c *poolCache) saveWithHealthCriterion(generation uint64, forwarding, proxyip []Proxy, stats map[string]nodeStats, healthCheckURL string) {
	c.saveWithHealthState(generation, forwarding, proxyip, stats, healthCheckURL, "", false)
}

func (c *poolCache) saveWithHealthState(generation uint64, forwarding, proxyip []Proxy, stats map[string]nodeStats, healthCheckURL, healthPolicy string, healthRecheckPending bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasLastGeneration && generation <= c.lastGeneration {
		return
	}
	f := poolCacheFile{
		Proxies: forwarding, ProxyIPNodes: proxyip, Stats: stats,
		HealthCheckURL: strings.TrimSpace(healthCheckURL), HealthPolicy: healthPolicy,
		HealthRecheckPending: healthRecheckPending,
	}
	if err := validatePoolCacheFile(&f); err != nil {
		log.Printf("[cache] validation failed: %v", err)
		return
	}
	data, err := json.Marshal(f)
	if err != nil {
		log.Printf("[cache] marshal failed: %v", err)
		return
	}
	if len(data) > maxPoolCacheDecodedBytes {
		log.Printf("[cache] decoded snapshot exceeds %d byte limit", maxPoolCacheDecodedBytes)
		return
	}
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		log.Printf("[cache] create compressor failed: %v", err)
		return
	}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		log.Printf("[cache] compress failed: %v", err)
		return
	}
	if err := writer.Close(); err != nil {
		log.Printf("[cache] finish compression failed: %v", err)
		return
	}
	if compressed.Len() > maxPoolCacheBytes {
		log.Printf("[cache] compressed snapshot exceeds %d byte limit", maxPoolCacheBytes)
		return
	}
	// Pool snapshots can contain upstream credentials. The shared atomic writer
	// uses a random 0600 temporary file, fsyncs it, renames it, then fsyncs the
	// directory; it never follows the legacy predictable .tmp path.
	if err := writePrivateFileAtomic(c.path, compressed.Bytes()); err != nil {
		log.Printf("[cache] write failed: %v", err)
		return
	}
	c.lastGeneration = generation
	c.hasLastGeneration = true
}

func decodePoolCacheBytes(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	decoded, readErr := io.ReadAll(io.LimitReader(reader, maxPoolCacheDecodedBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(decoded) > maxPoolCacheDecodedBytes {
		return nil, fmt.Errorf("decoded snapshot exceeds %d byte limit", maxPoolCacheDecodedBytes)
	}
	return decoded, nil
}

// UnmarshalJSON enforces collection bounds while decoding, rather than after a
// compact malicious array has already expanded into millions of Proxy values.
func (f *poolCacheFile) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("expected an object")
	}
	seenFields := make(map[string]bool, 6)
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := fieldToken.(string)
		if !ok {
			return fmt.Errorf("cache field name is not a string")
		}
		if seenFields[field] {
			return fmt.Errorf("duplicate cache field %q", field)
		}
		seenFields[field] = true
		switch field {
		case "proxies":
			f.Proxies, err = decodeBoundedProxyArray(decoder, maxPoolCacheNodes)
		case "proxyip_nodes":
			f.ProxyIPNodes, err = decodeBoundedProxyArray(decoder, maxPoolCacheNodes)
		case "stats":
			f.Stats, err = decodeBoundedStatsMap(decoder, maxPoolCacheStats)
		case "health_check_url":
			err = decoder.Decode(&f.HealthCheckURL)
		case "health_policy":
			err = decoder.Decode(&f.HealthPolicy)
		case "health_recheck_pending":
			err = decoder.Decode(&f.HealthRecheckPending)
		default:
			err = skipJSONValue(decoder)
		}
		if err != nil {
			return fmt.Errorf("cache field %q: %w", field, err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func decodeBoundedProxyArray(decoder *json.Decoder, limit int) ([]Proxy, error) {
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if opening == nil {
		return nil, nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '[' {
		return nil, fmt.Errorf("expected an array")
	}
	out := make([]Proxy, 0)
	for decoder.More() {
		if len(out) >= limit {
			return nil, fmt.Errorf("node count exceeds %d", limit)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		if len(raw) > maxCachedProxyJSONBytes {
			return nil, fmt.Errorf("node record exceeds %d bytes", maxCachedProxyJSONBytes)
		}
		if err := validateCachedProxyJSON(raw); err != nil {
			return nil, err
		}
		var px Proxy
		if err := json.Unmarshal(raw, &px); err != nil {
			return nil, err
		}
		out = append(out, px)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

func validateCachedProxyJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("node must be an object")
	}
	foundCredentials := false
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := fieldToken.(string)
		if !ok {
			return fmt.Errorf("node field name is not a string")
		}
		if !strings.EqualFold(field, "credential_alternates") {
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
			continue
		}
		if foundCredentials {
			return fmt.Errorf("duplicate credential_alternates field")
		}
		foundCredentials = true
		arrayToken, err := decoder.Token()
		if err != nil {
			return err
		}
		if arrayToken == nil {
			continue
		}
		if delimiter, ok := arrayToken.(json.Delim); !ok || delimiter != '[' {
			return fmt.Errorf("credential_alternates must be an array")
		}
		count := 0
		for decoder.More() {
			count++
			if count > maxCredentialAlternates {
				return fmt.Errorf("credential alternate count exceeds %d", maxCredentialAlternates)
			}
			var credential ProxyCredential
			if err := decoder.Decode(&credential); err != nil {
				return err
			}
			if len(credential.Username) > maxSourceCredentialBytes || len(credential.Password) > maxSourceCredentialBytes {
				return fmt.Errorf("credential alternate exceeds field limits")
			}
		}
		if _, err := decoder.Token(); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func decodeBoundedStatsMap(decoder *json.Decoder, limit int) (map[string]nodeStats, error) {
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if opening == nil {
		return nil, nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("expected an object")
	}
	out := make(map[string]nodeStats)
	records := 0
	for decoder.More() {
		records++
		if records > limit {
			return nil, fmt.Errorf("stats count exceeds %d", limit)
		}
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("stats key is not a string")
		}
		if len(key) > maxSourceProxyURLBytes || hasLogControlCharacters(key) {
			return nil, fmt.Errorf("invalid stats key")
		}
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("duplicate stats key")
		}
		var value nodeStats
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

func validatePoolCacheFile(f *poolCacheFile) error {
	if f == nil {
		return fmt.Errorf("nil cache")
	}
	if f.HealthCheckURL != "" {
		if f.HealthCheckURL != strings.TrimSpace(f.HealthCheckURL) {
			return fmt.Errorf("health check URL contains surrounding whitespace")
		}
		if err := validateCheckURL(f.HealthCheckURL); err != nil {
			return fmt.Errorf("health check URL: %w", err)
		}
	}
	if f.HealthPolicy != "" && !validHealthPolicyFingerprint(f.HealthPolicy) {
		return fmt.Errorf("unknown health policy fingerprint %q", f.HealthPolicy)
	}
	if len(f.Proxies)+len(f.ProxyIPNodes) > maxPoolCacheNodes {
		return fmt.Errorf("node count exceeds %d", maxPoolCacheNodes)
	}
	seen := make(map[string]struct{}, len(f.Proxies)+len(f.ProxyIPNodes))
	validateNodes := func(nodes []Proxy, wantProxyIP bool) error {
		for i := range nodes {
			px, ok := normalizeProxy(nodes[i])
			if !ok || (wantProxyIP && px.Protocol != "proxyip") || (!wantProxyIP && !isForwardingProtocol(px.Protocol)) {
				return fmt.Errorf("node %d has an invalid protocol/address", i)
			}
			if err := validateCachedProxy(px); err != nil {
				return fmt.Errorf("node %d: %w", i, err)
			}
			if _, exists := seen[px.Key()]; exists {
				return fmt.Errorf("duplicate node %q", px.Key())
			}
			seen[px.Key()] = struct{}{}
			nodes[i] = px
		}
		return nil
	}
	if err := validateNodes(f.Proxies, false); err != nil {
		return fmt.Errorf("forwarding cache: %w", err)
	}
	if err := validateNodes(f.ProxyIPNodes, true); err != nil {
		return fmt.Errorf("proxyip cache: %w", err)
	}
	if len(f.Stats) > maxPoolCacheStats {
		return fmt.Errorf("stats count exceeds %d", maxPoolCacheStats)
	}
	for key, stats := range f.Stats {
		if len(key) > maxSourceProxyURLBytes || hasLogControlCharacters(key) {
			return fmt.Errorf("invalid stats key")
		}
		if stats.Successes < 0 || stats.Failures < 0 || stats.LastLatencyMs < 0 || stats.ConsecutiveHealthFailures < 0 || stats.ConsecutiveHealthFailures > healthFailureThreshold {
			return fmt.Errorf("stats %q contains invalid negative/out-of-range values", key)
		}
	}
	return nil
}

func validateCachedProxy(px Proxy) error {
	if err := validateFetchedProxyFields(px); err != nil {
		return err
	}
	if len(px.SourceIDs) > maxConfiguredSources {
		return fmt.Errorf("too many source ids")
	}
	for _, id := range px.SourceIDs {
		if len(id) > maxConfigValueBytes || hasLogControlCharacters(id) {
			return fmt.Errorf("invalid source id")
		}
	}
	for _, value := range append(append([]string(nil), px.SourceNames...), px.SourceName, px.Country, px.City, px.Continent, px.ExitIP, px.Anonymity) {
		if hasLogControlCharacters(value) {
			return fmt.Errorf("metadata contains control characters")
		}
	}
	if len(px.ExitIP) > maxSourceAddressBytes || len(px.Anonymity) > 32 {
		return fmt.Errorf("exit/anonymity metadata exceeds limits")
	}
	if px.LatencyMs < 0 || px.SpeedKbps < 0 || math.IsNaN(px.SpeedKbps) || math.IsInf(px.SpeedKbps, 0) || px.SpeedTestedAt < 0 || px.SpeedBytes < 0 || px.SpeedDurationMs < 0 {
		return fmt.Errorf("measurement metadata contains invalid values")
	}
	return nil
}
