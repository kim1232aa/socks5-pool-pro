package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const (
	defaultNodePageSize = 20
	maxNodePageSize     = 100
)

// nodePageLight carries only the scalar fields required to sort/filter nodes
// plus the pool index for late materialization. A full NodeView is built only
// for the requested page after the bounded prefix or suffix is selected.
type nodePageLight struct {
	index               int
	key                 string
	country             string
	continent           string
	score               float64
	latencyMs           int64
	speedKbps           float64
	successes           int
	failures            int
	consecutiveFailures int
	available           bool
	active              bool
	unknownCountry      bool
}

func nodePageLightLess(a, b nodePageLight, sortBy string) bool {
	switch sortBy {
	case "latency":
		latency := func(e nodePageLight) int64 {
			if e.latencyMs > 0 {
				return e.latencyMs
			}
			return 1<<62 - 1
		}
		if la, lb := latency(a), latency(b); la != lb {
			return la < lb
		}
	case "speed":
		if a.speedKbps != b.speedKbps {
			return a.speedKbps > b.speedKbps
		}
	case "country":
		if a.country != b.country {
			return a.country < b.country
		}
	default:
		if a.score != b.score {
			return a.score > b.score
		}
	}
	return a.key < b.key
}

// validNodeAnonymity reports whether the anonymity filter value is supported.
// Empty means "no filter"; "unknown" matches nodes with no detected anonymity.
func validNodeAnonymity(anonymity string) bool {
	switch anonymity {
	case "", "elite", "anonymous", "transparent", "unknown":
		return true
	}
	return false
}

// nodeAnonymityMatches applies the anonymity filter. "unknown" matches nodes
// whose anonymity was never detected (empty); other values match exactly.
func nodeAnonymityMatches(filter, detected string) bool {
	if filter == "" {
		return true
	}
	if filter == "unknown" {
		return detected == ""
	}
	return detected == filter
}

func (s *StatusServer) handleNodesPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
		return
	}
	if anonymity := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("anonymity"))); !validNodeAnonymity(anonymity) {
		writeErrCode(w, http.StatusBadRequest, "invalid_anonymity", fmt.Errorf("anonymity must be one of elite, anonymous, transparent, unknown"))
		return
	}
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" {
		current := s.currentNodeSnapshotID()
		w.Header().Set("X-Snapshot-ID", current)
		if requested != current {
			writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
			return
		}
	}
	page := s.buildNodePage(r)
	w.Header().Set("X-Snapshot-ID", page.SnapshotID)
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" && requested != page.SnapshotID {
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	writeJSON(w, page)
}

func (s *StatusServer) currentNodeSnapshotID() string {
	s.pool.mu.RLock()
	generation, activeRevision := s.pool.cacheGeneration, s.pool.activeRevision
	s.pool.mu.RUnlock()
	return formatNodeSnapshotID(generation, activeRevision)
}

// buildNodePage counts and facets the pool before collecting page rows. Knowing
// filteredTotal lets an out-of-range request clamp to the last page before any
// collector is allocated. The second scan retains the smaller sorted prefix or
// suffix, so first and last pages use O(pageSize) row memory.
func (s *StatusServer) buildNodePage(r *http.Request) NodePageResponse {
	page, pageSize := nodePageParams(r)
	query := r.URL.Query()
	search := strings.ToLower(strings.TrimSpace(query.Get("search")))
	countryRaw := strings.TrimSpace(query.Get("country"))
	unknownCountry := strings.EqualFold(countryRaw, "__unknown__") || nodeQueryEnabled(query.Get("country_unknown"))
	country := ""
	if !unknownCountry {
		country = normalizedNodeCountry(countryRaw)
	}
	protocol := strings.ToLower(strings.TrimSpace(query.Get("protocol")))
	anonymity := strings.ToLower(strings.TrimSpace(query.Get("anonymity")))
	onlyChanged := nodeQueryEnabled(query.Get("only_changed"))
	onlyAvailable := nodeQueryEnabled(query.Get("available")) || nodeQueryEnabled(query.Get("hide_unavailable"))
	onlyUnavailable := !onlyAvailable && nodeQueryEnabled(query.Get("unavailable"))
	sortBy := strings.ToLower(strings.TrimSpace(query.Get("sort")))
	matches := func(px *Proxy, countryCode string, available bool) bool {
		if search != "" && !strings.Contains(strings.ToLower(px.Addr()+" "+px.ExitIP), search) {
			return false
		}
		if country != "" && countryCode != country {
			return false
		}
		if unknownCountry && countryCode != "" {
			return false
		}
		if protocol != "" && strings.ToLower(px.Protocol) != protocol {
			return false
		}
		if !nodeAnonymityMatches(anonymity, strings.ToLower(px.Anonymity)) {
			return false
		}
		if onlyChanged && !(px.IPChangeKnown && px.IPChanged) {
			return false
		}
		if onlyAvailable && !available {
			return false
		}
		if onlyUnavailable && available {
			return false
		}
		return true
	}
	less := func(a, b nodePageLight) bool { return nodePageLightLess(a, b, sortBy) }

	countries := make(map[string]*NodeCountrySummary)
	availableTotal, unknownCountryTotal, filteredTotal, poolTotal := 0, 0, 0, 0
	var activeLight *nodePageLight

	s.pool.mu.RLock()
	generation, activeRevision := s.pool.cacheGeneration, s.pool.activeRevision
	cursor := s.pool.groupState[GroupAny]
	stickyKey := ""
	if cursor != nil {
		stickyKey = cursor.stickyKey
	}
	activeIndex := -1
	firstRoutableIndex := -1
	for i := range s.pool.proxies {
		px := &s.pool.proxies[i]
		if !proxyHardRoutable(*px) {
			continue
		}
		if firstRoutableIndex == -1 {
			firstRoutableIndex = i
		}
		if px.Available {
			if activeIndex == -1 {
				activeIndex = i
			}
			if stickyKey != "" && statusProxyHasKey(*px, stickyKey) {
				activeIndex = i
				break
			}
		}
	}
	if activeIndex == -1 {
		activeIndex = firstRoutableIndex
	}

	for i := range s.pool.proxies {
		liveProxy := &s.pool.proxies[i]
		poolTotal++
		available := liveProxy.Available && proxyHardRoutable(*liveProxy)
		if available {
			availableTotal++
		}
		countryCode := normalizedNodeCountry(liveProxy.Country)
		if countryCode == "" {
			unknownCountryTotal++
		} else {
			summary := countries[countryCode]
			if summary == nil {
				summary = &NodeCountrySummary{Country: countryCode}
				countries[countryCode] = summary
			}
			summary.Total++
			if available {
				summary.Available++
			}
			if summary.Continent == "" && liveProxy.Continent != "" {
				summary.Continent = liveProxy.Continent
			}
		}
		if i == activeIndex {
			key := liveProxy.Key()
			successes, failures, consecutiveFailures := 0, 0, 0
			if stats := s.pool.stats[key]; stats != nil {
				successes, failures = stats.Successes, stats.Failures
				consecutiveFailures = stats.ConsecutiveHealthFailures
			}
			activeLight = &nodePageLight{
				index: i, key: key, country: countryCode, continent: liveProxy.Continent,
				score: scoreWithStats(*liveProxy, s.pool.stats[key]), latencyMs: liveProxy.LatencyMs,
				speedKbps: liveProxy.SpeedKbps, successes: successes, failures: failures,
				consecutiveFailures: consecutiveFailures, available: available, active: true,
				unknownCountry: countryCode == "",
			}
		}
		if matches(liveProxy, countryCode, available) {
			filteredTotal++
		}
	}

	pageCount := pageCountForTotal(filteredTotal, pageSize)
	if page > pageCount {
		page = pageCount
	}
	start := pageWindowLimit(page-1, pageSize)
	end := min(pageWindowLimit(page, pageSize), filteredTotal)
	limit, reverse := pageCollectorWindow(filteredTotal, start, end)
	collectorLess := less
	if reverse {
		collectorLess = func(a, b nodePageLight) bool { return less(b, a) }
	}
	collector := newBoundedCollector[nodePageLight](limit, collectorLess)

	for i := range s.pool.proxies {
		liveProxy := &s.pool.proxies[i]
		available := liveProxy.Available && proxyHardRoutable(*liveProxy)
		countryCode := normalizedNodeCountry(liveProxy.Country)
		if !matches(liveProxy, countryCode, available) {
			continue
		}
		if i == activeIndex && activeLight != nil {
			collector.add(*activeLight)
			continue
		}
		key := liveProxy.Key()
		successes, failures, consecutiveFailures := 0, 0, 0
		if stats := s.pool.stats[key]; stats != nil {
			successes, failures = stats.Successes, stats.Failures
			consecutiveFailures = stats.ConsecutiveHealthFailures
		}
		collector.add(nodePageLight{
			index: i, key: key, country: countryCode, continent: liveProxy.Continent,
			score: scoreWithStats(*liveProxy, s.pool.stats[key]), latencyMs: liveProxy.LatencyMs,
			speedKbps: liveProxy.SpeedKbps, successes: successes, failures: failures,
			consecutiveFailures: consecutiveFailures, available: available,
			unknownCountry: countryCode == "",
		})
	}

	ordered := collector.sortFinal(less)
	retainedStart := start
	if reverse {
		retainedStart = 0
	}
	retainedEnd := retainedStart + end - start
	pageNodes := make([]NodeView, 0, retainedEnd-retainedStart)
	for _, e := range ordered[retainedStart:retainedEnd] {
		px := cloneProxy(s.pool.proxies[e.index])
		view := nodeViewOf(px, "")
		view.Active = e.active
		view.Score = e.score
		view.Successes = e.successes
		view.Failures = e.failures
		view.ConsecutiveFailures = e.consecutiveFailures
		pageNodes = append(pageNodes, view)
	}

	var active *NodeView
	if activeLight != nil && activeLight.index < len(s.pool.proxies) {
		px := cloneProxy(s.pool.proxies[activeLight.index])
		view := nodeViewOf(px, "")
		view.Active = true
		view.Score = activeLight.score
		view.Successes = activeLight.successes
		view.Failures = activeLight.failures
		view.ConsecutiveFailures = activeLight.consecutiveFailures
		active = &view
	}
	s.pool.mu.RUnlock()

	countryList := make([]NodeCountrySummary, 0, len(countries))
	for _, summary := range countries {
		countryList = append(countryList, *summary)
	}
	sort.Slice(countryList, func(i, j int) bool { return countryList[i].Country < countryList[j].Country })

	return NodePageResponse{
		Nodes:               pageNodes,
		SnapshotID:          formatNodeSnapshotID(generation, activeRevision),
		Page:                page,
		PageSize:            pageSize,
		PageCount:           pageCount,
		HasNext:             page < pageCount,
		FilteredTotal:       filteredTotal,
		PoolTotal:           poolTotal,
		AvailableTotal:      availableTotal,
		UnavailableTotal:    poolTotal - availableTotal,
		Countries:           countryList,
		CountryUnknownTotal: unknownCountryTotal,
		Active:              active,
	}
}
