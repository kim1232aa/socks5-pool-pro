package main

import (
	"log"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
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
	Successes     int   `json:"successes"`
	Failures      int   `json:"failures"`
	LastLatencyMs int64 `json:"last_latency_ms"`
}

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
	// Nodes is an explicit allowlist of node addresses ("ip:port"). When
	// set, only these exact nodes are group members - this is how you pin
	// a rule (e.g. a domain) to one specific node: make a group whose
	// Nodes is that single address. Empty = any node (subject to the
	// other filters).
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
	mu           sync.RWMutex
	proxies      []Proxy // forwarding-capable (socks5/http/https)
	proxyIPNodes []Proxy // informational-only "proxyip" nodes (see parser.go)
	groupState   map[string]*groupCursor
	stats        map[string]*nodeStats // keyed by Proxy.Key()
	cache        *poolCache
}

func NewProxyPool() *ProxyPool {
	return &ProxyPool{
		groupState: make(map[string]*groupCursor),
		stats:      make(map[string]*nodeStats),
	}
}

// RecordResult logs the outcome of using a node (from the SOCKS5 server or
// a background re-check) so its quality score reflects real reliability.
func (p *ProxyPool) RecordResult(key string, ok bool, latencyMs int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.stats[key]
	if st == nil {
		st = &nodeStats{}
		p.stats[key] = st
	}
	if ok {
		st.Successes++
		if latencyMs > 0 {
			st.LastLatencyMs = latencyMs
		}
	} else {
		st.Failures++
	}
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

// mutateProxy finds the node matching key and applies fn to it in place.
// Caller must hold p.mu. Shared by every "look up one node by Key() and
// change one field" operation (UpdateLatency/SetAvailable/UpdateGeo/
// UpdateSpeed) so the lock+scan logic exists in exactly one place.
func (p *ProxyPool) mutateProxyLocked(key string, fn func(*Proxy)) bool {
	for i := range p.proxies {
		if p.proxies[i].Key() == key {
			fn(&p.proxies[i])
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
	p.mutateProxyLocked(key, func(px *Proxy) { px.LatencyMs = latencyMs })
}

// SetAvailable flips a node's Available flag from the lightweight periodic
// re-check (every -recheck-interval, cheaper and more frequent than a full
// scrape). This never removes the node - it only affects whether it's
// preferred for routing and whether the dashboard hides it by default.
func (p *ProxyPool) SetAvailable(key string, available bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mutateProxyLocked(key, func(px *Proxy) { px.Available = available })
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
	p.cache = c
	p.mu.Unlock()
}

// Prime seeds the pool from cached nodes at startup, so it's usable before
// the first scrape completes. Does not write back to the cache.
func (p *ProxyPool) Prime(forwarding, proxyip []Proxy) {
	p.mu.Lock()
	p.proxies = forwarding
	p.proxyIPNodes = proxyip
	p.mu.Unlock()
	log.Printf("[pool] primed from cache: %d forwarding, %d proxyip nodes", len(forwarding), len(proxyip))
}

// Update merges this cycle's health-check results into the live pool,
// splitting out info-only proxyip nodes. Nodes are identified by address
// (ip:port, matching dedupeByAddr's identity) and are never dropped here:
//   - an address present in freshlyAlive gets its data replaced wholesale
//     and is marked Available=true (found alive this cycle).
//   - an address in failedAddrs (dialed and genuinely failed to connect -
//     see CheckProxies) is marked Available=false but stays in the pool.
//     A node that was dialed successfully but excluded from freshlyAlive for
//     a policy reason (transparent proxy, blocked country) is neither here
//     nor in freshlyAlive, so it's left untouched by the next branch.
//   - an address in neither (skipped this cycle by random candidate
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
	var freshFwd, info []Proxy
	for _, px := range freshlyAlive {
		if px.Protocol == "proxyip" {
			info = append(info, px)
		} else {
			px.Available = true
			freshFwd = append(freshFwd, px)
		}
	}

	p.mu.Lock()
	merged := make(map[string]Proxy, len(p.proxies)+len(freshFwd))
	for _, existing := range p.proxies {
		merged[existing.Addr()] = existing
	}
	added, revived, failed := 0, 0, 0
	for _, px := range freshFwd {
		if existing, ok := merged[px.Addr()]; !ok {
			added++
		} else if !existing.Available {
			revived++
		}
		merged[px.Addr()] = px
	}
	for addr, existing := range merged {
		if failedAddrs[addr] && existing.Available {
			existing.Available = false
			merged[addr] = existing
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
	sort.Slice(fwd, func(i, j int) bool { return fwd[i].Addr() < fwd[j].Addr() })
	p.proxies = fwd
	p.proxyIPNodes = info
	cache := p.cache
	p.mu.Unlock()
	if cache != nil {
		cache.save(fwd, info, p.statsSnapshot())
	}
	log.Printf("[pool] updated: %d known forwarding proxies total (+%d new, %d revived, %d newly unavailable this cycle), %d proxyip (info-only) nodes",
		len(fwd), added, revived, failed, len(info))
}

// ClearUnavailable permanently removes nodes currently marked
// Available=false from the pool - an explicit, user-triggered purge (e.g.
// a dashboard button), never automatic. Returns the number removed.
func (p *ProxyPool) ClearUnavailable() int {
	p.mu.Lock()
	kept := p.proxies[:0:0]
	removed := 0
	for _, px := range p.proxies {
		if px.Available {
			kept = append(kept, px)
		} else {
			removed++
		}
	}
	p.proxies = kept
	info := p.proxyIPNodes
	cache := p.cache
	p.mu.Unlock()
	if cache != nil {
		cache.save(kept, info, p.statsSnapshot())
	}
	log.Printf("[pool] cleared %d unavailable node(s), %d remaining", removed, len(kept))
	return removed
}

func (p *ProxyPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

func (p *ProxyPool) All() []Proxy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Proxy, len(p.proxies))
	copy(out, p.proxies)
	return out
}

func (p *ProxyPool) ProxyIPNodes() []Proxy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Proxy, len(p.proxyIPNodes))
	copy(out, p.proxyIPNodes)
	return out
}

// Find returns a copy of the proxy matching key (Proxy.Key()), if present.
func (p *ProxyPool) Find(key string) (Proxy, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, px := range p.proxies {
		if px.Key() == key {
			return px, true
		}
	}
	return Proxy{}, false
}

// UpdateGeo records an on-demand exit-IP/geo re-verification result for the
// proxy matching key, so a stale label (from a proxy whose exit rotated
// since the last scrape) self-heals as soon as someone checks it.
func (p *ProxyPool) UpdateGeo(key, exitIP, country, city string, ipChanged bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mutateProxyLocked(key, func(px *Proxy) {
		px.ExitIP = exitIP
		px.IPChanged = ipChanged
		if country != "" {
			px.Country = country
			px.City = city
		}
	})
}

// UpdateSpeed records an on-demand speed-test result for the proxy
// matching key.
func (p *ProxyPool) UpdateSpeed(key string, kbps float64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mutateProxyLocked(key, func(px *Proxy) { px.SpeedKbps = kbps })
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
				if len(g.Nodes) > 0 && !containsFold(g.Nodes, px.Addr()) {
					continue
				}
				if len(g.Countries) > 0 && !containsFold(g.Countries, px.Country) {
					continue
				}
				if len(g.Protocols) > 0 && !containsFold(g.Protocols, px.Protocol) {
					continue
				}
				if len(g.Sources) > 0 && !containsFold(g.Sources, px.SourceName) {
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
	idx := 0
	for i, px := range p.proxies {
		if px.Key() == gc.stickyKey {
			idx = i
			break
		}
	}
	next := p.proxies[(idx+1)%len(p.proxies)]
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
