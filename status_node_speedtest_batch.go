package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// nodeSpeedtestBatchRequest is the JSON body for POST /api/nodes/speedtest/batch.
// Up to maxConcurrentNodeSpeedTests (16) distinct pool keys may be measured in
// a single request.
type nodeSpeedtestBatchRequest struct {
	Keys []string `json:"keys"`
}

type nodeSpeedtestBatchItemError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type nodeSpeedtestBatchItem struct {
	Key        string                       `json:"key"`
	OK         bool                         `json:"ok"`
	Kbps       float64                      `json:"kbps,omitempty"`
	Bytes      int64                        `json:"bytes,omitempty"`
	DurationMs int64                        `json:"duration_ms,omitempty"`
	Error      *nodeSpeedtestBatchItemError `json:"error,omitempty"`
}

type nodeSpeedtestBatchResponse struct {
	Results []nodeSpeedtestBatchItem `json:"results"`
}

// handleNodeSpeedtestBatch measures the download throughput of up to
// maxConcurrentNodeSpeedTests pool nodes concurrently. Each key follows the
// same sequence as the single-node /api/nodes/speedtest: pool.Find ->
// beginSpeedTest -> speedTestCredentialCandidatesContext -> UpdateSpeed.
//
// A node that is not found, already running, or in cooldown is reported as a
// per-item error rather than failing the whole batch. The request-level context
// deadline (if any) does not cancel individual workers mid-flight; each worker
// uses its own speedTestOperationTimeout budget so that one slow node cannot
// starve the others.
func (s *StatusServer) handleNodeSpeedtestBatch(w http.ResponseWriter, r *http.Request) {
	var in nodeSpeedtestBatchRequest
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	keys, err := uniqueNodeSpeedtestBatchKeys(in.Keys)
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_node_speedtest_batch_request", err)
		return
	}
	if len(keys) > maxConcurrentNodeSpeedTests {
		writeErrCode(w, http.StatusBadRequest, "node_speedtest_batch_too_large", fmt.Errorf("最多可同时测速 %d 个不同节点", maxConcurrentNodeSpeedTests))
		return
	}

	results := make([]nodeSpeedtestBatchItem, len(keys))
	var workers sync.WaitGroup
	for i, key := range keys {
		workers.Add(1)
		go func(i int, key string) {
			defer workers.Done()
			results[i] = s.speedtestNodeBatch(r.Context(), key)
		}(i, key)
	}
	workers.Wait()
	if err := r.Context().Err(); err != nil {
		writeErrCode(w, http.StatusRequestTimeout, "request_cancelled", err)
		return
	}

	// Persist any successful speed results before replying, mirroring the
	// single-node handler's contract.
	flushErr := s.pool.FlushCache()
	if flushErr != nil {
		writeErrCode(w, http.StatusInternalServerError, "node_speedtest_batch_not_durable", fmt.Errorf("batch speed test results were not fully persisted: %w", flushErr))
		return
	}
	writeJSON(w, nodeSpeedtestBatchResponse{Results: results})
}

// speedtestNodeBatch runs a single node speed test within a batch request.
// The parent context is only used for request-level cancellation; the actual
// measurement uses speedTestCredentialCandidatesContext with its own
// speedTestOperationTimeout budget.
func (s *StatusServer) speedtestNodeBatch(ctx context.Context, key string) nodeSpeedtestBatchItem {
	// The user may supply a key with embedded credentials
	// (socks5://user:pass@host:port). The pool stores only the credential-free
	// canonical form, so strip userinfo before lookup and use the safe form in
	// all response fields and error messages.
	safeKey := redactProxyKey(key)
	item := nodeSpeedtestBatchItem{Key: safeKey}

	healthGeneration := s.pool.HealthGeneration()
	px, ok := s.pool.Find(safeKey)
	if !ok {
		item.Error = nodeSpeedtestBatchError("node_not_found", fmt.Sprintf("node not found: %s", safeKey))
		return item
	}
	if err := s.beginSpeedTest(safeKey); err != nil {
		item.Error = nodeSpeedtestBatchError(nodeSpeedtestBatchBusyCode(err), err.Error())
		return item
	}
	completed := false
	defer func() { s.endSpeedTest(safeKey, completed) }()

	result, verified, err := speedTestCredentialCandidatesContext(ctx, px, speedTestOperationTimeout)
	if ctxErr := ctx.Err(); ctxErr != nil {
		item.Error = nodeSpeedtestBatchError("request_cancelled", ctxErr.Error())
		return item
	}
	if err != nil {
		item.Error = nodeSpeedtestBatchError("speedtest_failed", err.Error())
		return item
	}
	s.pool.UpdateVerifiedCredentialsAtGeneration(safeKey, verified, healthGeneration)
	if !s.pool.UpdateSpeed(safeKey, result.Kbps, result.Bytes, result.DurationMs) {
		item.Error = nodeSpeedtestBatchError("node_disappeared", "node disappeared while speed test was running")
		return item
	}
	completed = true
	item.OK = true
	item.Kbps = result.Kbps
	item.Bytes = result.Bytes
	item.DurationMs = result.DurationMs
	return item
}

// redactProxyKey strips any embedded userinfo from a proxy key URL so that the
// returned value is safe to echo in API responses and error messages. If the
// key cannot be parsed as a URL it is returned unchanged (the pool will simply
// not find it, yielding a credential-free "node not found" error).
func redactProxyKey(key string) string {
	u, err := url.Parse(key)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return key
	}
	u.User = nil
	return u.String()
}

// uniqueNodeSpeedtestBatchKeys validates and deduplicates the incoming key
// list. Empty keys and whitespace-only entries are rejected. The key itself
// is a credential-free canonical URL (scheme://host:port), so returning it in
// the response does not leak credentials.
func uniqueNodeSpeedtestBatchKeys(input []string) ([]string, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("keys must contain at least one node key")
	}
	keys := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, raw := range input {
		key := strings.TrimSpace(raw)
		if key == "" {
			return nil, fmt.Errorf("node key must not be empty")
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, nil
}

func nodeSpeedtestBatchBusyCode(err error) string {
	var cooldown *nodeOperationCooldownError
	if errors.As(err, &cooldown) {
		return "node_speedtest_cooldown"
	}
	return "node_speedtest_busy"
}

func nodeSpeedtestBatchError(code, message string) *nodeSpeedtestBatchItemError {
	return &nodeSpeedtestBatchItemError{Code: code, Message: message}
}
