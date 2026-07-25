package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// candidateSpeedTestContext is replaceable by focused handler tests. The
// production operation retains SpeedTestContext's fixed 1 MiB / 18 second
// contract. A successful explicit operator test promotes the verified
// candidate into the forwarding pool.
var (
	candidateHealthCheckContext = checkURLCredentialCandidatesContext
	candidateSpeedTestContext   = speedTestCredentialCandidatesContext
)

type candidateSpeedtestRequest struct {
	Keys []string `json:"keys"`
}

type candidateSpeedtestItemError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type candidateSpeedtestItem struct {
	Key        string                       `json:"key"`
	OK         bool                         `json:"ok"`
	Kbps       float64                      `json:"kbps,omitempty"`
	Bytes      int64                        `json:"bytes,omitempty"`
	DurationMs int64                        `json:"duration_ms,omitempty"`
	Error      *candidateSpeedtestItemError `json:"error,omitempty"`
}

type candidateSpeedtestResponse struct {
	Results []candidateSpeedtestItem `json:"results"`
}

// handleCandidateSpeedtest measures selected catalog entries concurrently.
// Successful explicit tests are operator-approved admissions to ProxyPool;
// failures remain catalog-only observations.
func (s *StatusServer) handleCandidateSpeedtest(w http.ResponseWriter, r *http.Request) {
	var in candidateSpeedtestRequest
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	keys, err := uniqueCandidateSpeedtestKeys(in.Keys)
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_candidate_speedtest_request", err)
		return
	}
	if len(keys) > maxConcurrentNodeSpeedTests {
		writeErrCode(w, http.StatusBadRequest, "candidate_speedtest_batch_too_large", fmt.Errorf("最多可同时测速 %d 个不同候选", maxConcurrentNodeSpeedTests))
		return
	}

	results := make([]candidateSpeedtestItem, len(keys))
	var workers sync.WaitGroup
	for i, key := range keys {
		workers.Add(1)
		go func(i int, key string) {
			defer workers.Done()
			results[i] = s.speedtestCandidate(r.Context(), key)
		}(i, key)
	}
	workers.Wait()
	if err := r.Context().Err(); err != nil {
		writeErrCode(w, http.StatusRequestTimeout, "request_cancelled", err)
		return
	}
	writeJSON(w, candidateSpeedtestResponse{Results: results})
}

func uniqueCandidateSpeedtestKeys(input []string) ([]string, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("keys must contain at least one candidate key")
	}
	keys := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, raw := range input {
		key := strings.TrimSpace(raw)
		if key == "" {
			return nil, fmt.Errorf("candidate key must not be empty")
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, nil
}

func (s *StatusServer) speedtestCandidate(ctx context.Context, key string) candidateSpeedtestItem {
	item := candidateSpeedtestItem{Key: key}
	px, lease, found := s.pool.candidates.leaseByKey(key)
	if !found || px.Key() != key {
		// Candidate leases exclude ProxyIP resources and non-forwarding
		// protocols, so the not-found path also covers an entry that cannot be
		// dialed as an upstream.
		item.Error = candidateSpeedtestError("candidate_not_found", fmt.Sprintf("candidate not found or not forwardable: %s", key))
		return item
	}
	if err := s.beginCandidateSpeedTest(key); err != nil {
		item.Error = candidateSpeedtestError(candidateSpeedtestBusyCode(err), err.Error())
		return item
	}
	defer s.endSpeedTest(key, false)

	s.coordinator.sourceLifecycleMu.RLock()
	healthGeneration, checkURL, requireIPChange := s.pool.HealthCriterionAndPolicy()
	if checkURL == "" {
		s.pool.SetHealthCriterion(s.store.CheckURL())
		healthGeneration, checkURL, requireIPChange = s.pool.HealthCriterionAndPolicy()
	}
	s.coordinator.sourceLifecycleMu.RUnlock()

	healthCtx, healthCancel := context.WithTimeout(ctx, manualNodeVerifyTotalTimeout)
	defer healthCancel()
	healthVerified, reachable, healthErr := candidateHealthCheckContext(healthCtx, px, checkURL, manualNodeVerifyTotalTimeout)
	if healthErr != nil || !reachable {
		item.Error = candidateSpeedtestError("candidate_healthcheck_failed", "candidate failed current health check criterion before speedtest")
		return item
	}

	exitIP := ""
	if requireIPChange {
		exitIP = probeExitIPContext(healthCtx, healthVerified, manualNodeVerifyExitTimeout)
	}
	policy := evaluateIPChangePolicy(exitIP, BaselineExitIP(), requireIPChange)
	if !policy.PolicyAllowed {
		item.Error = candidateSpeedtestError("candidate_healthcheck_failed", "candidate failed IP change policy before speedtest")
		return item
	}

	result, verified, err := candidateSpeedTestContext(ctx, healthVerified, speedTestOperationTimeout)
	if ctxErr := ctx.Err(); ctxErr != nil {
		item.Error = candidateSpeedtestError("request_cancelled", ctxErr.Error())
		return item
	}
	if err != nil {
		item.Error = candidateSpeedtestError("speedtest_failed", err.Error())
		return item
	}

	s.coordinator.sourceLifecycleMu.Lock()
	ok := s.pool.candidates.withPromotionLease(lease, func(current Proxy) bool {
		verified.SourceName = current.SourceName
		verified.SourceNames = append([]string(nil), current.SourceNames...)
		verified.SourceIDs = append([]string(nil), current.SourceIDs...)
		return s.pool.PromoteCandidateSpeed(verified, s.store.Sources(), healthGeneration, result.Kbps, result.Bytes, result.DurationMs)
	})
	s.coordinator.sourceLifecycleMu.Unlock()

	if !ok {
		item.Error = candidateSpeedtestError("candidate_promotion_failed", "候选在测速完成后无法加入转发池")
		return item
	}
	if err := s.pool.FlushCache(); err != nil {
		item.Error = candidateSpeedtestError("candidate_speedtest_not_durable", fmt.Sprintf("测速结果未持久化: %v", err))
		return item
	}

	item.OK = true
	item.Kbps = result.Kbps
	item.Bytes = result.Bytes
	item.DurationMs = result.DurationMs
	return item
}

func candidateSpeedtestBusyCode(err error) string {
	var cooldown *nodeOperationCooldownError
	if errors.As(err, &cooldown) {
		return "candidate_speedtest_cooldown"
	}
	return "candidate_speedtest_busy"
}

func candidateSpeedtestError(code, message string) *candidateSpeedtestItemError {
	return &candidateSpeedtestItemError{Code: code, Message: message}
}
