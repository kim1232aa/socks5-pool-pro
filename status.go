package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

type StatusServer struct {
	pool  *ProxyPool
	store *ConfigStore
}

func NewStatusServer(pool *ProxyPool, store *ConfigStore) *StatusServer {
	return &StatusServer{pool: pool, store: store}
}

func (s *StatusServer) Start(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/refresh", s.handleRefresh)

	mux.HandleFunc("/api/nodes", s.handleNodes)
	mux.HandleFunc("/api/nodes/switch", s.handleNodeSwitch)
	mux.HandleFunc("/api/nodes/auto", s.handleNodeAuto)
	mux.HandleFunc("/api/nodes/verify", s.handleNodeVerify)
	mux.HandleFunc("/api/nodes/clear-unavailable", s.handleNodesClearUnavailable)
	mux.HandleFunc("/api/nodes/speedtest", s.handleNodeSpeedtest)
	mux.HandleFunc("/api/nodes/export", s.handleNodeExport)

	mux.HandleFunc("/api/sources", s.handleSources)
	mux.HandleFunc("/api/sources/toggle", s.handleSourceToggle)
	mux.HandleFunc("/api/sources/delete", s.handleSourceDelete)

	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/rules/delete", s.handleRuleDelete)
	mux.HandleFunc("/api/rules/move", s.handleRuleMove)
	mux.HandleFunc("/api/rules/default", s.handleRuleDefault)
	mux.HandleFunc("/api/rules/preset-gfw", s.handleRulePresetGFW)

	mux.HandleFunc("/api/groups", s.handleGroups)
	mux.HandleFunc("/api/groups/strategy", s.handleGroupStrategy)
	mux.HandleFunc("/api/groups/delete", s.handleGroupDelete)

	return http.ListenAndServe(addr, mux)
}

// ---- view models ----

type NodeView struct {
	Key       string  `json:"key"`
	Addr      string  `json:"addr"`
	Protocol  string  `json:"protocol"`
	Country   string  `json:"country"`
	City      string  `json:"city"`
	Source    string  `json:"source"`
	ExitIP    string  `json:"exit_ip"`
	IPChanged bool    `json:"ip_changed"`
	Anonymity string  `json:"anonymity"`
	LatencyMs int64   `json:"latency_ms"`
	SpeedKbps float64 `json:"speed_kbps"`
	Score     float64 `json:"score"`
	Successes int     `json:"successes"`
	Failures  int     `json:"failures"`
	Active    bool    `json:"active"`    // this node is the ANY group's current upstream
	Available bool    `json:"available"` // false = last check failed; kept in the pool, hidden by default
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
	Groups       []GroupView `json:"groups"`
}

func (s *StatusServer) buildGroupViews() []GroupView {
	all := s.pool.All()
	groups := s.store.Groups()

	views := []GroupView{}
	anyCurrent, anyOK, anyDynamic := s.pool.EffectiveCurrent(GroupAny, groups)
	anyAddr := ""
	if anyOK {
		anyAddr = anyCurrent.Addr()
	}
	views = append(views, GroupView{
		Name: GroupAny, Strategy: StrategySticky, Count: len(all),
		Current: anyAddr, Dynamic: anyDynamic, Builtin: true,
		Pinned: s.pool.IsPinned(GroupAny),
	})

	for _, g := range groups {
		candidates, strategy := resolveGroup(all, g.Name, groups)
		cur, ok, dynamic := s.pool.EffectiveCurrent(g.Name, groups)
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

// anyCurrentAddr returns the address of the node the ANY group would use
// right now (for marking the active row in the node table).
func (s *StatusServer) anyCurrentAddr() string {
	if px, ok, _ := s.pool.EffectiveCurrent(GroupAny, s.store.Groups()); ok {
		return px.Addr()
	}
	return ""
}

func (s *StatusServer) buildSummary() StatusSummary {
	last, next := getScrapeTimes()
	beijingLoc := time.FixedZone("CST", 8*3600)

	var lastStr, nextStr string
	if !last.IsZero() {
		lastStr = last.In(beijingLoc).Format("2006-01-02 15:04:05")
	}
	if !next.IsZero() {
		nextStr = next.In(beijingLoc).Format("2006-01-02 15:04:05")
	}

	return StatusSummary{
		Total:        s.pool.Size(),
		ProxyIPTotal: len(s.pool.ProxyIPNodes()),
		LastScrape:   lastStr,
		NextScrape:   nextStr,
		Groups:       s.buildGroupViews(),
	}
}

type DashboardData struct {
	StatusSummary
	Nodes        []NodeView
	ProxyIPs     []NodeView
	Sources      []Source
	Rules        []Rule
	DefaultGroup string
	GroupOptions []string
	RuleTypes    []string
	Formats      []string
	Strategies   []string
}

func nodeViewOf(px Proxy, activeAddr string) NodeView {
	return NodeView{
		Key: px.Key(), Addr: px.Addr(), Protocol: px.Protocol,
		Country: px.Country, City: px.City, Source: px.SourceName,
		ExitIP: px.ExitIP, IPChanged: px.IPChanged, Anonymity: px.Anonymity,
		LatencyMs: px.LatencyMs, SpeedKbps: px.SpeedKbps,
		Active:    activeAddr != "" && px.Addr() == activeAddr,
		Available: px.Available,
	}
}

// nodeViews returns the live forwarding node list with the ANY group's
// current upstream flagged Active.
func (s *StatusServer) nodeViews() []NodeView {
	activeAddr := s.anyCurrentAddr()
	nodes := make([]NodeView, 0, s.pool.Size())
	for _, px := range s.pool.All() {
		nv := nodeViewOf(px, activeAddr)
		nv.Score = s.pool.Score(px)
		nv.Successes, nv.Failures = s.pool.StatsOf(px.Key())
		nodes = append(nodes, nv)
	}
	return nodes
}

func (s *StatusServer) buildDashboardData() DashboardData {
	summary := s.buildSummary()

	nodes := s.nodeViews()

	var proxyIPs []NodeView
	for _, px := range s.pool.ProxyIPNodes() {
		proxyIPs = append(proxyIPs, nodeViewOf(px, ""))
	}

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
		Nodes:         nodes,
		ProxyIPs:      proxyIPs,
		Sources:       s.store.Sources(),
		Rules:         rules,
		DefaultGroup:  defaultGroup,
		GroupOptions:  groupOptions,
		RuleTypes:     []string{RuleDomain, RuleDomainSuffix, RuleDomainKeyword, RuleIPCIDR, RuleGeosite},
		Formats:       []string{FormatTextRegex, FormatEDTJSON, FormatProxyIPJSON, FormatPlainList, FormatJSONArray},
		Strategies:    []string{StrategySticky, StrategyRoundRobin, StrategyRandom, StrategyLatency, StrategySpeed, StrategyScore},
	}
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func decodeJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---- handlers: dashboard + status ----

func (s *StatusServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := s.buildDashboardData()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	dashboardTmpl.Execute(w, data)
}

func (s *StatusServer) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.buildSummary())
}

func (s *StatusServer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	TriggerRefresh()
	writeJSON(w, map[string]string{"status": "refresh triggered"})
}

// ---- handlers: nodes ----

func (s *StatusServer) handleNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.nodeViews())
}

func (s *StatusServer) handleNodeSwitch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.pool.ForceSticky(GroupAny, in.Key) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("node not found: %s", in.Key))
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "pinned": "true"})
}

// handleNodeAuto clears the manual lock on the default (ANY) group so the
// periodic auto-rotation resumes.
func (s *StatusServer) handleNodeAuto(w http.ResponseWriter, r *http.Request) {
	s.pool.SetAuto(GroupAny)
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleNodesClearUnavailable is an explicit, user-triggered purge of nodes
// currently marked unavailable. The pool never does this on its own (see
// ProxyPool.Update) - it's only ever invoked by a dashboard button click.
func (s *StatusServer) handleNodesClearUnavailable(w http.ResponseWriter, r *http.Request) {
	n := s.pool.ClearUnavailable()
	writeJSON(w, map[string]int{"removed": n})
}

// handleNodeVerify re-probes a node's real exit IP/geo RIGHT NOW (dialing
// through the live tunnel, same as the periodic health check does), so the
// dashboard can answer "is this node's country label still accurate, and
// does it actually work" on demand instead of trusting a label that may be
// up to one scrape cycle (-scrape-interval, default 20m) stale.
func (s *StatusServer) handleNodeVerify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	px, ok := s.pool.Find(in.Key)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("node not found: %s", in.Key))
		return
	}

	const timeout = 10 * time.Second
	prevExitIP, prevCountry := px.ExitIP, px.Country

	reachable := checkGoogle(px, timeout)
	exitIP := probeExitIP(px, timeout)
	country, city := "", ""
	if exitIP != "" {
		country, city = LookupGeo(exitIP, timeout)
		if country == "Unknown" {
			country = ""
		}
	}
	ipChanged := exitIP != "" && baselineExitIP != "" && exitIP != baselineExitIP

	if exitIP != "" {
		s.pool.UpdateGeo(in.Key, exitIP, country, city, ipChanged)
	}

	writeJSON(w, map[string]interface{}{
		"reachable":     reachable,
		"exit_ip":       exitIP,
		"country":       country,
		"city":          city,
		"ip_changed":    ipChanged,
		"prev_exit_ip":  prevExitIP,
		"prev_country":  prevCountry,
		"label_matched": exitIP == "" || prevCountry == "" || strings.EqualFold(country, prevCountry),
		"baseline_exit": baselineExitIP,
	})
}

func (s *StatusServer) handleNodeSpeedtest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	px, ok := s.pool.Find(in.Key)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("node not found: %s", in.Key))
		return
	}
	kbps, err := SpeedTest(px, 20*time.Second)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	s.pool.UpdateSpeed(in.Key, kbps)
	writeJSON(w, map[string]float64{"kbps": kbps})
}

// ---- handlers: sources ----

func (s *StatusServer) handleSources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.store.Sources())
	case http.MethodPost:
		var in Source
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.store.AddSource(in)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		TriggerRefresh()
		writeJSON(w, created)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *StatusServer) handleSourceToggle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.ToggleSource(in.ID, in.Enabled); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Enabled {
		TriggerRefresh()
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *StatusServer) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.DeleteSource(in.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// ---- handlers: rules ----

func (s *StatusServer) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.store.Rules())
	case http.MethodPost:
		var in Rule
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.store.AddRule(in)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, created)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *StatusServer) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.DeleteRule(in.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *StatusServer) handleRuleMove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    string `json:"id"`
		Delta int    `json:"delta"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.MoveRule(in.ID, in.Delta); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *StatusServer) handleRulePresetGFW(w http.ResponseWriter, r *http.Request) {
	if err := s.store.InstallGFWPreset(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *StatusServer) handleRuleDefault(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Group string `json:"group"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.SetDefaultGroup(in.Group); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// ---- handlers: groups ----

func (s *StatusServer) handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.store.Groups())
	case http.MethodPost:
		var in Group
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.store.AddGroup(in)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, created)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *StatusServer) handleGroupStrategy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       string `json:"id"`
		Strategy string `json:"strategy"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.SetGroupStrategy(in.ID, in.Strategy); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *StatusServer) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.DeleteGroup(in.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// ---- dashboard template ----

var dashboardTmpl = template.Must(template.New("dashboard").Parse(dashboardHTML))
