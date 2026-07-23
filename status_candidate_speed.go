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
// contract and never promotes an unverified candidate into the forwarding pool.
var candidateSpeedTestContext = speedTestCredentialCandidatesContext

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

// handleCandidateSpeedtest measures selected catalog entries concurrently. It
// intentionally does not call ProxyPool.Update/Prime: a successful measurement
// alone is not a health verification and must never make a candidate routable.
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
	px, found := s.pool.candidates.FindByKey(key)
	if !found || px.Key() != key {
		// FindByKey excludes ProxyIP resources and non-forwarding protocols
		// by returning ok=false, so the not-found path also covers a
		// candidate that cannot be dialed as an upstream.
		item.Error = candidateSpeedtestError("candidate_not_found", fmt.Sprintf("candidate not found or not forwardable: %s", key))
		return item
	}
	if err := s.beginSpeedTest(key); err != nil {
		item.Error = candidateSpeedtestError(candidateSpeedtestBusyCode(err), err.Error())
		return item
	}
	completed := false
	defer func() { s.endSpeedTest(key, completed) }()

	result, _, err := candidateSpeedTestContext(ctx, px, speedTestOperationTimeout)
	if ctxErr := ctx.Err(); ctxErr != nil {
		item.Error = candidateSpeedtestError("request_cancelled", ctxErr.Error())
		return item
	}
	// A completed attempt, successful or not, receives the same-node cooldown.
	// Parent cancellation is the sole exception because the operation was
	// abandoned rather than completed.
	completed = true
	if err != nil {
		item.Error = candidateSpeedtestError("speedtest_failed", err.Error())
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
