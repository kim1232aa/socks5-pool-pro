package main

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"sort"
	"strings"
)

// handleHealthz is intentionally independent of pool state, scrape state,
// configuration, and credentials. It is a liveness endpoint, not a readiness
// or status API, so callers learn only that this HTTP process can respond.
func (s *StatusServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "ok\n")
	}
}

// handleReadyz reports whether the first candidate inventory has been
// published. Unlike /healthz, it may return 503 during startup, but it remains
// deliberately data-free and unauthenticated for container orchestrators.
func (s *StatusServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	ready := s.pool != nil && s.pool.candidates != nil && s.pool.candidates.snapshot.Load() != nil
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !ready {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "not ready\n")
		}
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "ready\n")
	}
}

// ---- handlers: dashboard + status ----

func (s *StatusServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	token, err := newCSRFToken()
	if err != nil {
		writeErrCode(w, http.StatusInternalServerError, "csrf_token_generation_failed", err)
		return
	}
	data := s.buildDashboardData()
	data.CSRFToken = token
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTmpl.Execute(w, data); err != nil {
		writeErrCode(w, http.StatusInternalServerError, "dashboard_render_failed", err)
	}
}

func (s *StatusServer) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("compact") == "1" {
		summary := s.buildSummaryWithProxies(false)
		candidate := s.compactCandidateStatus()
		writeJSON(w, compactStatusSummary{
			Total: summary.Total, ProxyIPTotal: summary.ProxyIPTotal,
			LastScrape: summary.LastScrape, NextScrape: summary.NextScrape,
			LastScrapeAt: summary.LastScrapeAt, NextScrapeAt: summary.NextScrapeAt,
			Groups: summary.Groups, ActiveProxy: summary.ActiveProxy,
			AvailableTotal: summary.AvailableTotal, UnavailableTotal: summary.UnavailableTotal,
			HealthRecheckPending: summary.HealthRecheckPending,
			Scrape:               summary.Scrape,
			CandidateTotal:       candidate.Total, CandidatePhase: candidate.Phase,
			CandidateSourceErrors: candidate.SourceErrors, CandidateUpdatedAt: candidate.UpdatedAt,
		})
		return
	}
	writeJSON(w, s.buildSummary())
}

func (s *StatusServer) handleV1Proxies(w http.ResponseWriter, r *http.Request) {
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
		return
	}
	protocol, err := validatedV1ProxyProtocol(r.URL.Query().Get("protocol"))
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_protocol", err)
		return
	}
	page, pageSize, err := strictV1PageParams(r)
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_pagination", err)
		return
	}
	all, snapshotID := s.v1HealthyProxySnapshot()
	w.Header().Set("X-Snapshot-ID", snapshotID)
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" && requested != snapshotID {
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	filtered := filterV1ProxyViews(all, protocol, r)
	pageCount := (len(filtered) + pageSize - 1) / pageSize
	if pageCount < 1 {
		pageCount = 1
	}
	if page > pageCount {
		writeErrCode(w, http.StatusBadRequest, "page_out_of_range", fmt.Errorf("page %d exceeds page_count %d", page, pageCount))
		return
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	rows := make([]V1ProxyView, 0, end-start)
	if start < len(filtered) {
		rows = append(rows, filtered[start:end]...)
	}
	writeJSON(w, V1ProxyPage{
		APIVersion: "v1", SnapshotID: snapshotID, Proxies: rows,
		Page: page, PageSize: pageSize, PageCount: pageCount, HasNext: page < pageCount,
		FilteredTotal: len(filtered), AvailableTotal: len(all),
	})
}

func (s *StatusServer) handleV1ProxyPick(w http.ResponseWriter, r *http.Request) {
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
		return
	}
	protocol, err := validatedV1ProxyProtocol(r.URL.Query().Get("protocol"))
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_protocol", err)
		return
	}
	all, snapshotID := s.v1HealthyProxySnapshot()
	snapshotID = formatV1ProxyPickSnapshotID(all)
	w.Header().Set("X-Snapshot-ID", snapshotID)
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" && requested != snapshotID {
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	filtered := filterV1ProxyViews(all, protocol, r)
	if len(filtered) == 0 {
		writeErrCode(w, http.StatusNotFound, "proxy_not_found", fmt.Errorf("no healthy proxy matches the requested filters"))
		return
	}
	selected := filtered[0]
	for _, candidate := range filtered[1:] {
		if candidate.score > selected.score || candidate.score == selected.score && candidate.Key < selected.Key {
			selected = candidate
		}
	}
	writeJSON(w, V1ProxyPickResponse{APIVersion: "v1", SnapshotID: snapshotID, Proxy: selected})
}

func (s *StatusServer) v1HealthyProxySnapshot() ([]V1ProxyView, string) {
	s.pool.mu.RLock()
	views := make([]V1ProxyView, 0, len(s.pool.proxies))
	for _, px := range s.pool.proxies {
		if !px.Available || !proxyHardRoutable(px) {
			continue
		}
		switch px.Protocol {
		case "socks5", "http", "https":
		default:
			continue
		}
		proxyURL := px.ConsumerURL()
		view := V1ProxyView{
			ProxyURL: proxyURL, Key: px.Key(), Protocol: px.Protocol,
			Country: normalizedNodeCountry(px.Country), City: px.City,
			Latency: px.LatencyMs, Speed: px.SpeedKbps, score: s.pool.scoreLocked(px),
		}
		if px.Protocol == "socks5" {
			view.SocksURL = proxyURL
		}
		views = append(views, view)
	}
	s.pool.mu.RUnlock()
	sort.SliceStable(views, func(i, j int) bool { return views[i].Key < views[j].Key })
	return views, formatV1ProxySnapshotID(views)
}

func (s *StatusServer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	operation, accepted := s.coordinator.requestRefresh()
	w.Header().Set("Location", "/api/refresh/status")
	writeJSONStatus(w, http.StatusAccepted, struct {
		RefreshOperation
		Accepted  bool   `json:"accepted"`
		Coalesced bool   `json:"coalesced"`
		StatusURL string `json:"status_url"`
	}{
		RefreshOperation: operation,
		Accepted:         accepted, Coalesced: !accepted, StatusURL: "/api/refresh/status",
	})
}

func (s *StatusServer) handleRefreshStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.coordinator.refreshOperationStatus())
}

func (s *StatusServer) handleHealthRecheckStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.coordinator.healthRecheckOperationStatus())
}

// handleCheckURL gets or sets the health-check target URL - the sole
// criterion for whether a node counts as alive (see checker.go checkURL).
// A successful POST immediately invalidates health learned under the old
// criterion, then schedules one full retained-pool recheck. Source inventory
// is unchanged, so a duplicate source scrape would only add load and delay.
func (s *StatusServer) handleCheckURL(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, map[string]string{"url": s.store.CheckURL()})
	case http.MethodPost:
		var in struct {
			URL string `json:"url"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		requestedURL := strings.TrimSpace(in.URL)
		if err := validateCheckURL(requestedURL); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if requestedURL == s.store.CheckURL() {
			writeJSON(w, struct {
				Status                 string `json:"status"`
				URL                    string `json:"url"`
				Changed                bool   `json:"changed"`
				InvalidatedTotal       int    `json:"invalidated_total"`
				CandidateOutcomesReset int    `json:"candidate_outcomes_reset"`
			}{Status: "ok", URL: requestedURL, Changed: false})
			return
		}
		if err := s.store.SetCheckURL(requestedURL); err != nil {
			writeConfigStoreError(w, err)
			return
		}
		invalidated := s.pool.InvalidateHealth(s.store.CheckURL())
		candidateOutcomesReset := s.pool.candidates.ResetHealthOutcomes()
		s.pool.FlushCache()
		operation, accepted := s.coordinator.triggerFullRecheck(s.pool)
		w.Header().Set("Location", "/api/health-recheck/status")
		writeJSON(w, struct {
			Status                 string                 `json:"status"`
			URL                    string                 `json:"url"`
			Changed                bool                   `json:"changed"`
			InvalidatedTotal       int                    `json:"invalidated_total"`
			CandidateOutcomesReset int                    `json:"candidate_outcomes_reset"`
			HealthRecheck          HealthRecheckOperation `json:"health_recheck"`
			Accepted               bool                   `json:"accepted"`
			StatusURL              string                 `json:"status_url"`
		}{
			Status: "ok", URL: s.store.CheckURL(), Changed: true,
			InvalidatedTotal: invalidated, CandidateOutcomesReset: candidateOutcomesReset,
			HealthRecheck: operation, Accepted: accepted, StatusURL: "/api/health-recheck/status",
		})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
	}
}

// ---- handlers: nodes ----

func (s *StatusServer) handleNodes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Sunset", "Thu, 31 Dec 2026 23:59:59 GMT")
	w.Header().Add("Link", `</api/nodes/page>; rel="successor-version"`)
	writeJSON(w, s.nodeViews())
}

const (
	defaultNodePageSize = 20
	maxNodePageSize     = 100
)

// handleNodesPage serves a bounded, server-filtered page for the dashboard.
// Keep handleNodes above as-is: external callers may still rely on its legacy
// plain JSON array contract.
func (s *StatusServer) handleNodesPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
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
	generation := s.pool.cacheGeneration
	s.pool.mu.RUnlock()
	return formatPoolSnapshotID(generation)
}

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
	onlyChanged := nodeQueryEnabled(query.Get("only_changed"))
	onlyAvailable := nodeQueryEnabled(query.Get("available")) || nodeQueryEnabled(query.Get("hide_unavailable"))
	sortBy := strings.ToLower(strings.TrimSpace(query.Get("sort")))

	s.pool.mu.RLock()
	poolGeneration := s.pool.cacheGeneration
	activeProxy, activeOK := effectiveAnyCurrentLocked(s.pool.proxies, s.pool.groupState[GroupAny])
	activeKey := ""
	if activeOK {
		activeKey = activeProxy.Key()
	}
	views := make([]NodeView, 0, len(s.pool.proxies))
	for _, liveProxy := range s.pool.proxies {
		px := cloneProxy(liveProxy)
		view := nodeViewOf(px, activeKey)
		view.Score = s.pool.scoreLocked(px)
		if stats := s.pool.stats[px.Key()]; stats != nil {
			view.Successes, view.Failures = stats.Successes, stats.Failures
		}
		views = append(views, view)
	}
	s.pool.mu.RUnlock()
	snapshotID := formatPoolSnapshotID(poolGeneration)
	countries := make(map[string]*NodeCountrySummary)
	availableTotal := 0
	unknownCountryTotal := 0
	var active *NodeView
	for _, view := range views {
		if view.Available {
			availableTotal++
		}
		if view.Active {
			activeCopy := view
			active = &activeCopy
		}

		if code := normalizedNodeCountry(view.Country); code != "" {
			summary := countries[code]
			if summary == nil {
				summary = &NodeCountrySummary{Country: code}
				countries[code] = summary
			}
			summary.Total++
			if view.Available {
				summary.Available++
			}
			if summary.Continent == "" && view.Continent != "" {
				summary.Continent = view.Continent
			}
		} else {
			unknownCountryTotal++
		}
	}

	filtered := make([]NodeView, 0, len(views))
	for _, view := range views {
		if search != "" && !strings.Contains(strings.ToLower(view.Addr+" "+view.ExitIP), search) {
			continue
		}
		if country != "" && normalizedNodeCountry(view.Country) != country {
			continue
		}
		if unknownCountry && normalizedNodeCountry(view.Country) != "" {
			continue
		}
		if protocol != "" && strings.ToLower(view.Protocol) != protocol {
			continue
		}
		if onlyChanged && !(view.IPChangeKnown && view.IPChanged) {
			continue
		}
		if onlyAvailable && !view.Available {
			continue
		}
		filtered = append(filtered, view)
	}
	sortNodeViews(filtered, sortBy)

	filteredTotal := len(filtered)
	pageCount := (filteredTotal + pageSize - 1) / pageSize
	if pageCount < 1 {
		pageCount = 1
	}
	if page > pageCount {
		page = pageCount
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > filteredTotal {
		end = filteredTotal
	}
	pageNodes := make([]NodeView, 0, end-start)
	if start < filteredTotal {
		pageNodes = append(pageNodes, filtered[start:end]...)
	}

	countryList := make([]NodeCountrySummary, 0, len(countries))
	for _, summary := range countries {
		countryList = append(countryList, *summary)
	}
	sort.Slice(countryList, func(i, j int) bool { return countryList[i].Country < countryList[j].Country })

	return NodePageResponse{
		Nodes:               pageNodes,
		SnapshotID:          snapshotID,
		Page:                page,
		PageSize:            pageSize,
		PageCount:           pageCount,
		HasNext:             page < pageCount,
		FilteredTotal:       filteredTotal,
		PoolTotal:           len(views),
		AvailableTotal:      availableTotal,
		UnavailableTotal:    len(views) - availableTotal,
		Countries:           countryList,
		CountryUnknownTotal: unknownCountryTotal,
		Active:              active,
	}
}

func (s *StatusServer) handleNodeSwitch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	switch s.pool.forceSticky(GroupAny, in.Key) {
	case forceStickyNotFound:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("node not found: %s", in.Key))
		return
	case forceStickyUnavailable:
		writeErrCode(w, http.StatusConflict, "node_unavailable", fmt.Errorf("节点当前不可用，不能手动切换；请先复检并等待节点恢复"))
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
	if s.pool.HealthRecheckPending() {
		writeErrCode(w, http.StatusConflict, "health_recheck_in_progress", fmt.Errorf("健康标准全量复检尚未完成，暂不能永久清理不可用节点"))
		return
	}
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
	healthGeneration, healthCheckURL := s.pool.HealthCriterion()
	if healthCheckURL == "" {
		s.pool.SetHealthCriterion(s.store.CheckURL())
		healthGeneration, healthCheckURL = s.pool.HealthCriterion()
	}
	px, ok := s.pool.Find(in.Key)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("node not found: %s", in.Key))
		return
	}
	if err := s.beginManualNodeVerify(in.Key); err != nil {
		w.Header().Set("Retry-After", "2")
		writeErrCode(w, http.StatusTooManyRequests, "node_verify_busy", err)
		return
	}
	defer s.endManualNodeVerify(in.Key)

	prevExitIP, prevCountry := px.ExitIP, px.Country
	verifyCtx, cancel := context.WithTimeout(r.Context(), manualNodeVerifyTotalTimeout)
	defer cancel()
	verified, reachable, attempts, latencyMs, err := runManualNodeVerifyChecks(
		verifyCtx, s.nodeVerifyOps, px, healthCheckURL,
	)
	if err != nil {
		writeManualNodeVerifyCanceled(w, attempts, err)
		return
	}

	exitIP := ""
	country, city, continent := "", "", ""
	if reachable {
		px = verified
		exitIP = s.nodeVerifyOps.probeExitIP(verifyCtx, px, manualNodeVerifyExitTimeout)
	}
	if reachable && exitIP != "" {
		country, city, continent = s.nodeVerifyOps.lookupGeo(verifyCtx, exitIP, manualNodeVerifyGeoTimeout)
		country = strings.TrimSpace(country)
		if strings.EqualFold(country, "Unknown") {
			country = ""
		}
	}
	// Cancellation is not a health observation. In particular, do not let a
	// browser navigation or client timeout mark the node unavailable (or record
	// a success) after the caller has stopped the verification.
	if err := verifyCtx.Err(); err != nil {
		writeManualNodeVerifyCanceled(w, attempts, err)
		return
	}
	if reachable {
		s.pool.UpdateVerifiedCredentialsAtGeneration(in.Key, verified, healthGeneration)
	}

	baseline := BaselineExitIP()
	policy := evaluateIPChangePolicy(exitIP, baseline, s.pool.RequireIPChangePolicy())
	ipChangeKnown := policy.IPChangeKnown
	ipChanged := policy.IPChanged
	policyAllowed := policy.PolicyAllowed

	// Three transport attempts form one explicit health observation, not three
	// independent failures. A success revives immediately; a final failure joins
	// the same three-observation debounce used by background health work so one
	// unlucky manual click cannot evict an intermittently reachable node.
	if !s.pool.ObserveHealthOutcomeAtGeneration(in.Key, reachable, policyAllowed, latencyMs, healthGeneration) {
		if s.pool.HealthGeneration() != healthGeneration {
			writeErrCode(w, http.StatusConflict, "health_criterion_changed", fmt.Errorf("检测标准已改变，结果未应用"))
			return
		}
		writeErr(w, http.StatusConflict, fmt.Errorf("node disappeared while verification was running"))
		return
	}
	available, consecutiveFailures, stateOK := s.pool.HealthStateOf(in.Key)
	if !stateOK {
		writeErr(w, http.StatusConflict, fmt.Errorf("node disappeared while verification was running"))
		return
	}
	if exitIP != "" {
		s.pool.UpdateGeo(in.Key, exitIP, country, city, continent, ipChanged, ipChangeKnown)
	}
	reachableKeys := map[string]bool{}
	policyFiltered := map[string]bool{}
	if reachable {
		reachableKeys[in.Key] = true
	}
	if reachable && !policyAllowed {
		policyFiltered[in.Key] = true
	}
	s.pool.candidates.ApplyHealthOutcomes([]Proxy{px}, reachableKeys, policyFiltered)
	labelMatchKnown, labelMatched := manualNodeLabelMatch(country, prevCountry)
	// Manual verification is an explicit operator action, so make the health
	// state durable before replying instead of leaving it in the debounce window.
	s.pool.FlushCache()

	writeJSON(w, map[string]interface{}{
		"reachable":            reachable,
		"attempts":             attempts,
		"available":            available,
		"consecutive_failures": consecutiveFailures,
		"latency_ms":           latencyMs,
		"exit_ip":              exitIP,
		"country":              country,
		"city":                 city,
		"ip_changed":           ipChanged,
		"ip_change_known":      ipChangeKnown,
		"policy_excluded":      reachable && !policyAllowed,
		"prev_exit_ip":         prevExitIP,
		"prev_country":         prevCountry,
		"label_match_known":    labelMatchKnown,
		"label_matched":        labelMatched,
		"baseline_exit":        baseline,
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
	healthGeneration := s.pool.HealthGeneration()
	px, ok := s.pool.Find(in.Key)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("node not found: %s", in.Key))
		return
	}
	if err := s.beginSpeedTest(in.Key); err != nil {
		w.Header().Set("Retry-After", "2")
		writeErr(w, http.StatusTooManyRequests, err)
		return
	}
	defer s.endSpeedTest(in.Key)

	result, verified, err := speedTestCredentialCandidatesContext(r.Context(), px, speedTestOperationTimeout)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.pool.UpdateVerifiedCredentialsAtGeneration(in.Key, verified, healthGeneration)
	if !s.pool.UpdateSpeed(in.Key, result.Kbps, result.Bytes, result.DurationMs) {
		writeErr(w, http.StatusConflict, fmt.Errorf("node disappeared while speed test was running"))
		return
	}
	// Speed test results are explicit user actions, so persist them before
	// replying rather than leaving them in the normal debounce window.
	s.pool.FlushCache()
	writeJSON(w, map[string]interface{}{
		"kbps": result.Kbps, "bytes": result.Bytes, "duration_ms": result.DurationMs,
	})
}

func (s *StatusServer) beginSpeedTest(key string) error {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	if _, running := s.speedRunning[key]; running {
		return fmt.Errorf("该节点正在测速")
	}
	select {
	case s.speedSlots <- struct{}{}:
		s.speedRunning[key] = struct{}{}
		return nil
	default:
		return fmt.Errorf("测速任务已满,请稍后重试")
	}
}

func (s *StatusServer) endSpeedTest(key string) {
	s.speedMu.Lock()
	delete(s.speedRunning, key)
	<-s.speedSlots
	s.speedMu.Unlock()
}

// ---- handlers: sources ----

func (s *StatusServer) handleSources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, safeManagementSources(s.store.Sources()))
	case http.MethodPost:
		var in Source
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.store.AddSource(in)
		if err != nil {
			writeConfigStoreError(w, err)
			return
		}
		s.coordinator.triggerRefresh()
		writeJSON(w, safeManagementSource(created))
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
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
	s.coordinator.sourceLifecycleMu.Lock()
	defer s.coordinator.sourceLifecycleMu.Unlock()
	if err := s.store.ToggleSource(in.ID, in.Enabled); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	retired := s.pool.ApplyEnabledSources(s.store.Sources())
	s.coordinator.triggerRefresh()
	writeJSON(w, map[string]interface{}{"status": "ok", "retired_total": retired})
}

func (s *StatusServer) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.coordinator.sourceLifecycleMu.Lock()
	defer s.coordinator.sourceLifecycleMu.Unlock()
	if err := s.store.DeleteSource(in.ID); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	retired := s.pool.ApplyEnabledSources(s.store.Sources())
	s.coordinator.triggerRefresh()
	writeJSON(w, map[string]interface{}{"status": "ok", "retired_total": retired})
}

// ---- handlers: rules ----

func (s *StatusServer) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, s.store.Rules())
	case http.MethodPost:
		var in Rule
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.store.AddRule(in)
		if err != nil {
			writeConfigStoreError(w, err)
			return
		}
		writeJSON(w, created)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
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
		writeConfigStoreError(w, err)
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
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *StatusServer) handleRulePresetGFW(w http.ResponseWriter, r *http.Request) {
	if err := s.store.InstallGFWPreset(); err != nil {
		writeConfigStoreError(w, err)
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
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// ---- handlers: groups ----

func (s *StatusServer) handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, s.store.Groups())
	case http.MethodPost:
		var in Group
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.store.AddGroup(in)
		if err != nil {
			writeConfigStoreError(w, err)
			return
		}
		writeJSON(w, created)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
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
		writeConfigStoreError(w, err)
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
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// ---- dashboard template ----

var dashboardTmpl = template.Must(template.New("dashboard").Parse(dashboardHTML))
