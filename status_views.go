package main

import (
	"strings"
	"time"
)

// ---- view models ----

type NodeView struct {
	Key               string  `json:"key"`
	Addr              string  `json:"addr"`
	Protocol          string  `json:"protocol"`
	ProxyURL          string  `json:"proxy_url"`
	Username          string  `json:"username"`
	Password          string  `json:"password"`
	Country           string  `json:"country"`
	City              string  `json:"city"`
	Continent         string  `json:"continent"` // AS/NA/EU/AF/SA/OC/AN - groups the dashboard's country filter
	Source            string  `json:"source"`
	ExitIP            string  `json:"exit_ip"`
	IPChanged         bool    `json:"ip_changed"`
	IPChangeKnown     bool    `json:"ip_change_known"`
	Anonymity         string  `json:"anonymity"`
	LatencyMs         int64   `json:"latency_ms"`
	SpeedKbps         float64 `json:"speed_kbps"`
	SpeedTestedAt     int64   `json:"speed_tested_at,omitempty"`
	SpeedBytes        int64   `json:"speed_bytes,omitempty"`
	SpeedDurationMs   int64   `json:"speed_duration_ms,omitempty"`
	Score             float64 `json:"score"`
	Successes         int     `json:"successes"`
	Failures          int     `json:"failures"`
	Active            bool    `json:"active"`    // this node is the ANY group's current upstream
	Available         bool    `json:"available"` // false = last check failed; kept in the pool, hidden by default
	SourceRetired     bool    `json:"source_retired,omitempty"`
	HealthInvalidated bool    `json:"health_invalidated,omitempty"`
	PolicyExcluded    bool    `json:"policy_excluded,omitempty"`
}

// NodeCountrySummary is the small, pool-wide country index used by the
// dashboard's country selector. It is intentionally independent of the
// current page filters, so changing a selector never makes other choices
// disappear merely because their rows are not on this page.
type NodeCountrySummary struct {
	Country   string `json:"country"`
	Continent string `json:"continent,omitempty"`
	Total     int    `json:"total"`
	Available int    `json:"available"`
}

// NodePageResponse is the bounded alternative to the legacy /api/nodes array.
// The dashboard receives just Nodes for one page while retaining the counters,
// country controls, and active-node context needed to render the node tab.
type NodePageResponse struct {
	Nodes               []NodeView           `json:"nodes"`
	SnapshotID          string               `json:"snapshot_id"`
	Page                int                  `json:"page"`
	PageSize            int                  `json:"page_size"`
	PageCount           int                  `json:"page_count"`
	HasNext             bool                 `json:"has_next"`
	FilteredTotal       int                  `json:"filtered_total"`
	PoolTotal           int                  `json:"pool_total"`
	AvailableTotal      int                  `json:"available_total"`
	UnavailableTotal    int                  `json:"unavailable_total"`
	Countries           []NodeCountrySummary `json:"countries"`
	CountryUnknownTotal int                  `json:"country_unknown_total"`
	Active              *NodeView            `json:"active,omitempty"`
}

type GroupView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Strategy  string   `json:"strategy"`
	Count     int      `json:"count"`
	Current   string   `json:"current"`
	Dynamic   bool     `json:"dynamic"` // current rotates per-connection (round-robin/random)
	Pinned    bool     `json:"pinned"`  // manually locked to Current; auto-rotation paused
	Builtin   bool     `json:"builtin"`
	Countries []string `json:"countries,omitempty"`
	Protocols []string `json:"protocols,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	Nodes     []string `json:"nodes,omitempty"`
}

type StatusSummary struct {
	Total        int         `json:"total"`
	ProxyIPTotal int         `json:"proxyip_total"`
	LastScrape   string      `json:"last_scrape"`
	NextScrape   string      `json:"next_scrape"`
	LastScrapeAt string      `json:"last_scrape_at,omitempty"`
	NextScrapeAt string      `json:"next_scrape_at,omitempty"`
	Groups       []GroupView `json:"groups"`
	// Keep active_proxy present even when the pool has no healthy selection.
	// Registration clients depend on a stable top-level extraction shape.
	ActiveProxy          string         `json:"active_proxy"`
	Proxies              []PoolAPIProxy `json:"proxies"`
	AvailableTotal       int            `json:"available_total"`
	UnavailableTotal     int            `json:"unavailable_total"`
	HealthRecheckPending bool           `json:"health_recheck_pending"`
	Scrape               ScrapeInfo     `json:"scrape"`
}

// PoolAPIProxy is a connection-ready, healthy upstream for consumers that
// fetch an address from an IP-pool API. The URL retains protocol and optional
// credentials, so a SOCKS5 upstream is never mistaken for an HTTP proxy.
type PoolAPIProxy struct {
	ProxyURL string `json:"proxy_url"`
	SocksURL string `json:"socks_url,omitempty"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// V1ProxyView is the bounded, metadata-bearing alternative to the unbounded
// legacy /api/status proxies list. It deliberately retains the same
// connection-ready URL fields and never exposes Telegram links.
type V1ProxyView struct {
	ProxyURL string  `json:"proxy_url"`
	SocksURL string  `json:"socks_url,omitempty"`
	Username string  `json:"username"`
	Password string  `json:"password"`
	Key      string  `json:"key"`
	Protocol string  `json:"protocol"`
	Country  string  `json:"country"`
	City     string  `json:"city,omitempty"`
	Latency  int64   `json:"latency_ms"`
	Speed    float64 `json:"speed_kbps"`
	score    float64
}

type V1ProxyPage struct {
	APIVersion     string        `json:"api_version"`
	SnapshotID     string        `json:"snapshot_id"`
	Proxies        []V1ProxyView `json:"proxies"`
	Page           int           `json:"page"`
	PageSize       int           `json:"page_size"`
	PageCount      int           `json:"page_count"`
	HasNext        bool          `json:"has_next"`
	FilteredTotal  int           `json:"filtered_total"`
	AvailableTotal int           `json:"available_total"`
}

type V1ProxyPickResponse struct {
	APIVersion string      `json:"api_version"`
	SnapshotID string      `json:"snapshot_id"`
	Proxy      V1ProxyView `json:"proxy"`
}

func buildGroupViewsFromSnapshot(state statusPoolSnapshot, groups []Group) []GroupView {
	all := state.Proxies
	views := []GroupView{}
	anyCandidates, anyStrategy := resolveGroup(all, GroupAny, groups)
	anyCursor := state.GroupState[GroupAny]
	anyCurrent, anyOK, anyDynamic := effectiveCurrentFromSnapshot(anyCandidates, anyStrategy, anyCursor, state.Stats)
	anyAddr := ""
	if anyOK {
		anyAddr = anyCurrent.Addr()
	}
	views = append(views, GroupView{
		Name: GroupAny, Strategy: StrategySticky, Count: len(all),
		Current: anyAddr, Dynamic: anyDynamic, Builtin: true,
		Pinned: anyCursor != nil && anyCursor.pinned,
	})

	for _, g := range groups {
		candidates, strategy := resolveGroup(all, g.Name, groups)
		cur, ok, dynamic := effectiveCurrentFromSnapshot(candidates, strategy, state.GroupState[g.Name], state.Stats)
		current := ""
		if ok {
			current = cur.Addr()
		}
		views = append(views, GroupView{
			ID: g.ID, Name: g.Name, Strategy: strategy, Count: len(candidates),
			Current: current, Dynamic: dynamic,
			Countries: g.Countries, Protocols: g.Protocols, Sources: g.Sources, Nodes: g.Nodes,
		})
	}
	return views
}

func effectiveCurrentFromSnapshot(candidates []Proxy, strategy string, cursor *groupCursor, stats map[string]nodeStats) (Proxy, bool, bool) {
	if len(candidates) == 0 {
		return Proxy{}, false, false
	}
	stickyKey, lastPicked := "", ""
	if cursor != nil {
		stickyKey, lastPicked = cursor.stickyKey, cursor.lastPicked
	}
	find := func(key string) (Proxy, bool) {
		for _, candidate := range candidates {
			if candidate.Key() == key {
				return candidate, true
			}
		}
		return Proxy{}, false
	}
	switch strategy {
	case StrategyLatency:
		return bestBy(candidates, func(candidate Proxy) float64 { return float64(candidate.LatencyMs) }, false), true, false
	case StrategySpeed:
		return bestBy(candidates, func(candidate Proxy) float64 { return candidate.SpeedKbps }, true), true, false
	case StrategyScore:
		return bestBy(candidates, func(candidate Proxy) float64 {
			stat, ok := stats[candidate.Key()]
			if !ok {
				return scoreWithStats(candidate, nil)
			}
			return scoreWithStats(candidate, &stat)
		}, true), true, false
	case StrategyRoundRobin, StrategyRandom:
		if candidate, ok := find(lastPicked); ok {
			return candidate, true, true
		}
		return candidates[0], true, true
	default:
		if candidate, ok := find(stickyKey); ok {
			return candidate, true, false
		}
		return candidates[0], true, false
	}
}

func (s *StatusServer) buildSummary() StatusSummary {
	return s.buildSummaryWithProxies(true)
}

func (s *StatusServer) buildSummaryWithProxies(includeProxies bool) StatusSummary {
	scrapeState := s.coordinator.scrapeStatusSnapshot()
	last, next := scrapeState.Last, scrapeState.Next
	beijingLoc := time.FixedZone("CST", 8*3600)

	var lastStr, nextStr string
	if !last.IsZero() {
		lastStr = last.In(beijingLoc).Format("2006-01-02 15:04:05")
	}
	if !next.IsZero() {
		nextStr = next.In(beijingLoc).Format("2006-01-02 15:04:05")
	}

	groups := s.store.Groups()
	if !includeProxies {
		poolState := s.captureCompactStatusPoolSnapshot(groups)
		activeProxy := ""
		if poolState.ActiveOK && poolState.Active.Available {
			activeProxy = poolState.Active.ConsumerURL()
		}
		proxyIPTotal := poolState.ProxyIPFallback
		if catalogTotal, loaded := s.pool.candidates.protocolTotal("proxyip"); loaded {
			proxyIPTotal = catalogTotal
		}
		return StatusSummary{
			Total:                poolState.Total,
			ProxyIPTotal:         proxyIPTotal,
			LastScrape:           lastStr,
			NextScrape:           nextStr,
			LastScrapeAt:         formatRFC3339UTC(last),
			NextScrapeAt:         formatRFC3339UTC(next),
			Groups:               poolState.Groups,
			ActiveProxy:          activeProxy,
			AvailableTotal:       poolState.AvailableTotal,
			UnavailableTotal:     poolState.Total - poolState.AvailableTotal,
			HealthRecheckPending: poolState.HealthCheckPending,
			Scrape:               scrapeState.Info,
		}
	}

	includeStats := false
	for _, group := range groups {
		if group.Strategy == StrategyScore {
			includeStats = true
			break
		}
	}
	poolState := s.captureStatusPoolSnapshot(includeStats)
	availableTotal := countAPIPoolProxies(poolState.Proxies)
	proxies := apiPoolProxiesFrom(poolState.Proxies)
	availableTotal = len(proxies)
	activeProxy := ""
	if poolState.ActiveOK && poolState.Active.Available && proxyHardRoutable(poolState.Active) {
		activeProxy = poolState.Active.ConsumerURL()
	}

	proxyIPTotal := 0
	if catalogTotal, loaded := s.pool.candidates.protocolTotal("proxyip"); loaded {
		proxyIPTotal = catalogTotal
	} else {
		proxyIPTotal = s.pool.ProxyIPCount()
	}
	summary := StatusSummary{
		Total:                len(poolState.Proxies),
		ProxyIPTotal:         proxyIPTotal,
		LastScrape:           lastStr,
		NextScrape:           nextStr,
		LastScrapeAt:         formatRFC3339UTC(last),
		NextScrapeAt:         formatRFC3339UTC(next),
		Groups:               buildGroupViewsFromSnapshot(poolState, groups),
		ActiveProxy:          activeProxy,
		Proxies:              proxies,
		AvailableTotal:       availableTotal,
		UnavailableTotal:     len(poolState.Proxies) - availableTotal,
		HealthRecheckPending: s.pool.HealthRecheckPending(),
		Scrape:               scrapeState.Info,
	}
	return summary
}

func formatRFC3339UTC(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// apiPoolProxies exposes only nodes that passed the pool's health check.
// Stale nodes stay in the internal pool for self-healing, but must not be
// returned to the registration service, which dials the selected node
// directly and would otherwise repeatedly fail registration tasks.
func (s *StatusServer) apiPoolProxies() []PoolAPIProxy {
	return apiPoolProxiesFrom(s.pool.All())
}

func apiPoolProxiesFrom(proxies []Proxy) []PoolAPIProxy {
	out := make([]PoolAPIProxy, 0)
	for _, px := range proxies {
		if !px.Available || !proxyHardRoutable(px) {
			continue
		}
		switch px.Protocol {
		case "socks5", "http", "https":
		default:
			continue
		}

		proxyURL := px.ConsumerURL()
		view := PoolAPIProxy{
			ProxyURL: proxyURL, Username: px.Username, Password: px.Password,
		}
		if px.Protocol == "socks5" {
			view.SocksURL = proxyURL
		}
		out = append(out, view)
	}
	return out
}

func countAPIPoolProxies(proxies []Proxy) int {
	count := 0
	for _, px := range proxies {
		if !px.Available || !proxyHardRoutable(px) {
			continue
		}
		switch px.Protocol {
		case "socks5", "http", "https":
			count++
		}
	}
	return count
}

type DashboardData struct {
	StatusSummary
	Sources      []Source
	Rules        []Rule
	DefaultGroup string
	GroupOptions []string
	RuleTypes    []string
	Formats      []string
	Strategies   []string
	CheckURL     string
	CSRFToken    string
}

func nodeViewOf(px Proxy, activeKey string) NodeView {
	source := px.SourceName
	if len(px.SourceNames) > 0 {
		source = strings.Join(px.SourceNames, ", ")
	}
	return NodeView{
		Key: px.Key(), Addr: px.Addr(), Protocol: px.Protocol, ProxyURL: px.ConsumerURL(),
		Username: px.Username, Password: px.Password,
		Country: px.Country, City: px.City, Continent: px.Continent, Source: source,
		ExitIP: px.ExitIP, IPChanged: px.IPChanged, IPChangeKnown: px.IPChangeKnown, Anonymity: px.Anonymity,
		LatencyMs: px.LatencyMs, SpeedKbps: px.SpeedKbps,
		SpeedTestedAt: px.SpeedTestedAt, SpeedBytes: px.SpeedBytes, SpeedDurationMs: px.SpeedDurationMs,
		Active:        activeKey != "" && px.Key() == activeKey,
		Available:     px.Available && proxyHardRoutable(px),
		SourceRetired: px.SourceRetired, HealthInvalidated: px.HealthInvalidated, PolicyExcluded: px.PolicyExcluded,
	}
}

// nodeViews returns the live forwarding node list with the ANY group's
// current upstream flagged Active.
func (s *StatusServer) nodeViews() []NodeView {
	activeKey := s.anyCurrentKey()
	nodes := make([]NodeView, 0, s.pool.Size())
	for _, px := range s.pool.All() {
		nv := nodeViewOf(px, activeKey)
		nv.Score = s.pool.Score(px)
		nv.Successes, nv.Failures = s.pool.StatsOf(px.Key())
		nodes = append(nodes, nv)
	}
	return nodes
}

func (s *StatusServer) buildDashboardData() DashboardData {
	summary := s.buildSummaryWithProxies(false)

	groupOptions := []string{GroupAny, GroupDirect}
	for _, g := range s.store.Groups() {
		groupOptions = append(groupOptions, g.Name)
	}

	rules := s.store.Rules()
	defaultGroup := GroupAny
	for _, r := range rules {
		if r.Type == RuleMatch {
			defaultGroup = r.Group
			break
		}
	}

	return DashboardData{
		StatusSummary: summary,
		Sources:       safeManagementSources(s.store.Sources()),
		Rules:         rules,
		DefaultGroup:  defaultGroup,
		GroupOptions:  groupOptions,
		RuleTypes:     []string{RuleDomain, RuleDomainSuffix, RuleDomainKeyword, RuleIPCIDR, RuleGeosite},
		Formats:       []string{FormatTextRegex, FormatEDTJSON, FormatProxyIPJSON, FormatPlainList, FormatJSONArray},
		Strategies:    []string{StrategySticky, StrategyRoundRobin, StrategyRandom, StrategyLatency, StrategySpeed, StrategyScore},
		CheckURL:      s.store.CheckURL(),
	}
}
