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
	"sort"
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

// normalizeCompletedCachedHealthState distinguishes the old transient
// HealthInvalidated marker from the terminal state now persisted in nodeStats.
// Old caches decode the new terminal field as false and retain their historical
// normalization; terminal failures keep the hard-routing mirror across restart.
func normalizeCompletedCachedHealthState(f *poolCacheFile) int {
	if f == nil {
		return 0
	}
	reset := 0
	for i := range f.Proxies {
		stats := f.Stats[f.Proxies[i].Key()]
		if stats.HealthFailureTerminal {
			f.Proxies[i].HealthInvalidated = true
			f.Proxies[i].PolicyExcluded = false
			f.Proxies[i].Available = false
			continue
		}
		if f.HealthRecheckPending || !f.Proxies[i].HealthInvalidated {
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
	_ = c.saveWithHealthCriterion(generation, forwarding, proxyip, stats, "")
}

func (c *poolCache) saveWithHealthCriterion(generation uint64, forwarding, proxyip []Proxy, stats map[string]nodeStats, healthCheckURL string) error {
	return c.saveWithHealthState(generation, forwarding, proxyip, stats, healthCheckURL, "", false)
}

func (c *poolCache) saveWithHealthState(generation uint64, forwarding, proxyip []Proxy, stats map[string]nodeStats, healthCheckURL, healthPolicy string, healthRecheckPending bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasLastGeneration && generation <= c.lastGeneration {
		return nil
	}
	f := poolCacheFile{
		Proxies: forwarding, ProxyIPNodes: proxyip, Stats: stats,
		HealthCheckURL: strings.TrimSpace(healthCheckURL), HealthPolicy: healthPolicy,
		HealthRecheckPending: healthRecheckPending,
	}
	if err := validatePoolCacheFile(&f); err != nil {
		return fmt.Errorf("validate pool cache: %w", err)
	}
	// Encode directly through gzip into the atomic temporary file. This avoids
	// retaining both the complete JSON document and a complete compressed copy
	// alongside the already-copied pool snapshot.
	if err := writePoolCacheAtomic(c.path, &f); err != nil {
		return fmt.Errorf("write pool cache: %w", err)
	}
	c.lastGeneration = generation
	c.hasLastGeneration = true
	return nil
}

type poolCacheSizeWriter struct {
	writer  io.Writer
	written int64
	limit   int64
	label   string
}

func (w *poolCacheSizeWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.limit-w.written {
		return 0, fmt.Errorf("%s exceeds %d byte limit", w.label, w.limit)
	}
	n, err := w.writer.Write(data)
	w.written += int64(n)
	return n, err
}

type poolCacheJSONStream struct {
	output  io.Writer
	buffer  bytes.Buffer
	encoder *json.Encoder
}

func newPoolCacheJSONStream(output io.Writer) *poolCacheJSONStream {
	stream := &poolCacheJSONStream{output: output}
	stream.encoder = json.NewEncoder(&stream.buffer)
	return stream
}

func (s *poolCacheJSONStream) writeString(value string) error {
	_, err := io.WriteString(s.output, value)
	return err
}

// encodeValue uses Encoder for one bounded record at a time. Encoder appends a
// newline, which is removed to retain the compact json.Marshal wire format.
func (s *poolCacheJSONStream) encodeValue(value any) error {
	s.buffer.Reset()
	if err := s.encoder.Encode(value); err != nil {
		return err
	}
	encoded := s.buffer.Bytes()
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		return fmt.Errorf("JSON encoder omitted record terminator")
	}
	_, err := s.output.Write(encoded[:len(encoded)-1])
	return err
}

func (s *poolCacheJSONStream) encodeProxies(proxies []Proxy) error {
	if proxies == nil {
		return s.writeString("null")
	}
	if err := s.writeString("["); err != nil {
		return err
	}
	for i := range proxies {
		if i > 0 {
			if err := s.writeString(","); err != nil {
				return err
			}
		}
		if err := s.encodeValue(&proxies[i]); err != nil {
			return err
		}
	}
	return s.writeString("]")
}

func (s *poolCacheJSONStream) encodeStats(stats map[string]nodeStats) error {
	if err := s.writeString("{"); err != nil {
		return err
	}
	keys := make([]string, 0, len(stats))
	for key := range stats {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		if i > 0 {
			if err := s.writeString(","); err != nil {
				return err
			}
		}
		if err := s.encodeValue(key); err != nil {
			return err
		}
		if err := s.writeString(":"); err != nil {
			return err
		}
		if err := s.encodeValue(stats[key]); err != nil {
			return err
		}
	}
	return s.writeString("}")
}

func encodePoolCacheJSON(output io.Writer, f *poolCacheFile) error {
	stream := newPoolCacheJSONStream(output)
	if err := stream.writeString(`{"proxies":`); err != nil {
		return err
	}
	if err := stream.encodeProxies(f.Proxies); err != nil {
		return err
	}
	if err := stream.writeString(`,"proxyip_nodes":`); err != nil {
		return err
	}
	if err := stream.encodeProxies(f.ProxyIPNodes); err != nil {
		return err
	}
	if len(f.Stats) > 0 {
		if err := stream.writeString(`,"stats":`); err != nil {
			return err
		}
		if err := stream.encodeStats(f.Stats); err != nil {
			return err
		}
	}
	if f.HealthCheckURL != "" {
		if err := stream.writeString(`,"health_check_url":`); err != nil {
			return err
		}
		if err := stream.encodeValue(f.HealthCheckURL); err != nil {
			return err
		}
	}
	if f.HealthPolicy != "" {
		if err := stream.writeString(`,"health_policy":`); err != nil {
			return err
		}
		if err := stream.encodeValue(f.HealthPolicy); err != nil {
			return err
		}
	}
	if f.HealthRecheckPending {
		if err := stream.writeString(`,"health_recheck_pending":true`); err != nil {
			return err
		}
	}
	return stream.writeString("}")
}

func encodePoolCacheGZIP(output io.Writer, f *poolCacheFile) error {
	compressed := &poolCacheSizeWriter{
		writer: output,
		limit:  maxPoolCacheBytes,
		label:  "compressed snapshot",
	}
	writer, err := gzip.NewWriterLevel(compressed, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("create compressor: %w", err)
	}
	decoded := &poolCacheSizeWriter{
		writer: writer,
		limit:  maxPoolCacheDecodedBytes,
		label:  "decoded snapshot",
	}
	if err := encodePoolCacheJSON(decoded, f); err != nil {
		_ = writer.Close()
		return fmt.Errorf("encode pool cache: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish compression: %w", err)
	}
	return nil
}

// writePoolCacheAtomic mirrors the shared private atomic writer while allowing
// the cache encoder to stream directly into its random 0600 temporary file.
func writePoolCacheAtomic(path string, f *poolCacheFile) (returnErr error) {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("persisted state path must not be empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		if returnErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if err := encodePoolCacheGZIP(temp, f); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
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
		if stats.Successes < 0 || stats.Failures < 0 || stats.LastLatencyMs < 0 || stats.ConsecutiveHealthFailures < 0 {
			return fmt.Errorf("stats %q contains invalid negative values", key)
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
