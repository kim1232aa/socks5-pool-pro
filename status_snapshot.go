package main

import (
	"net"
)

// compactStatusPoolSnapshot contains exactly the pool-wide values needed by
// dashboard and compact-status polling. Unlike statusPoolSnapshot it never
// owns a detached copy of the proxy inventory.
type compactStatusPoolSnapshot struct {
	Total              int
	ProxyIPFallback    int
	AvailableTotal     int
	Groups             []GroupView
	Active             Proxy
	ActiveOK           bool
	HealthCheckPending bool
}

// streamingStatusSelection reproduces effectiveCurrentFromSnapshot while a
// caller walks matching proxies one at a time. Keeping both an all-routable
// and an available-only instance lets the caller apply resolveGroup's legacy
// "prefer healthy, otherwise use a soft-unavailable fallback" rule without
// ever materializing either candidate slice.
type streamingStatusSelection struct {
	count         int
	current       Proxy
	metric        float64
	cursorMatched bool
}

func (selection *streamingStatusSelection) observe(px Proxy, strategy string, cursor *groupCursor, score float64) {
	first := selection.count == 0
	selection.count++

	switch strategy {
	case StrategyLatency, StrategySpeed, StrategyScore:
		metric, higher := float64(px.LatencyMs), false
		if strategy == StrategySpeed {
			metric, higher = px.SpeedKbps, true
		} else if strategy == StrategyScore {
			metric, higher = score, true
		}
		if first || metric != 0 && (selection.metric == 0 || higher && metric > selection.metric || !higher && metric < selection.metric) {
			selection.current = px
			selection.metric = metric
		}
	case StrategyRoundRobin, StrategyRandom:
		if first {
			selection.current = px
		}
		lastPicked := ""
		if cursor != nil {
			lastPicked = cursor.lastPicked
		}
		if !selection.cursorMatched && statusProxyHasKey(px, lastPicked) {
			selection.current = px
			selection.cursorMatched = true
		}
	default: // sticky, including the historical empty-strategy fallback
		if first {
			selection.current = px
		}
		stickyKey := ""
		if cursor != nil {
			stickyKey = cursor.stickyKey
		}
		if !selection.cursorMatched && statusProxyHasKey(px, stickyKey) {
			selection.current = px
			selection.cursorMatched = true
		}
	}
}

// statusProxyHasKey compares a cursor identity without allocating a fresh
// Proxy.Key string for every proxy on every dashboard poll. The fallback
// retains exact historical behavior for malformed legacy values.
func statusProxyHasKey(px Proxy, key string) bool {
	prefixLen := len(px.Protocol) + len("://")
	if len(key) < prefixLen || key[:len(px.Protocol)] != px.Protocol || key[len(px.Protocol):prefixLen] != "://" {
		return false
	}
	host, port, err := net.SplitHostPort(key[prefixLen:])
	if err != nil {
		return px.Key() == key
	}
	return host == px.IP && port == px.Port
}

type streamingStatusGroup struct {
	group     Group
	strategy  string
	cursor    *groupCursor
	routable  streamingStatusSelection
	available streamingStatusSelection
}

func (group *streamingStatusGroup) observe(px Proxy, score float64) {
	group.routable.observe(px, group.strategy, group.cursor, score)
	if px.Available {
		group.available.observe(px, group.strategy, group.cursor, score)
	}
}

func (group *streamingStatusGroup) preferred() streamingStatusSelection {
	if group.available.count > 0 {
		return group.available
	}
	return group.routable
}

func statusProxyMatchesGroup(px Proxy, group Group) bool {
	if len(group.Nodes) > 0 && !groupMatchesNode(group.Nodes, px) {
		return false
	}
	if len(group.Countries) > 0 && !containsFold(group.Countries, px.Country) {
		return false
	}
	if len(group.Protocols) > 0 && !containsFold(group.Protocols, px.Protocol) {
		return false
	}
	return len(group.Sources) == 0 || proxyMatchesSources(px, group.Sources)
}

// captureCompactStatusPoolSnapshot performs one read-locked streaming pass
// across the live pool. Its retained memory is O(number of configured groups),
// not O(pool size) or O(pool size * groups): each group keeps only two running
// selections and counters.
func (s *StatusServer) captureCompactStatusPoolSnapshot(groups []Group) compactStatusPoolSnapshot {
	groupStates := make([]streamingStatusGroup, len(groups))
	for i, group := range groups {
		strategy := group.Strategy
		if strategy == "" {
			strategy = StrategySticky
		}
		groupStates[i] = streamingStatusGroup{group: group, strategy: strategy}
	}

	views := make([]GroupView, 0, len(groups)+1)
	s.pool.mu.RLock()
	defer s.pool.mu.RUnlock()

	any := streamingStatusGroup{strategy: StrategySticky, cursor: s.pool.groupState[GroupAny]}
	for i := range groupStates {
		groupStates[i].cursor = s.pool.groupState[groupStates[i].group.Name]
	}

	availableTotal := 0
	for _, px := range s.pool.proxies {
		// Hard routing exclusions win even if a legacy/corrupt cache contains an
		// inconsistent Available=true bit.
		if px.Available && proxyHardRoutable(px) {
			switch px.Protocol {
			case "socks5", "http", "https":
				availableTotal++
			}
		}
		if !proxyHardRoutable(px) {
			continue
		}

		any.observe(px, 0)
		score, scoreReady := 0.0, false
		for i := range groupStates {
			group := &groupStates[i]
			if !statusProxyMatchesGroup(px, group.group) {
				continue
			}
			if group.strategy == StrategyScore && !scoreReady {
				score = s.pool.scoreLocked(px)
				scoreReady = true
			}
			group.observe(px, score)
		}
	}

	anyPreferred := any.preferred()
	anyCurrent, anyOK := Proxy{}, anyPreferred.count > 0
	if anyOK {
		anyCurrent = anyPreferred.current
	}
	views = append(views, GroupView{
		Name: GroupAny, Strategy: StrategySticky, Count: len(s.pool.proxies),
		Current: statusSelectionAddr(anyPreferred), Builtin: true,
		Pinned: any.cursor != nil && any.cursor.pinned,
	})
	for i := range groupStates {
		group := &groupStates[i]
		preferred := group.preferred()
		dynamic := preferred.count > 0 && (group.strategy == StrategyRoundRobin || group.strategy == StrategyRandom)
		views = append(views, GroupView{
			ID: group.group.ID, Name: group.group.Name, Strategy: group.strategy, Count: preferred.count,
			Current: statusSelectionAddr(preferred), Dynamic: dynamic,
			Countries: group.group.Countries, Protocols: group.group.Protocols,
			Sources: group.group.Sources, Nodes: group.group.Nodes,
		})
	}

	return compactStatusPoolSnapshot{
		Total: len(s.pool.proxies), ProxyIPFallback: len(s.pool.proxyIPNodes),
		AvailableTotal: availableTotal, Groups: views, Active: anyCurrent, ActiveOK: anyOK,
		HealthCheckPending: s.pool.healthRecheckPending,
	}
}

func statusSelectionAddr(selection streamingStatusSelection) string {
	if selection.count == 0 {
		return ""
	}
	return selection.current.Addr()
}

// anyCurrentKey returns the protocol-aware identity of the node the ANY group
// would use right now (for marking exactly one active row in the node table).
func (s *StatusServer) anyCurrentKey() string {
	if px, ok, _ := s.pool.EffectiveCurrent(GroupAny, s.store.Groups()); ok {
		return px.Key()
	}
	return ""
}

type statusPoolSnapshot struct {
	Proxies    []Proxy
	Generation uint64
	Active     Proxy
	ActiveOK   bool
	GroupState map[string]*groupCursor
	Stats      map[string]nodeStats
}

func (s *StatusServer) captureStatusPoolSnapshot(includeStats bool) statusPoolSnapshot {
	s.pool.mu.RLock()
	defer s.pool.mu.RUnlock()
	proxies := make([]Proxy, len(s.pool.proxies))
	for i, proxy := range s.pool.proxies {
		// The compatibility response needs a detached pool snapshot because it
		// emits every healthy proxy after releasing the lock. Compact/dashboard
		// polling uses captureCompactStatusPoolSnapshot and never enters this path.
		proxy.SourceNames = append([]string(nil), proxy.SourceNames...)
		proxy.SourceIDs = nil
		proxy.CredentialAlternates = nil
		proxies[i] = proxy
	}
	active, activeOK := effectiveAnyCurrentLocked(proxies, s.pool.groupState[GroupAny])
	groupState := make(map[string]*groupCursor, len(s.pool.groupState))
	for name, cursor := range s.pool.groupState {
		if cursor == nil {
			continue
		}
		copy := *cursor
		groupState[name] = &copy
	}
	var stats map[string]nodeStats
	if includeStats {
		stats = make(map[string]nodeStats, len(s.pool.stats))
		for key, stat := range s.pool.stats {
			if stat != nil {
				stats[key] = *stat
			}
		}
	}
	return statusPoolSnapshot{
		Proxies: proxies, Generation: s.pool.cacheGeneration, Active: active, ActiveOK: activeOK,
		GroupState: groupState, Stats: stats,
	}
}

// effectiveAnyCurrentLocked mirrors EffectiveCurrent(GroupAny) against a
// detached pool slice while the caller holds the pool lock that protects the
// sticky cursor. Healthy nodes are preferred; if none are healthy the legacy
// no-blackhole fallback still reports the first retained node.
func effectiveAnyCurrentLocked(proxies []Proxy, cursor *groupCursor) (Proxy, bool) {
	if len(proxies) == 0 {
		return Proxy{}, false
	}
	candidates := make([]Proxy, 0, len(proxies))
	for _, px := range proxies {
		if proxyHardRoutable(px) {
			candidates = append(candidates, px)
		}
	}
	if len(candidates) == 0 {
		return Proxy{}, false
	}
	if available := filterAvailable(candidates); len(available) > 0 {
		candidates = available
	}
	stickyKey := ""
	if cursor != nil {
		stickyKey = cursor.stickyKey
	}
	for _, px := range candidates {
		if px.Key() == stickyKey {
			return px, true
		}
	}
	return candidates[0], true
}
