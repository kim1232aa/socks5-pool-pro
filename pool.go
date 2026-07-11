package main

import (
	"log"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reserved group names.
const (
	GroupDirect = "DIRECT" // bypass proxying, dial the target directly
	GroupAny    = "ANY"    // the entire live forwarding pool
)

// Load-balancing strategies a Group can use to pick among its members.
const (
	StrategySticky     = "sticky"      // stay on one proxy until it's manually switched or fails
	StrategyRoundRobin = "round-robin" // rotate on every new connection
	StrategyRandom     = "random"      // pick uniformly at random on every new connection
	StrategyLatency    = "latency"     // prefer the lowest measured health-check latency
	StrategySpeed      = "speed"       // prefer the highest on-demand speed-test throughput
	StrategyScore      = "score"       // prefer the highest composite quality score
)

// nodeStats accumulates observed reliability for one node across
// connections and background re-checks, keyed by Proxy.Key().
type nodeStats struct {
	Successes                 int   `json:"successes"`
	Failures                  int   `json:"failures"`
	LastLatencyMs             int64 `json:"last_latency_ms"`
	ConsecutiveHealthFailures int   `json:"consecutive_health_failures,omitempty"`
}

// Background checks are deliberately tolerant of brief DNS, scheduler, and
// upstream stalls. Live client dials and explicit policy decisions still take
// effect immediately through SetAvailable.
const healthFailureThreshold = 3

// Group is a user-defined named subset of the pool plus a selection
// strategy. GroupAny and GroupDirect are reserved built-ins that always
// exist implicitly and never appear in the persisted Groups list.
type Group struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Strategy  string   `json:"strategy"`
	Countries []string `json:"countries,omitempty"` // country code allowlist; empty = any
	Protocols []string `json:"protocols,omitempty"` // protocol allowlist; empty = any
	Sources   []string `json:"sources,omitempty"`   // source-name allowlist; empty = any
	// Nodes is an explicit allowlist of protocol-aware node keys
	// ("socks5://ip:port", "http://ip:port", ...). When set, only these
	// exact nodes are group members - this is how you pin a rule (e.g. a
	// domain) to one specific node. Legacy "ip:port" values remain
	// supported and intentionally match every protocol at that address.
	// Empty = any node (subject to the other filters).
	Nodes []string `json:"nodes,omitempty"`
}

type groupCursor struct {
	stickyKey  string
	rrIdx      int
	lastPicked string
	// pinned marks a manual lock: the user explicitly chose this node from
	// the dashboard, so the periodic auto-rotation must leave it alone until
	// the lock is cleared (SetAuto). Only meaningful for sticky groups.
	pinned bool
}

// countryGroupPrefix marks a dynamic routing target that resolves to "any
// live node whose real exit is in this ISO country", e.g. "COUNTRY:US".
// Unlike a named Group it needs no pre-creation, so a routing rule can point
// a domain straight at a country (DOMAIN-SUFFIX com -> COUNTRY:US, DOMAIN
// 111.com -> COUNTRY:JP).
const countryGroupPrefix = "COUNTRY:"

// parseCountryGroup returns the ISO country code of a "COUNTRY:XX" dynamic
// group target, or ok=false for any other group name. The prefix match is
// case-insensitive - AddRule/SetDefaultGroup rely on this to recognize and
// canonicalize any casing a caller submits (e.g. a direct API call with
// "country:us"), and resolveGroup relies on it to recognize whatever ended
// up persisted. A case-sensitive check here would make those callers'
// upper-casing normalization never even fire for a fully-lowercase input,
// since it's gated on this function returning ok=true in the first place.
func parseCountryGroup(name string) (code string, ok bool) {
	if len(name) < len(countryGroupPrefix) || !strings.EqualFold(name[:len(countryGroupPrefix)], countryGroupPrefix) {
		return "", false
	}
	if cc := strings.TrimSpace(name[len(countryGroupPrefix):]); cc != "" {
		return cc, true
	}
	return "", false
}

// ProxyPool holds the live, health-checked node list plus per-group
// selection state.
type ProxyPool struct {
	mu              sync.RWMutex
	proxies         []Proxy           // forwarding-capable (socks5/http/https)
	proxyIndex      map[string]int    // protocol-aware key -> first index in proxies
	proxyIPNodes    []Proxy           // informational-only "proxyip" nodes (see parser.go)
	candidates      *CandidateCatalog // full, non-routable source inventory
	groupState      map[string]*groupCursor
	stats           map[string]*nodeStats // keyed by Proxy.Key()
	cache           *poolCache
	cacheGeneration uint64
	persistTimer    *time.Timer
	persistToken    uint64
	persistDebounce time.Duration
	recheckCursor   string
}

const defaultPoolPersistDebounce = 500 * time.Millisecond

func NewProxyPool() *ProxyPool {
	return &ProxyPool{
		candidates:      &CandidateCatalog{},
		groupState:      make(map[string]*groupCursor),
		stats:           make(map[string]*nodeStats),
		proxyIndex:      make(map[string]int),
		persistDebounce: defaultPoolPersistDebounce,
	}
}

// RecordResult logs the outcome of using a node (from the SOCKS5 server or
// a background re-check) so its quality score reflects real reliability.
func (p *ProxyPool) RecordResult(key string, ok bool, latencyMs int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.statsForKeyLocked(key)
	if ok {
		st.Successes++
		st.ConsecutiveHealthFailures = 0
		if latencyMs > 0 {
			st.LastLatencyMs = latencyMs
		}
	} else {
		st.Failures++
	}
	p.queuePersistenceLocked()
}

func (p *ProxyPool) statsForKeyLocked(key string) *nodeStats {
	st := p.stats[key]
	if st == nil {
		st = &nodeStats{}
		p.stats[key] = st
	}
	return st
}

// ObserveHealthResult atomically applies one health observation shared by
// periodic background checks and bounded manual verification. A success
// immediately restores routing eligibility and clears the failure streak;
// transient failures remain visible in reliability stats but only the third
// consecutive observation marks the known node unavailable.
//
// This intentionally counts Successes/Failures exactly as the former
// RecordResult call in reCheckAlive did, preserving score semantics while
// replacing three independently locked mutations with one.
func (p *ProxyPool) ObserveHealthResult(key string, ok bool, latencyMs int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	observed := p.mutateProxyLocked(key, func(px *Proxy) {
		st := p.statsForKeyLocked(key)
		if ok {
			st.Successes++
			st.ConsecutiveHealthFailures = 0
			if latencyMs > 0 {
				st.LastLatencyMs = latencyMs
			}
			px.Available = true
			px.LatencyMs = latencyMs
			return
		}

		st.Failures++
		if st.ConsecutiveHealthFailures < healthFailureThreshold {
			st.ConsecutiveHealthFailures++
		}
		if st.ConsecutiveHealthFailures >= healthFailureThreshold {
			px.Available = false
		}
	})
	if observed {
		p.queuePersistenceLocked()
	}
	return observed
}

// scoreLocked computes a 0-100 quality score for px from its stats,
// latency, and speed. Caller must hold p.mu. Weights success-rate most,
// then latency, then measured speed. Nodes with no observations yet get a
// neutral success-rate so they aren't unfairly buried.
func (p *ProxyPool) scoreLocked(px Proxy) float64 {
	st := p.stats[px.Key()]

	successRate := 0.75 // neutral-ish prior for unobserved nodes
	if st != nil {
		total := st.Successes + st.Failures
		if total > 0 {
			successRate = float64(st.Successes) / float64(total)
		}
	}

	lat := px.LatencyMs
	if st != nil && st.LastLatencyMs > 0 {
		lat = st.LastLatencyMs
	}
	latScore := 1.0
	if lat > 0 {
		latScore = 1.0 / (1.0 + float64(lat)/1000.0) // 0ms→1, 1s→0.5
	}

	speedScore := 0.0
	if px.SpeedKbps > 0 {
		speedScore = math.Min(px.SpeedKbps/10000.0, 1.0) // cap at ~10 Mbps
	}

	return 100 * (0.6*successRate + 0.3*latScore + 0.1*speedScore)
}

// Score returns the quality score for a node (0 if unknown).
func (p *ProxyPool) Score(px Proxy) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.scoreLocked(px)
}

// StatsOf returns the accumulated success/failure counts for a node.
func (p *ProxyPool) StatsOf(key string) (successes, failures int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if st := p.stats[key]; st != nil {
		return st.Successes, st.Failures
	}
	return 0, 0
}

// HealthStateOf returns the current routing eligibility and the debounced
// consecutive health-failure streak for one protocol-aware node key.
func (p *ProxyPool) HealthStateOf(key string) (available bool, consecutiveFailures int, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	index, ok := p.proxyIndexLookupLocked(key)
	if !ok {
		return false, 0, false
	}
	if st := p.stats[key]; st != nil {
		consecutiveFailures = st.ConsecutiveHealthFailures
	}
	return p.proxies[index].Available, consecutiveFailures, true
}

// rebuildProxyIndexLocked rebuilds the protocol-aware key -> slice-index map.
// Caller must hold p.mu for writing whenever p.proxies may be replaced. Keep
// the first occurrence for defensive compatibility with legacy/corrupt cache
// data: Find and the former linear mutation scan both returned the first
// matching entry if duplicate keys somehow made it into the slice.
func (p *ProxyPool) rebuildProxyIndexLocked() {
	index := make(map[string]int, len(p.proxies))
	for i := range p.proxies {
		key := p.proxies[i].Key()
		if _, exists := index[key]; !exists {
			index[key] = i
		}
	}
	p.proxyIndex = index
}

// proxyIndexLookupLocked returns the indexed location only when the map and
// slice still agree. Callers may hold either p.mu for reading or writing.
func (p *ProxyPool) proxyIndexLookupLocked(key string) (int, bool) {
	i, ok := p.proxyIndex[key]
	if !ok || i < 0 || i >= len(p.proxies) || p.proxies[i].Key() != key {
		return 0, false
	}
	return i, true
}

// mutateProxy finds the node matching key and applies fn to it in place.
// Caller must hold p.mu. Shared by every "look up one node by Key() and
// change one field" operation (UpdateLatency/SetAvailable/UpdateGeo/
// UpdateSpeed). Normal lookups are O(1); the linear fallback only protects
// callers that constructed a zero-value pool directly or supplied stale
// legacy state outside the normal Prime/Update/ClearUnavailable lifecycle.
func (p *ProxyPool) mutateProxyLocked(key string, fn func(*Proxy)) bool {
	if i, ok := p.proxyIndexLookupLocked(key); ok {
		fn(&p.proxies[i])
		return true
	}
	for i := range p.proxies {
		if p.proxies[i].Key() == key {
			fn(&p.proxies[i])
			p.rebuildProxyIndexLocked()
			return true
		}
	}
	return false
}

// UpdateLatency refreshes a live node's measured latency (from a
// background re-check) so latency/score strategies stay current.
func (p *ProxyPool) UpdateLatency(key string, latencyMs int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mutateProxyLocked(key, func(px *Proxy) { px.LatencyMs = latencyMs }) {
		p.queuePersistenceLocked()
	}
}

// SetAvailable flips a node's Available flag from the lightweight periodic
// re-check (every -recheck-interval, cheaper and more frequent than a full
// scrape). This never removes the node - it only affects whether it's
// preferred for routing and whether the dashboard hides it by default.
func (p *ProxyPool) SetAvailable(key string, available bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mutateProxyLocked(key, func(px *Proxy) { px.Available = available }) {
		if available {
			if st := p.stats[key]; st != nil {
				st.ConsecutiveHealthFailures = 0
			}
		}
		p.queuePersistenceLocked()
	}
}

// queuePersistenceLocked marks the in-memory state as newer and schedules a
// single delayed write. High-frequency rechecks can update thousands of nodes;
// batching them avoids turning every individual field mutation into a blocking
// disk write. Caller must hold p.mu.
func (p *ProxyPool) queuePersistenceLocked() {
	p.cacheGeneration++
	if p.cache == nil {
		return
	}
	delay := p.persistDebounce
	if delay <= 0 {
		delay = defaultPoolPersistDebounce
	}
	if p.persistTimer != nil {
		p.persistTimer.Stop()
	}
	p.persistToken++
	token := p.persistToken
	p.persistTimer = time.AfterFunc(delay, func() {
		p.flushScheduledPersistence(token)
	})
}

// cacheSnapshotLocked returns detached data: neither slice shares its backing
// array with the live pool, and the stats map contains values rather than the
// pool's mutable pointers. Caller must hold p.mu (read or write).
func (p *ProxyPool) cacheSnapshotLocked() (uint64, []Proxy, []Proxy, map[string]nodeStats) {
	forwarding := cloneProxySlice(p.proxies)
	proxyip := cloneProxySlice(p.proxyIPNodes)
	stats := make(map[string]nodeStats, len(p.stats))
	for key, st := range p.stats {
		if st != nil {
			stats[key] = *st
		}
	}
	return p.cacheGeneration, forwarding, proxyip, stats
}

func cloneProxySlice(in []Proxy) []Proxy {
	out := make([]Proxy, len(in))
	for i, px := range in {
		out[i] = cloneProxy(px)
	}
	return out
}

func cloneProxy(px Proxy) Proxy {
	px.SourceNames = append([]string(nil), px.SourceNames...)
	return px
}

func (p *ProxyPool) cancelScheduledPersistenceLocked() {
	p.persistToken++
	if p.persistTimer != nil {
		p.persistTimer.Stop()
		p.persistTimer = nil
	}
}

func (p *ProxyPool) flushScheduledPersistence(token uint64) {
	p.mu.Lock()
	if p.persistTimer == nil || token != p.persistToken {
		p.mu.Unlock()
		return
	}
	p.persistTimer = nil
	cache := p.cache
	if cache == nil {
		p.mu.Unlock()
		return
	}
	generation, forwarding, proxyip, stats := p.cacheSnapshotLocked()
	p.mu.Unlock()
	cache.save(generation, forwarding, proxyip, stats)
}

// FlushCache synchronously persists a detached snapshot. Normal state changes
// use the debounced writer above; this hook cancels its pending timer and is
// useful at explicit durability boundaries and in tests.
func (p *ProxyPool) FlushCache() {
	p.mu.Lock()
	p.cancelScheduledPersistenceLocked()
	cache := p.cache
	if cache == nil {
		p.mu.Unlock()
		return
	}
	generation, forwarding, proxyip, stats := p.cacheSnapshotLocked()
	p.mu.Unlock()
	cache.save(generation, forwarding, proxyip, stats)
}

// statsSnapshot / restoreStats support persisting scores across restarts.
func (p *ProxyPool) statsSnapshot() map[string]nodeStats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]nodeStats, len(p.stats))
	for k, v := range p.stats {
		out[k] = *v
	}
	return out
}

func (p *ProxyPool) restoreStats(m map[string]nodeStats) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range m {
		vv := v
		p.stats[k] = &vv
	}
}

// SetCache enables persistence of the live pool to disk (see poolcache.go).
func (p *ProxyPool) SetCache(c *poolCache) {
	p.mu.Lock()
	if c == nil {
		p.cancelScheduledPersistenceLocked()
	}
	p.cache = c
	p.mu.Unlock()
}

// Prime seeds the pool from cached nodes at startup, so it's usable before
// the first scrape completes. Does not write back to the cache.
func (p *ProxyPool) Prime(forwarding, proxyip []Proxy) {
	p.mu.Lock()
	p.proxies = cloneProxySlice(forwarding)
	p.rebuildProxyIndexLocked()
	p.proxyIPNodes = cloneProxySlice(proxyip)
	p.mu.Unlock()
	log.Printf("[pool] primed from cache: %d forwarding, %d proxyip nodes", len(forwarding), len(proxyip))
}

// Update merges this cycle's health-check results into the live pool,
// splitting out info-only proxyip nodes. Nodes are identified by Proxy.Key()
// (protocol + address) and are never dropped here; the same endpoint can
// legitimately expose HTTP and SOCKS independently:
//   - an address present in freshlyAlive is marked Available=true and gets
//     its newly-observed connection data, while on-demand speed results and
//     trustworthy exit/geo metadata survive a partial probe result.
//   - an already-known address in failedAddrs (dialed and genuinely failed to
//     connect - see CheckProxies) increments its background failure streak and
//     is marked Available=false only at healthFailureThreshold. A failed new
//     candidate is absent from the merged pool and therefore is not admitted.
//     A node that was dialed successfully but excluded from freshlyAlive for a
//     policy reason (transparent proxy, blocked country) is neither here nor
//     in freshlyAlive, so it's left untouched by the next branch.
//   - an address in neither (deferred this cycle by bounded candidate
//     sampling, never re-scraped, or excluded for policy) is left
//     completely untouched, including its previous Available value.
//
// This means the known-node list only grows (or self-heals a node back to
// Available=true) - hiding currently-dead nodes is left to the dashboard
// filter (see NodeView.Available), not to deletion. Use ClearUnavailable
// for an explicit, user-triggered purge of the ones marked unavailable.
//
// Per-group cursors are left as-is; Pick re-anchors automatically against
// whatever is present in the new list. The merged pool is persisted to the
// cache (if enabled) for fast recovery on restart.
func (p *ProxyPool) Update(freshlyAlive []Proxy, failedAddrs map[string]bool) {
	var freshFwd, freshInfo []Proxy
	for _, px := range freshlyAlive {
		if px.Protocol == "proxyip" {
			freshInfo = append(freshInfo, cloneProxy(px))
		} else {
			px.Available = true
			freshFwd = append(freshFwd, cloneProxy(px))
		}
	}

	p.mu.Lock()
	merged := make(map[string]Proxy, len(p.proxies)+len(freshFwd))
	for _, existing := range p.proxies {
		merged[existing.Key()] = existing
	}
	freshKeys := make(map[string]bool, len(freshFwd))
	added, revived, failed := 0, 0, 0
	for _, px := range freshFwd {
		key := px.Key()
		freshKeys[key] = true
		if existing, ok := merged[key]; !ok {
			added++
		} else {
			if !existing.Available {
				revived++
			}
			px = mergeFreshProxy(existing, px)
		}
		if st := p.stats[key]; st != nil {
			st.ConsecutiveHealthFailures = 0
		}
		merged[key] = px
	}
	for key, existing := range merged {
		if !failedAddrs[key] || freshKeys[key] {
			continue
		}
		st := p.statsForKeyLocked(key)
		if st.ConsecutiveHealthFailures < healthFailureThreshold {
			st.ConsecutiveHealthFailures++
		}
		if st.ConsecutiveHealthFailures >= healthFailureThreshold && existing.Available {
			existing.Available = false
			merged[key] = existing
			failed++
		}
	}

	fwd := make([]Proxy, 0, len(merged))
	for _, px := range merged {
		fwd = append(fwd, px)
	}
	// Go randomizes map iteration order, so without this the merged slice
	// would be reshuffled into an unrelated random permutation on every
	// call - breaking RotateSticky's array-adjacency "next node" rotation
	// (it would stop being a rotation at all) and making pick()/bestBy's
	// candidates[0] fallback change for no reason other than map reordering.
	// Sorting by address gives a stable, reproducible order: existing nodes
	// keep their relative position across cycles, new nodes are inserted at
	// their sorted slot instead of a random one.
	sort.Slice(fwd, func(i, j int) bool {
		if fwd[i].Addr() == fwd[j].Addr() {
			return fwd[i].Protocol < fwd[j].Protocol
		}
		return fwd[i].Addr() < fwd[j].Addr()
	})

	// ProxyIP entries are informational rather than forwarding-capable, but
	// they are still a user-visible pool. Keep their last known inventory by
	// key just like forwarding nodes: a transient source/TCP failure must not
	// make thousands of Worker external reverse-proxy resources disappear
	// from the separate ProxyIP panel.
	infoMerged := make(map[string]Proxy, len(p.proxyIPNodes)+len(freshInfo))
	for _, px := range p.proxyIPNodes {
		infoMerged[px.Key()] = px
	}
	for _, px := range freshInfo {
		if existing, ok := infoMerged[px.Key()]; ok {
			px = mergeFreshProxy(existing, px)
		}
		infoMerged[px.Key()] = px
	}
	info := make([]Proxy, 0, len(infoMerged))
	for _, px := range infoMerged {
		info = append(info, px)
	}
	sort.Slice(info, func(i, j int) bool {
		return info[i].Addr() < info[j].Addr()
	})
	p.proxies = fwd
	p.rebuildProxyIndexLocked()
	p.proxyIPNodes = info
	cache := p.cache
	p.cacheGeneration++
	p.cancelScheduledPersistenceLocked()
	generation, snapshotFwd, snapshotInfo, snapshotStats := p.cacheSnapshotLocked()
	p.mu.Unlock()
	if cache != nil {
		cache.save(generation, snapshotFwd, snapshotInfo, snapshotStats)
	}
	log.Printf("[pool] updated: %d known forwarding proxies total (+%d new, %d revived, %d reached %d-failure unavailable threshold), %d proxyip (info-only) nodes",
		len(fwd), added, revived, failed, healthFailureThreshold, len(info))
}

// mergeFreshProxy combines a successful health-check result with durable
// observations collected independently of that check. A refresh never performs
// a speed test, so it must not erase the last sample. Exit/geo probes can fail
// even when basic connectivity succeeds; empty fields therefore mean "no new
// observation", not "clear the trusted old value". IPChanged is meaningful
// only alongside a newly observed ExitIP.
func mergeFreshProxy(existing, fresh Proxy) Proxy {
	fresh.SpeedKbps = existing.SpeedKbps
	fresh.SpeedTestedAt = existing.SpeedTestedAt
	fresh.SpeedBytes = existing.SpeedBytes
	fresh.SpeedDurationMs = existing.SpeedDurationMs

	if fresh.ExitIP == "" {
		fresh.ExitIP = existing.ExitIP
		fresh.IPChanged = existing.IPChanged
		fresh.IPChangeKnown = existing.IPChangeKnown
	}
	if fresh.Anonymity == "" {
		fresh.Anonymity = existing.Anonymity
	}
	if fresh.Country == "" {
		fresh.Country = existing.Country
	}
	if fresh.City == "" {
		fresh.City = existing.City
	}
	if fresh.Continent == "" {
		fresh.Continent = existing.Continent
	}
	return fresh
}

// ClearUnavailable permanently removes nodes currently marked
// Available=false from the pool - an explicit, user-triggered purge (e.g.
// a dashboard button), never automatic. Returns the number removed.
func (p *ProxyPool) ClearUnavailable() int {
	p.mu.Lock()
	kept := make([]Proxy, 0, len(p.proxies))
	removed := 0
	for _, px := range p.proxies {
		if px.Available {
			kept = append(kept, px)
		} else {
			removed++
		}
	}
	p.proxies = kept
	p.rebuildProxyIndexLocked()

	// Stats are keyed by Proxy.Key(), so delete both the nodes removed by this
	// operation and any older orphan entries left by a protocol reclassification.
	liveKeys := make(map[string]bool, len(kept))
	for _, px := range kept {
		liveKeys[px.Key()] = true
	}
	for key := range p.stats {
		if !liveKeys[key] {
			delete(p.stats, key)
		}
	}

	// Drop node references that no longer exist. A removed manual pin must not
	// leave the group permanently pinned to nowhere; cursors with no remaining
	// anchor are discarded so the next Pick starts cleanly.
	for name, cursor := range p.groupState {
		if cursor == nil {
			delete(p.groupState, name)
			continue
		}
		orphaned := false
		if cursor.stickyKey != "" && !liveKeys[cursor.stickyKey] {
			cursor.stickyKey = ""
			cursor.pinned = false
			orphaned = true
		}
		if cursor.lastPicked != "" && !liveKeys[cursor.lastPicked] {
			cursor.lastPicked = ""
			orphaned = true
		}
		if orphaned && cursor.stickyKey == "" && cursor.lastPicked == "" {
			delete(p.groupState, name)
		}
	}

	cache := p.cache
	p.cacheGeneration++
	p.cancelScheduledPersistenceLocked()
	generation, snapshotFwd, snapshotInfo, snapshotStats := p.cacheSnapshotLocked()
	p.mu.Unlock()
	if cache != nil {
		cache.save(generation, snapshotFwd, snapshotInfo, snapshotStats)
	}
	log.Printf("[pool] cleared %d unavailable node(s), %d remaining", removed, len(kept))
	return removed
}

func (p *ProxyPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

// AvailableCount returns the number of forwarding nodes whose most recent
// health result is usable. It avoids materializing the whole pool for compact
// status polling.
func (p *ProxyPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, px := range p.proxies {
		if px.Available {
			count++
		}
	}
	return count
}

func (p *ProxyPool) All() []Proxy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cloneProxySlice(p.proxies)
}

// RecheckCandidates returns a bounded, rotating slice of known forwarding
// nodes for the periodic health worker. Keeping every discovered node is
// useful, but re-dialing an ever-growing pool in one five-minute pass can
// otherwise take longer than the interval and block fresh scrapes. The cursor
// follows the stable pool order and advances even across unavailable entries,
// giving them a chance to self-heal without starving newer nodes.
func (p *ProxyPool) RecheckCandidates(limit int) []Proxy {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit <= 0 || len(p.proxies) == 0 {
		return nil
	}
	if len(p.proxies) <= limit {
		return cloneProxySlice(p.proxies)
	}

	start := 0
	if p.recheckCursor != "" {
		for i, px := range p.proxies {
			if px.Key() == p.recheckCursor {
				start = (i + 1) % len(p.proxies)
				break
			}
		}
	}
	out := make([]Proxy, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, cloneProxy(p.proxies[(start+i)%len(p.proxies)]))
	}
	p.recheckCursor = out[len(out)-1].Key()
	return out
}

func (p *ProxyPool) ProxyIPNodes() []Proxy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cloneProxySlice(p.proxyIPNodes)
}

// ProxyIPCount reports the legacy informational inventory size without
// cloning it. New deployments count ProxyIP resources from CandidateCatalog;
// this O(1) fallback keeps compact status polling cheap while an older cache
// is being migrated by its first refresh.
func (p *ProxyPool) ProxyIPCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxyIPNodes)
}

// Find returns a copy of the proxy matching key (Proxy.Key()), if present.
func (p *ProxyPool) Find(key string) (Proxy, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if i, ok := p.proxyIndexLookupLocked(key); ok {
		return cloneProxy(p.proxies[i]), true
	}
	// Preserve behavior for a zero-value ProxyPool or externally restored
	// legacy state that predates the index. The normal lifecycle maintains the
	// index, so this fallback is not on the hot path.
	for _, px := range p.proxies {
		if px.Key() == key {
			return cloneProxy(px), true
		}
	}
	return Proxy{}, false
}

// UpdateGeo records an on-demand exit-IP/geo re-verification result for the
// proxy matching key, so a stale label (from a proxy whose exit rotated
// since the last scrape) self-heals as soon as someone checks it.
func (p *ProxyPool) UpdateGeo(key, exitIP, country, city, continent string, ipChanged, ipChangeKnown bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	updated := p.mutateProxyLocked(key, func(px *Proxy) {
		px.ExitIP = exitIP
		px.IPChanged = ipChanged
		px.IPChangeKnown = ipChangeKnown
		if country != "" {
			px.Country = country
			px.City = city
			px.Continent = continent
		}
	})
	if updated {
		p.queuePersistenceLocked()
	}
	return updated
}

// UpdateSpeed records an on-demand speed-test result for the proxy
// matching key.
func (p *ProxyPool) UpdateSpeed(key string, kbps float64, bytes, durationMs int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	updated := p.mutateProxyLocked(key, func(px *Proxy) {
		px.SpeedKbps = kbps
		px.SpeedTestedAt = time.Now().Unix()
		px.SpeedBytes = bytes
		px.SpeedDurationMs = durationMs
	})
	if updated {
		p.queuePersistenceLocked()
	}
	return updated
}

func filterAvailable(list []Proxy) []Proxy {
	var out []Proxy
	for _, px := range list {
		if px.Available {
			out = append(out, px)
		}
	}
	return out
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

func proxyMatchesSources(px Proxy, allowed []string) bool {
	if containsFold(allowed, px.SourceName) {
		return true
	}
	for _, source := range px.SourceNames {
		if containsFold(allowed, source) {
			return true
		}
	}
	return false
}

// resolveGroup filters all down to groupName's members and resolves its
// strategy, then prefers members currently marked Available. Falls back to
// the full pool (sticky) for GroupAny, an empty name, or a name that no
// longer maps to a configured group (e.g. a rule pointing at a
// since-deleted group) - so a stale reference degrades gracefully instead
// of blackholing traffic.
func resolveGroup(all []Proxy, groupName string, groups []Group) ([]Proxy, string) {
	out, strategy := resolveGroupCandidates(all, groupName, groups)
	// The pool keeps every node it's ever seen alive (Update never deletes -
	// see its docstring), so prefer nodes currently marked Available for
	// actual routing/selection. This is applied to THIS group's already-
	// narrowed candidate set, not the whole pool - otherwise a narrowly
	// scoped group (e.g. pinned to one exact node via Group.Nodes) could be
	// starved to zero candidates just because *unrelated* nodes elsewhere in
	// the pool happen to be available, even though its own wanted (but
	// transiently unavailable) node still exists. Only fall back to the
	// full (possibly-stale) set if nothing in it is currently available, so
	// routing never blackholes just because everything happens to be
	// unavailable this cycle.
	if live := filterAvailable(out); len(live) > 0 {
		out = live
	}
	return out, strategy
}

// resolveGroupCandidates does the actual group-membership filtering
// (GroupAny / COUNTRY:xx / named Group), before any Available preference is
// applied by resolveGroup.
func resolveGroupCandidates(all []Proxy, groupName string, groups []Group) ([]Proxy, string) {
	if groupName == "" || groupName == GroupAny {
		return all, StrategySticky
	}
	// Dynamic country group ("COUNTRY:JP"): any live node whose real exit is
	// in that country. Prefer the fastest such node - "give me a JP node"
	// almost always means "the best JP node", and there's no per-country
	// group config to carry a different strategy.
	if cc, ok := parseCountryGroup(groupName); ok {
		var out []Proxy
		for _, px := range all {
			if strings.EqualFold(px.Country, cc) {
				out = append(out, px)
			}
		}
		return out, StrategyLatency
	}
	for _, g := range groups {
		if strings.EqualFold(g.Name, groupName) || g.ID == groupName {
			var out []Proxy
			for _, px := range all {
				if len(g.Nodes) > 0 && !groupMatchesNode(g.Nodes, px) {
					continue
				}
				if len(g.Countries) > 0 && !containsFold(g.Countries, px.Country) {
					continue
				}
				if len(g.Protocols) > 0 && !containsFold(g.Protocols, px.Protocol) {
					continue
				}
				if len(g.Sources) > 0 && !proxyMatchesSources(px, g.Sources) {
					continue
				}
				out = append(out, px)
			}
			strategy := g.Strategy
			if strategy == "" {
				strategy = StrategySticky
			}
			return out, strategy
		}
	}
	return all, StrategySticky
}

// groupMatchesNode prefers the protocol-aware Proxy.Key identity. Existing
// saved groups used bare ip:port entries before protocol variants were kept
// independently, so retain that syntax as a backward-compatible fallback.
// An address-only entry deliberately means "any protocol at this endpoint";
// users who mean exactly one upstream should save the key shown by the API/UI.
func groupMatchesNode(allowed []string, px Proxy) bool {
	for _, value := range allowed {
		value = strings.TrimSpace(value)
		if strings.Contains(value, "://") {
			if strings.EqualFold(value, px.Key()) {
				return true
			}
			continue
		}
		if strings.EqualFold(value, px.Addr()) {
			return true
		}
	}
	return false
}

// Pick selects an upstream proxy for groupName. direct=true means the
// caller should bypass proxying entirely (GroupDirect).
func (p *ProxyPool) Pick(groupName string, groups []Group) (Proxy, bool, bool) {
	return p.pick(groupName, groups, nil)
}

// PickExcluding behaves like Pick but skips candidates whose Key() is in
// exclude - used by the server's retry loop so a failed upstream isn't
// retried within the same connection attempt.
func (p *ProxyPool) PickExcluding(groupName string, groups []Group, exclude map[string]bool) (Proxy, bool, bool) {
	return p.pick(groupName, groups, exclude)
}

func (p *ProxyPool) pick(groupName string, groups []Group, exclude map[string]bool) (Proxy, bool, bool) {
	if groupName == GroupDirect {
		return Proxy{}, false, true
	}

	candidates, strategy := resolveGroup(p.All(), groupName, groups)
	if exclude != nil {
		var filtered []Proxy
		for _, c := range candidates {
			if !exclude[c.Key()] {
				filtered = append(filtered, c)
			}
		}
		candidates = filtered
	}
	if len(candidates) == 0 {
		return Proxy{}, false, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	gc, ok := p.groupState[groupName]
	if !ok {
		gc = &groupCursor{}
		p.groupState[groupName] = gc
	}

	var chosen Proxy
	switch strategy {
	case StrategyRoundRobin:
		chosen = candidates[gc.rrIdx%len(candidates)]
		gc.rrIdx++
	case StrategyRandom:
		chosen = candidates[rand.Intn(len(candidates))]
	case StrategyLatency:
		chosen = bestBy(candidates, func(c Proxy) float64 { return float64(c.LatencyMs) }, false)
	case StrategySpeed:
		chosen = bestBy(candidates, func(c Proxy) float64 { return c.SpeedKbps }, true)
	case StrategyScore:
		chosen = bestBy(candidates, p.scoreLocked, true)
	default: // sticky
		chosen = candidates[0]
		found := false
		for _, c := range candidates {
			if c.Key() == gc.stickyKey {
				chosen = c
				found = true
				break
			}
		}
		if !found && gc.pinned {
			// The manually-locked node isn't a viable candidate for THIS
			// pick (excluded by a retry, marked unavailable, or
			// reclassified to a different Key()) - fall back to some
			// candidate for this one pick only, but do NOT persist that as
			// the new pin. Leaving stickyKey untouched means the lock
			// self-heals the moment the pinned node is viable again,
			// instead of silently and permanently drifting to whatever
			// candidates[0] happened to be this time.
			break
		}
		gc.stickyKey = chosen.Key()
	}

	gc.lastPicked = chosen.Key()
	return chosen, true, false
}

// bestBy returns the candidate with the lowest metric value (or highest,
// when higher==true). Entries whose metric is zero (never measured) are
// skipped unless every candidate is unmeasured, in which case the first
// candidate is returned.
func bestBy(candidates []Proxy, metric func(Proxy) float64, higher bool) Proxy {
	best := candidates[0]
	bestVal := metric(best)
	for _, c := range candidates[1:] {
		v := metric(c)
		if v == 0 {
			continue
		}
		if bestVal == 0 || (higher && v > bestVal) || (!higher && v < bestVal) {
			best = c
			bestVal = v
		}
	}
	return best
}

// ForceSticky pins a specific proxy (by Key) as the sticky choice for a
// group - used for manual "switch" clicks from the dashboard. It works
// regardless of the group's configured strategy, but only "sticks" if that
// strategy is actually "sticky"; otherwise the next Pick recomputes per its
// own rule and overwrites it.
func (p *ProxyPool) ForceSticky(groupName, key string) bool {
	if _, ok := p.Find(key); !ok {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	gc, ok := p.groupState[groupName]
	if !ok {
		gc = &groupCursor{}
		p.groupState[groupName] = gc
	}
	gc.stickyKey = key
	gc.lastPicked = key
	gc.pinned = true // manual choice: auto-rotation must not override it
	return true
}

// SetAuto clears a group's manual lock so the periodic auto-rotation timer
// resumes moving it. Used by the dashboard's "resume auto-rotation" button.
func (p *ProxyPool) SetAuto(groupName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gc, ok := p.groupState[groupName]; ok {
		gc.pinned = false
	}
}

// IsPinned reports whether a group is manually locked to a specific node.
func (p *ProxyPool) IsPinned(groupName string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if gc, ok := p.groupState[groupName]; ok {
		return gc.pinned
	}
	return false
}

// RotateSticky advances a group's pinned proxy to the next one in list
// order (wrapping around). Used by the periodic auto-rotation timer. It
// no-ops (returns ok=false) when the group is manually locked, so a user's
// explicit node choice is never rotated away underneath them.
func (p *ProxyPool) RotateSticky(groupName string) (Proxy, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.proxies) == 0 {
		return Proxy{}, false
	}
	gc, ok := p.groupState[groupName]
	if !ok {
		gc = &groupCursor{}
		p.groupState[groupName] = gc
	}
	if gc.pinned {
		return Proxy{}, false
	}

	// ANY is the automatic default-group rotation. When at least one healthy
	// node exists, walk forward from the previous position until the next
	// healthy node instead of rotating onto a known-dead entry. Keeping the
	// original full-pool fallback means an all-unavailable pool still returns
	// something and retains the project's no-blackhole behavior.
	nextIdx := -1
	if groupName == GroupAny || groupName == "" {
		hasAvailable := false
		for _, px := range p.proxies {
			if px.Available {
				hasAvailable = true
				break
			}
		}
		if hasAvailable {
			anchor := -1
			for i, px := range p.proxies {
				if px.Key() == gc.stickyKey {
					anchor = i
					break
				}
			}
			if anchor < 0 {
				for i, px := range p.proxies {
					if px.Available {
						nextIdx = i
						break
					}
				}
			} else {
				for offset := 1; offset <= len(p.proxies); offset++ {
					i := (anchor + offset) % len(p.proxies)
					if p.proxies[i].Available {
						nextIdx = i
						break
					}
				}
			}
		}
	}
	if nextIdx < 0 {
		anchor := -1
		for i, px := range p.proxies {
			if px.Key() == gc.stickyKey {
				anchor = i
				break
			}
		}
		if anchor < 0 {
			nextIdx = 0
		} else {
			nextIdx = (anchor + 1) % len(p.proxies)
		}
	}
	next := p.proxies[nextIdx]
	gc.stickyKey = next.Key()
	gc.lastPicked = next.Key()
	log.Printf("[pool] rotated %s -> %s (%s %s)", groupName, next.Addr(), next.Country, next.City)
	return next, true
}

// EffectiveCurrent reports which node a group would use *right now*, for
// read-only dashboard display, without consuming any round-robin/sticky
// state. For deterministic strategies (sticky/latency/speed) it returns
// the node an actual Pick would choose, so the dashboard is never blank
// or out of sync with reality even before the first request. For
// per-connection strategies (round-robin/random) there is no single
// "current" node, so it returns the most recently picked one with
// dynamic=true to signal "rotates every connection".
//
// Returns (proxy, ok, dynamic): ok=false means the group currently has no
// members.
func (p *ProxyPool) EffectiveCurrent(groupName string, groups []Group) (Proxy, bool, bool) {
	if groupName == GroupDirect {
		return Proxy{}, false, false
	}
	candidates, strategy := resolveGroup(p.All(), groupName, groups)
	if len(candidates) == 0 {
		return Proxy{}, false, false
	}

	p.mu.RLock()
	gc := p.groupState[groupName]
	var stickyKey, lastPicked string
	if gc != nil {
		stickyKey = gc.stickyKey
		lastPicked = gc.lastPicked
	}
	p.mu.RUnlock()

	find := func(key string) (Proxy, bool) {
		for _, c := range candidates {
			if c.Key() == key {
				return c, true
			}
		}
		return Proxy{}, false
	}

	switch strategy {
	case StrategyLatency:
		return bestBy(candidates, func(c Proxy) float64 { return float64(c.LatencyMs) }, false), true, false
	case StrategySpeed:
		return bestBy(candidates, func(c Proxy) float64 { return c.SpeedKbps }, true), true, false
	case StrategyScore:
		return bestBy(candidates, p.Score, true), true, false
	case StrategyRoundRobin, StrategyRandom:
		if px, ok := find(lastPicked); ok {
			return px, true, true
		}
		return candidates[0], true, true
	default: // sticky
		if px, ok := find(stickyKey); ok {
			return px, true, false
		}
		return candidates[0], true, false
	}
}
