package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
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
			LastSourceRefresh: summary.LastSourceRefresh, NextSourceRefresh: summary.NextSourceRefresh,
			LastSourceRefreshAt: summary.LastSourceRefreshAt, NextSourceRefreshAt: summary.NextSourceRefreshAt,
			LastFullRecheck: summary.LastFullRecheck, NextFullRecheck: summary.NextFullRecheck,
			LastFullRecheckAt: summary.LastFullRecheckAt, NextFullRecheckAt: summary.NextFullRecheckAt,
			Groups: summary.Groups, ActiveProxy: summary.ActiveProxy,
			AvailableTotal: summary.AvailableTotal, UnavailableTotal: summary.UnavailableTotal,
			HealthRecheckPending: summary.HealthRecheckPending,
			Scrape:               summary.Scrape,
			CandidateTotal:       candidate.Total, FailedCandidateTotal: candidate.FailedTotal,
			IsolatedUnreachableTotal: candidate.IsolatedUnreachableTotal,
			CandidatePhase:           candidate.Phase, CandidateSourceErrors: candidate.SourceErrors, CandidateUpdatedAt: candidate.UpdatedAt,
		})
		return
	}
	writeJSON(w, s.buildSummary())
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
		storeErr := s.store.SetCheckURL(requestedURL)
		if storeErr != nil && !isConfigDurabilityUncertain(storeErr) {
			writeConfigStoreError(w, storeErr)
			return
		}
		invalidated := s.pool.InvalidateHealth(s.store.Snapshot().CheckURL)
		candidateOutcomesReset := s.pool.candidates.ResetHealthOutcomesSoft()
		flushErr := s.pool.FlushCache()
		if storeErr != nil {
			s.coordinator.triggerFullRecheck(s.pool)
			writeConfigStoreError(w, errors.Join(storeErr, flushErr))
			return
		}
		if flushErr != nil {
			writeErrCode(w, http.StatusInternalServerError, "check_url_not_durable", fmt.Errorf("health criterion change was not persisted: %w", flushErr))
			return
		}
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
	n, err := s.pool.ClearUnavailable()
	if err != nil {
		writeErrCode(w, http.StatusInternalServerError, "clear_unavailable_not_durable", fmt.Errorf("unavailable nodes were not cleared because cache persistence failed: %w", err))
		return
	}
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
		writeNodeOperationBusy(w, "node_verify_busy", err)
		return
	}
	defer s.endManualNodeVerify(in.Key)

	prevExitIP, prevCountry := px.ExitIP, px.Country
	// Use the operator-configured check timeout for each attempt; derive a
	// generous total that covers all retries plus exit-IP and geo probes.
	configuredTimeout, _, _, _ := s.effectiveCheckOptions()
	if configuredTimeout <= 0 {
		configuredTimeout = manualNodeVerifyAttemptTimeout(0)
	}
	verifyTotal := configuredTimeout*manualNodeVerifyMaxAttempts + manualNodeVerifyRetryBackoff(manualNodeVerifyMaxAttempts) + manualNodeVerifyExitTimeout + manualNodeVerifyGeoTimeout
	verifyCtx, cancel := context.WithTimeout(r.Context(), verifyTotal)
	defer cancel()
	verified, reachable, attempts, latencyMs, err := runManualNodeVerifyChecks(
		verifyCtx, s.nodeVerifyOps, px, healthCheckURL, configuredTimeout,
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

	// The transport retries form one explicit health observation. A final
	// failure becomes terminal immediately; a later exhaustive full recheck or
	// explicit manual success may recover that terminal state.
	if !s.pool.ObserveManualHealthOutcomeAtGeneration(in.Key, reachable, policyAllowed, latencyMs, healthGeneration) {
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
	if exitIP != "" && !s.pool.UpdateGeoAtGeneration(in.Key, exitIP, country, city, continent, ipChanged, ipChangeKnown, healthGeneration) {
		if s.pool.HealthGeneration() != healthGeneration {
			writeErrCode(w, http.StatusConflict, "health_criterion_changed", fmt.Errorf("检测标准已改变，结果未应用"))
			return
		}
		writeErr(w, http.StatusConflict, fmt.Errorf("node disappeared while verification was running"))
		return
	}
	labelMatchKnown, labelMatched := manualNodeLabelMatch(country, prevCountry)
	// Manual verification is an explicit operator action, so make the health
	// state durable before replying instead of leaving it in the debounce window.
	if err := s.pool.FlushCache(); err != nil {
		writeErrCode(w, http.StatusInternalServerError, "node_verify_not_durable", fmt.Errorf("verification result was not persisted: %w", err))
		return
	}

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
		writeNodeOperationBusy(w, "node_speedtest_busy", err)
		return
	}
	completed := false
	defer func() { s.endSpeedTest(in.Key, completed) }()

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
	completed = true
	// Speed test results are explicit user actions, so persist them before
	// replying rather than leaving them in the normal debounce window.
	if err := s.pool.FlushCache(); err != nil {
		writeErrCode(w, http.StatusInternalServerError, "node_speedtest_not_durable", fmt.Errorf("speed test result was not persisted: %w", err))
		return
	}
	writeJSON(w, map[string]interface{}{
		"kbps": result.Kbps, "bytes": result.Bytes, "duration_ms": result.DurationMs,
	})
}
func writeNodeOperationBusy(w http.ResponseWriter, code string, err error) {
	retryAfter := 2
	var cooldown *nodeOperationCooldownError
	if errors.As(err, &cooldown) {
		retryAfter = retryAfterSeconds(cooldown.Remaining)
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeErrCode(w, http.StatusTooManyRequests, code, err)
}

func (s *StatusServer) beginSpeedTest(key string) error {
	return s.beginSpeedTestOperation(key, true)
}

func (s *StatusServer) beginCandidateSpeedTest(key string) error {
	return s.beginSpeedTestOperation(key, false)
}

func (s *StatusServer) beginSpeedTestOperation(key string, enforceCooldown bool) error {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	now := time.Now()
	for candidateKey, until := range s.speedCooldownUntil {
		if !until.After(now) {
			delete(s.speedCooldownUntil, candidateKey)
		}
	}
	if _, running := s.speedRunning[key]; running {
		return fmt.Errorf("该节点正在测速")
	}
	if enforceCooldown {
		if until := s.speedCooldownUntil[key]; until.After(now) {
			return &nodeOperationCooldownError{Operation: "测速", Remaining: until.Sub(now)}
		}
	}
	select {
	case s.speedSlots <- struct{}{}:
		s.speedRunning[key] = struct{}{}
		return nil
	default:
		return fmt.Errorf("测速并发已达上限，请稍后重试")
	}
}

func (s *StatusServer) endSpeedTest(key string, completed ...bool) {
	s.speedMu.Lock()
	if _, running := s.speedRunning[key]; running {
		delete(s.speedRunning, key)
		if len(completed) == 0 || completed[0] {
			s.speedCooldownUntil[key] = time.Now().Add(nodeSpeedTestCooldown)
		}
		<-s.speedSlots
	}
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
