package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// refreshBaselineForStatus is injectable so tests can exercise the
// baseline-refresh endpoint without real egress traffic.
var refreshBaselineForStatus = RefreshBaselineExitWithChangeContext

// checkOptionsPayload is the JSON shape of GET/POST /api/settings/check-options.
// Numbers use 0 for "follow the CLI flag"; RequireIPChange uses "default" for
// the same, or a boolean to set an explicit override.
type checkOptionsPayload struct {
	MaxConcurrent                *int        `json:"max_concurrent"`
	CheckTimeoutSeconds          *int        `json:"check_timeout_seconds"`
	MaxCandidates                *int        `json:"max_candidates"`
	SourceRefreshIntervalSeconds *int        `json:"source_refresh_interval_seconds"`
	FullRecheckIntervalSeconds   *int        `json:"full_recheck_interval_seconds"`
	RequireIPChange              interface{} `json:"require_ip_change"`
}

// handleCheckOptions reads or updates the dashboard-tunable health-check
// options (concurrency, per-node timeout, per-cycle candidate budget, and the
// require-ip-change policy). Numeric changes take effect on the next check
// cycle without invalidating anything. A require-ip-change policy change
// invalidates prior health outcomes and schedules a full recheck, same as a
// CheckURL change, because every cached verdict was made under the old policy.
func (s *StatusServer) handleCheckOptions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		timeout, maxConcurrent, maxCandidates, requireIPChange := s.effectiveCheckOptions()
		sourceRefreshInterval, fullRecheckInterval := s.effectiveScheduleIntervals()
		storedConcurrent, storedTimeoutSeconds, storedCandidates, storedSourceRefresh, storedFullRecheck, storedRequire := 0, 0, 0, 0, 0, interface{}("default")
		if s.store != nil {
			var requireOverride *bool
			storedConcurrent, storedTimeoutSeconds, storedCandidates, requireOverride = s.store.CheckOptionsOverride()
			storedSourceRefresh, storedFullRecheck = s.store.ScheduleIntervalsOverride()
			if requireOverride != nil {
				storedRequire = *requireOverride
			}
		}
		writeJSON(w, map[string]interface{}{
			"max_concurrent":                  maxConcurrent,
			"check_timeout_seconds":           int(timeout / time.Second),
			"max_candidates":                  maxCandidates,
			"source_refresh_interval_seconds": int(sourceRefreshInterval / time.Second),
			"full_recheck_interval_seconds":   int(fullRecheckInterval / time.Second),
			"require_ip_change":               requireIPChange,
			"overrides": map[string]interface{}{
				"max_concurrent":                  storedConcurrent,
				"check_timeout_seconds":           storedTimeoutSeconds,
				"max_candidates":                  storedCandidates,
				"source_refresh_interval_seconds": storedSourceRefresh,
				"full_recheck_interval_seconds":   storedFullRecheck,
				"require_ip_change":               storedRequire,
			},
		})
	case http.MethodPost:
		var in checkOptionsPayload
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		var requireOverride *bool
		clearRequireOverride := false
		policySupplied := false
		switch v := in.RequireIPChange.(type) {
		case nil:
			// Key absent or explicit null: leave the stored override untouched.
		case bool:
			requireOverride = &v
			policySupplied = true
		case string:
			if !strings.EqualFold(strings.TrimSpace(v), "default") {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("require_ip_change must be a boolean or \"default\""))
				return
			}
			clearRequireOverride = true
			policySupplied = true
		default:
			writeErr(w, http.StatusBadRequest, fmt.Errorf("require_ip_change must be a boolean or \"default\""))
			return
		}
		if err := s.store.updateRuntimeOptions(runtimeOptionsUpdate{
			maxConcurrent:                in.MaxConcurrent,
			checkTimeoutSeconds:          in.CheckTimeoutSeconds,
			maxCandidates:                in.MaxCandidates,
			sourceRefreshIntervalSeconds: in.SourceRefreshIntervalSeconds,
			fullRecheckIntervalSeconds:   in.FullRecheckIntervalSeconds,
			requireIPChange:              requireOverride,
			clearRequireIPChange:         clearRequireOverride,
		}); err != nil {
			writeConfigStoreError(w, err)
			return
		}
		// Numeric options need no invalidation; the next cycle picks them up.
		// A policy flip changes the meaning of every cached health verdict.
		policyChanged := false
		baselineRefreshAttempted := false
		baselineRefreshed := false
		baselineChanged := false
		recheck := HealthRecheckOperation{}
		accepted := false
		if policySupplied {
			_, _, _, effectiveRequire := s.effectiveCheckOptions()
			if effectiveRequire && !s.pool.RequireIPChangePolicy() {
				baselineRefreshAttempted = true
				refreshTimeout, _, _, _ := s.effectiveCheckOptions()
				baselineRefreshed, baselineChanged = refreshBaselineForStatus(r.Context(), refreshTimeout)
			}
			if s.pool.SetRequireIPChangePolicy(effectiveRequire) {
				policyChanged = true
				s.pool.InvalidateHealth(s.store.CheckURL())
				s.pool.candidates.ResetHealthOutcomes()
				if err := s.pool.FlushCache(); err != nil {
					writeErrCode(w, http.StatusInternalServerError, "check_options_not_durable", fmt.Errorf("check options change was not persisted: %w", err))
					return
				}
				recheck, accepted = s.coordinator.triggerFullRecheck(s.pool)
			}
		}
		newTimeout, newConcurrent, newCandidates, newRequire := s.effectiveCheckOptions()
		newSourceRefresh, newFullRecheck := s.effectiveScheduleIntervals()
		writeJSON(w, map[string]interface{}{
			"status":                          "ok",
			"max_concurrent":                  newConcurrent,
			"check_timeout_seconds":           int(newTimeout / time.Second),
			"max_candidates":                  newCandidates,
			"source_refresh_interval_seconds": int(newSourceRefresh / time.Second),
			"full_recheck_interval_seconds":   int(newFullRecheck / time.Second),
			"require_ip_change":               newRequire,
			"policy_changed":                  policyChanged,
			"baseline_refresh_attempted":      baselineRefreshAttempted,
			"baseline_refreshed":              baselineRefreshed,
			"baseline_changed":                baselineChanged,
			"baseline_ip":                     BaselineExitIP(),
			"health_recheck":                  recheck,
			"accepted":                        accepted,
		})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
	}
}

// handleBaselineExit reports or refreshes the baseline direct-egress IP used
// by the require-ip-change policy and the anonymity probe.
func (s *StatusServer) handleBaselineExit(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, map[string]string{"baseline_ip": BaselineExitIP()})
	case http.MethodPost:
		timeout, _, _, _ := s.effectiveCheckOptions()
		success, changed := refreshBaselineForStatus(r.Context(), timeout)
		policyChanged := false
		recheck := HealthRecheckOperation{}
		accepted := false
		if success && changed && s.pool.RequireIPChangePolicy() && s.pool.SetRequireIPChangePolicy(true) {
			policyChanged = true
			_, checkURL := s.pool.HealthCriterion()
			if s.store != nil {
				checkURL = s.store.CheckURL()
			}
			s.pool.InvalidateHealth(checkURL)
			s.pool.candidates.ResetHealthOutcomes()
			if err := s.pool.FlushCache(); err != nil {
				writeErrCode(w, http.StatusInternalServerError, "baseline_not_durable", fmt.Errorf("baseline policy change was not persisted: %w", err))
				return
			}
			recheck, accepted = s.coordinator.triggerFullRecheck(s.pool)
		}
		writeJSON(w, map[string]interface{}{
			"success":        success,
			"changed":        changed,
			"baseline_ip":    BaselineExitIP(),
			"policy_changed": policyChanged,
			"health_recheck": recheck,
			"accepted":       accepted,
		})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
	}
}

// handleNodeRotate advances the default (ANY) group to its next node, even
// when the group is manually pinned - the pin is kept, only the position
// moves. This is the operator's "next node" action.
func (s *StatusServer) handleNodeRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	next, ok := s.pool.RotateStickyForced(GroupAny)
	if !ok {
		writeErrCode(w, http.StatusConflict, "no_routable_node", fmt.Errorf("no routable node available to rotate to"))
		return
	}
	view := nodeViewOf(next, next.Key())
	writeJSON(w, map[string]interface{}{"status": "ok", "node": view})
}

// handleNodeStats returns the full per-node stats record for the dashboard's
// per-node diagnostics view.
func (s *StatusServer) handleNodeStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("missing node key"))
		return
	}
	stats, available, ok := s.pool.StatsDetailOf(key)
	if !ok {
		writeErrCode(w, http.StatusNotFound, "node_not_found", fmt.Errorf("node not found: %s", key))
		return
	}
	lastSuccess := ""
	if !stats.LastHealthSuccessAt.IsZero() {
		lastSuccess = stats.LastHealthSuccessAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, map[string]interface{}{
		"key":                         key,
		"successes":                   stats.Successes,
		"failures":                    stats.Failures,
		"consecutive_health_failures": stats.ConsecutiveHealthFailures,
		"health_failure_terminal":     stats.HealthFailureTerminal,
		"last_health_success_at":      lastSuccess,
		"last_latency_ms":             stats.LastLatencyMs,
		"available":                   available,
	})
}
