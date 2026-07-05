package main

import (
	"log"
	"math/rand"
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
)

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
}

// ProxyPool holds the live, health-checked node list plus per-group
// selection state.
type ProxyPool struct {
	mu           sync.RWMutex
	proxies      []Proxy // forwarding-capable (socks5/http/https)
	proxyIPNodes []Proxy // informational-only "proxyip" nodes (see parser.go)
	groupState   map[string]*groupCursor
}

func NewProxyPool() *ProxyPool {
	return &ProxyPool{groupState: make(map[string]*groupCursor)}
}

// Update replaces the live proxy list, splitting out info-only proxyip
// nodes. Per-group cursors are left as-is; Pick re-anchors automatically
// against whatever is present in the new list.
func (p *ProxyPool) Update(all []Proxy) {
	var fwd, info []Proxy
	for _, px := range all {
		if px.Protocol == "proxyip" {
			info = append(info, px)
		} else {
			fwd = append(fwd, px)
		}
	}
	p.mu.Lock()
	p.proxies = fwd
	p.proxyIPNodes = info
	p.mu.Unlock()
	log.Printf("[pool] updated: %d forwarding proxies, %d proxyip (info-only) nodes", len(fwd), len(info))
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

// UpdateSpeed records an on-demand speed-test result for the proxy
// matching key.
func (p *ProxyPool) UpdateSpeed(key string, kbps float64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.proxies {
		if p.proxies[i].Key() == key {
			p.proxies[i].SpeedKbps = kbps
			return true
		}
	}
	return false
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
// strategy. Falls back to the full pool (sticky) for GroupAny, an empty
// name, or a name that no longer maps to a configured group (e.g. a rule
// pointing at a since-deleted group) - so a stale reference degrades
// gracefully instead of blackholing traffic.
func resolveGroup(all []Proxy, groupName string, groups []Group) ([]Proxy, string) {
	if groupName == "" || groupName == GroupAny {
		return all, StrategySticky
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
	default: // sticky
		chosen = candidates[0]
		for _, c := range candidates {
			if c.Key() == gc.stickyKey {
				chosen = c
				break
			}
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
	return true
}

// RotateSticky advances a group's pinned proxy to the next one in list
// order (wrapping around). Used by the periodic auto-rotation timer.
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
