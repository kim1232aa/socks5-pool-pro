package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// invokeNodeSpeedtestBatch posts to /api/nodes/speedtest/batch and decodes the
// response. It mirrors the candidate speedtest test helper but targets the
// node-pool batch endpoint.
func invokeNodeSpeedtestBatch(t *testing.T, server *StatusServer, keys []string, ctx context.Context) struct {
	status int
	body   nodeSpeedtestBatchResponse
	raw    string
} {
	t.Helper()
	payload, err := json.Marshal(nodeSpeedtestBatchRequest{Keys: keys})
	if err != nil {
		t.Fatalf("marshal node speedtest batch request: %v", err)
	}
	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodPost, "/api/nodes/speedtest/batch", bytes.NewReader(payload)).WithContext(ctx)
	server.handler().ServeHTTP(recorder, request)
	result := struct {
		status int
		body   nodeSpeedtestBatchResponse
		raw    string
	}{status: recorder.Code, raw: recorder.Body.String()}
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &result.body); err != nil {
			t.Fatalf("decode node speedtest batch response: %v; body=%s", err, recorder.Body.String())
		}
	}
	return result
}

// speedTestTargetServer returns a test HTTP server that serves a fixed
// speedTestMaxBytes payload, suitable as a speedtest download target.
func speedTestTargetServer(t *testing.T) *httptest.Server {
	t.Helper()
	payload := bytes.Repeat([]byte{'x'}, speedTestMaxBytes)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(target.Close)
	return target
}

// httpProxyFromTestServer converts a httptest.Server URL into a Proxy struct
// with protocol "http", suitable for seeding into a ProxyPool.
func httpProxyFromTestServer(t *testing.T, serverURL string) Proxy {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse proxy url %q: %v", serverURL, err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host:port %q: %v", u.Host, err)
	}
	return Proxy{IP: host, Port: port, Protocol: "http"}
}

func TestNodeSpeedtestBatchSuccess(t *testing.T) {
	target := speedTestTargetServer(t)
	restoreSpeedTestURL(t, target.URL)

	proxies := []Proxy{
		newSpeedTestConnectProxy(t),
		newSpeedTestConnectProxy(t),
	}
	for i := range proxies {
		proxies[i].Available = true
	}
	pool := NewProxyPool()
	pool.Prime(proxies, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	keys := []string{proxies[0].Key(), proxies[1].Key()}
	result := invokeNodeSpeedtestBatch(t, server, keys, context.Background())
	if result.status != http.StatusOK {
		t.Fatalf("batch speedtest = %d %s", result.status, result.raw)
	}
	if len(result.body.Results) != 2 {
		t.Fatalf("result count = %d, want 2", len(result.body.Results))
	}
	for i, item := range result.body.Results {
		if item.Key != keys[i] {
			t.Fatalf("result[%d].Key = %q, want %q", i, item.Key, keys[i])
		}
		if !item.OK || item.Bytes != speedTestMaxBytes || item.Error != nil {
			t.Fatalf("result[%d] = %#v, want OK with %d bytes", i, item, speedTestMaxBytes)
		}
		if item.Kbps <= 0 || item.DurationMs <= 0 {
			t.Fatalf("result[%d] has zero metrics: %#v", i, item)
		}
	}
	// Verify the pool persisted the speed results.
	for _, px := range proxies {
		got, ok := pool.Find(px.Key())
		if !ok {
			t.Fatalf("pool.Find after batch = %v", ok)
		}
		if got.SpeedBytes != speedTestMaxBytes || got.SpeedTestedAt == 0 {
			t.Fatalf("pool speed for %s = %#v, want persisted result", px.Key(), got)
		}
	}
}

func TestNodeSpeedtestBatchOverLimit(t *testing.T) {
	server := NewStatusServer(NewProxyPool(), &ConfigStore{})

	keys := make([]string, maxConcurrentNodeSpeedTests+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("socks5://198.51.100.%d:1080", i+1)
	}
	result := invokeNodeSpeedtestBatch(t, server, keys, context.Background())
	if result.status != http.StatusBadRequest || !strings.Contains(result.raw, "node_speedtest_batch_too_large") {
		t.Fatalf("over-limit batch = %d %s", result.status, result.raw)
	}
}

func TestNodeSpeedtestBatchEmptyKeys(t *testing.T) {
	server := NewStatusServer(NewProxyPool(), &ConfigStore{})

	// Empty array
	result := invokeNodeSpeedtestBatch(t, server, []string{}, context.Background())
	if result.status != http.StatusBadRequest || !strings.Contains(result.raw, "invalid_node_speedtest_batch_request") {
		t.Fatalf("empty keys = %d %s", result.status, result.raw)
	}

	// Whitespace-only key
	whitespace := invokeNodeSpeedtestBatch(t, server, []string{"  "}, context.Background())
	if whitespace.status != http.StatusBadRequest || !strings.Contains(whitespace.raw, "invalid_node_speedtest_batch_request") {
		t.Fatalf("whitespace key = %d %s", whitespace.status, whitespace.raw)
	}

	// Missing "keys" field entirely
	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodPost, "/api/nodes/speedtest/batch", bytes.NewBufferString(`{}`))
	server.handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_node_speedtest_batch_request") {
		t.Fatalf("missing keys field = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestNodeSpeedtestBatchKeyNotInPool(t *testing.T) {
	target := speedTestTargetServer(t)
	restoreSpeedTestURL(t, target.URL)

	// Seed one real node, but request a non-existent key alongside it.
	real := newSpeedTestConnectProxy(t)
	real.Available = true
	pool := NewProxyPool()
	pool.Prime([]Proxy{real}, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	missingKey := "socks5://198.51.100.99:1080"
	result := invokeNodeSpeedtestBatch(t, server, []string{real.Key(), missingKey}, context.Background())
	if result.status != http.StatusOK {
		t.Fatalf("batch with missing key = %d %s", result.status, result.raw)
	}
	if len(result.body.Results) != 2 {
		t.Fatalf("result count = %d, want 2", len(result.body.Results))
	}
	// The order follows the request order.
	if result.body.Results[0].Key != real.Key() {
		t.Fatalf("result[0].Key = %q, want %q", result.body.Results[0].Key, real.Key())
	}
	if result.body.Results[1].Key != missingKey {
		t.Fatalf("result[1].Key = %q, want %q", result.body.Results[1].Key, missingKey)
	}
	if !result.body.Results[0].OK || result.body.Results[0].Error != nil {
		t.Fatalf("real node result = %#v, want OK", result.body.Results[0])
	}
	if result.body.Results[1].OK || result.body.Results[1].Error == nil || result.body.Results[1].Error.Code != "node_not_found" {
		t.Fatalf("missing node result = %#v, want node_not_found", result.body.Results[1])
	}
}

func TestNodeSpeedtestBatchConcurrencyLimit(t *testing.T) {
	target := speedTestTargetServer(t)
	restoreSpeedTestURL(t, target.URL)

	// Seed exactly maxConcurrentNodeSpeedTests nodes.
	proxies := make([]Proxy, maxConcurrentNodeSpeedTests)
	for i := range proxies {
		proxies[i] = newSpeedTestConnectProxy(t)
		proxies[i].Available = true
	}
	pool := NewProxyPool()
	pool.Prime(proxies, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	// Manually occupy all speed slots so the batch workers cannot acquire any.
	for i := 0; i < maxConcurrentNodeSpeedTests; i++ {
		if err := server.beginSpeedTest(proxies[i].Key()); err != nil {
			t.Fatalf("beginSpeedTest(%d) = %v", i, err)
		}
	}

	keys := make([]string, maxConcurrentNodeSpeedTests)
	for i, px := range proxies {
		keys[i] = px.Key()
	}
	result := invokeNodeSpeedtestBatch(t, server, keys, context.Background())
	if result.status != http.StatusOK {
		t.Fatalf("batch with all slots busy = %d %s", result.status, result.raw)
	}
	for i, item := range result.body.Results {
		if item.OK || item.Error == nil || (item.Error.Code != "node_speedtest_busy" && item.Error.Code != "node_speedtest_cooldown") {
			t.Fatalf("result[%d] = %#v, want busy/cooldown", i, item)
		}
	}

	// Release all slots.
	for _, px := range proxies {
		server.endSpeedTest(px.Key(), false)
	}
	if got := len(server.speedSlots); got != 0 {
		t.Fatalf("speed slots after release = %d, want 0", got)
	}
}

func TestNodeSpeedtestBatchDoesNotLeakCredentials(t *testing.T) {
	target := speedTestTargetServer(t)
	restoreSpeedTestURL(t, target.URL)

	// Create a node with sensitive credentials that should never appear in the
	// response body. The Proxy carries the credentials; the key (scheme://host:port)
	// never includes them.
	proxy, _ := newSpeedTestConnectProxyWithAuth(t, "secret-user", "super-secret-pass")
	proxy.Username = "secret-user"
	proxy.Password = "super-secret-pass"
	proxy.Available = true
	pool := NewProxyPool()
	pool.Prime([]Proxy{proxy}, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	result := invokeNodeSpeedtestBatch(t, server, []string{proxy.Key()}, context.Background())
	if result.status != http.StatusOK {
		t.Fatalf("batch speedtest = %d %s", result.status, result.raw)
	}
	if len(result.body.Results) != 1 || !result.body.Results[0].OK {
		t.Fatalf("batch result = %#v", result.body)
	}
	// The key must be the credential-free canonical URL.
	if strings.Contains(result.body.Results[0].Key, "secret-user") || strings.Contains(result.body.Results[0].Key, "super-secret-pass") {
		t.Fatalf("result key leaks credentials: %q", result.body.Results[0].Key)
	}
	// The entire raw response must not contain any credential fragment.
	for _, secret := range []string{"secret-user", "super-secret-pass"} {
		if strings.Contains(result.raw, secret) {
			t.Fatalf("response body leaks credential %q: %s", secret, result.raw)
		}
	}

	// Also verify the error path does not leak credentials: request a
	// non-existent key that contains a credential-like fragment.
	missingKey := "socks5://secret-user:super-secret-pass@198.51.100.99:1080"
	errResult := invokeNodeSpeedtestBatch(t, server, []string{missingKey}, context.Background())
	if errResult.status != http.StatusOK {
		t.Fatalf("batch with credential-bearing missing key = %d %s", errResult.status, errResult.raw)
	}
	if len(errResult.body.Results) != 1 {
		t.Fatalf("result count = %d, want 1", len(errResult.body.Results))
	}
	item := errResult.body.Results[0]
	if item.OK || item.Error == nil || item.Error.Code != "node_not_found" {
		t.Fatalf("missing key result = %#v, want node_not_found", item)
	}
	// The response must not contain the credential fragments anywhere.
	for _, secret := range []string{"secret-user", "super-secret-pass"} {
		if strings.Contains(errResult.raw, secret) {
			t.Fatalf("error response leaks credential %q: %s", secret, errResult.raw)
		}
	}
}

func TestNodeSpeedtestBatchDeduplicatesKeys(t *testing.T) {
	target := speedTestTargetServer(t)
	restoreSpeedTestURL(t, target.URL)

	proxy := newSpeedTestConnectProxy(t)
	proxy.Available = true
	pool := NewProxyPool()
	pool.Prime([]Proxy{proxy}, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	// Send the same key three times; only one result should come back.
	result := invokeNodeSpeedtestBatch(t, server, []string{proxy.Key(), proxy.Key(), proxy.Key()}, context.Background())
	if result.status != http.StatusOK {
		t.Fatalf("deduplicated batch = %d %s", result.status, result.raw)
	}
	if len(result.body.Results) != 1 {
		t.Fatalf("result count = %d, want 1 after dedup", len(result.body.Results))
	}
	if !result.body.Results[0].OK {
		t.Fatalf("deduplicated result = %#v, want OK", result.body.Results[0])
	}
}

func TestNodeSpeedtestBatchRejectsGET(t *testing.T) {
	server := NewStatusServer(NewProxyPool(), &ConfigStore{})
	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodGet, "/api/nodes/speedtest/batch", nil)
	server.handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET batch speedtest = %d Allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}
}

func TestNodeSpeedtestBatchPartialFailureDoesNotCancelPeers(t *testing.T) {
	target := speedTestTargetServer(t)
	restoreSpeedTestURLs(t, target.URL, "http://speed-fallback.invalid/blocked")

	// A "bad" node whose proxy rejects CONNECT, causing a speedtest failure.
	badProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refused", http.StatusForbidden)
	}))
	t.Cleanup(badProxy.Close)
	badNode := httpProxyFromTestServer(t, badProxy.URL)
	badNode.Available = true

	good1 := newSpeedTestConnectProxy(t)
	good1.Available = true
	good2 := newSpeedTestConnectProxy(t)
	good2.Available = true

	pool := NewProxyPool()
	pool.Prime([]Proxy{good1, badNode, good2}, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	result := invokeNodeSpeedtestBatch(t, server, []string{good1.Key(), badNode.Key(), good2.Key()}, context.Background())
	if result.status != http.StatusOK {
		t.Fatalf("partial failure batch = %d %s", result.status, result.raw)
	}
	if len(result.body.Results) != 3 {
		t.Fatalf("result count = %d, want 3", len(result.body.Results))
	}
	if !result.body.Results[0].OK {
		t.Fatalf("good1 result = %#v, want OK", result.body.Results[0])
	}
	if result.body.Results[1].OK || result.body.Results[1].Error == nil {
		t.Fatalf("bad node result = %#v, want error", result.body.Results[1])
	}
	if !result.body.Results[2].OK {
		t.Fatalf("good2 result = %#v, want OK (peer failure should not cancel)", result.body.Results[2])
	}
}

func TestNodeSpeedtestBatchRunsKeysConcurrently(t *testing.T) {
	// Use a target that blocks until released, and verify that all keys start
	// their speedtest concurrently (up to maxConcurrentNodeSpeedTests slots).
	entered := make(chan struct{}, maxConcurrentNodeSpeedTests)
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		payload := bytes.Repeat([]byte{'x'}, speedTestMaxBytes)
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(target.Close)
	restoreSpeedTestURL(t, target.URL)

	const count = 4
	proxies := make([]Proxy, count)
	for i := range proxies {
		proxies[i] = newSpeedTestConnectProxy(t)
		proxies[i].Available = true
	}
	pool := NewProxyPool()
	pool.Prime(proxies, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	keys := make([]string, count)
	for i, px := range proxies {
		keys[i] = px.Key()
	}

	type batchResult struct {
		status int
		body   nodeSpeedtestBatchResponse
		raw    string
	}
	done := make(chan batchResult, 1)
	go func() {
		r := invokeNodeSpeedtestBatch(t, server, keys, context.Background())
		done <- batchResult{r.status, r.body, r.raw}
	}()

	// Wait for all workers to enter the speedtest target.
	timeout := time.After(5 * time.Second)
	for i := 0; i < count; i++ {
		select {
		case <-entered:
		case <-timeout:
			close(release)
			t.Fatalf("only %d/%d workers started concurrently", i, count)
		}
	}

	close(release)
	select {
	case r := <-done:
		if r.status != http.StatusOK {
			t.Fatalf("concurrent batch = %d %s", r.status, r.raw)
		}
		if len(r.body.Results) != count {
			t.Fatalf("result count = %d, want %d", len(r.body.Results), count)
		}
		for i, item := range r.body.Results {
			if !item.OK || item.Error != nil {
				t.Fatalf("result[%d] = %#v, want OK", i, item)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent batch did not complete after release")
	}
}

func TestNodeSpeedtestBatchReleasesSlotsAfterCompletion(t *testing.T) {
	target := speedTestTargetServer(t)
	restoreSpeedTestURL(t, target.URL)

	proxy := newSpeedTestConnectProxy(t)
	proxy.Available = true
	pool := NewProxyPool()
	pool.Prime([]Proxy{proxy}, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	result := invokeNodeSpeedtestBatch(t, server, []string{proxy.Key()}, context.Background())
	if result.status != http.StatusOK {
		t.Fatalf("batch speedtest = %d %s", result.status, result.raw)
	}

	// After the batch completes, all speed slots must be released.
	if got := len(server.speedSlots); got != 0 {
		t.Fatalf("occupied speed slots after batch = %d, want 0", got)
	}
	server.speedMu.Lock()
	running := len(server.speedRunning)
	server.speedMu.Unlock()
	if running != 0 {
		t.Fatalf("running speedtests after batch = %d, want 0", running)
	}

	// The successful node should be in cooldown (matching the single-node
	// handler's behavior), but a different node should still be testable.
	proxy2 := newSpeedTestConnectProxy(t)
	proxy2.Available = true
	pool.Prime([]Proxy{proxy2}, nil)
	if err := server.beginSpeedTest(proxy2.Key()); err != nil {
		t.Fatalf("second node speedtest after batch = %v", err)
	}
	server.endSpeedTest(proxy2.Key(), false)
}

func TestNodeSpeedtestBatchRespectsRequestCancellation(t *testing.T) {
	// A target that blocks forever until the request context is cancelled.
	started := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(target.Close)
	restoreSpeedTestURL(t, target.URL)

	proxy := newSpeedTestConnectProxy(t)
	proxy.Available = true
	pool := NewProxyPool()
	pool.Prime([]Proxy{proxy}, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	ctx, cancel := context.WithCancel(context.Background())
	type batchResult struct {
		status int
		body   nodeSpeedtestBatchResponse
		raw    string
	}
	done := make(chan batchResult, 1)
	go func() {
		r := invokeNodeSpeedtestBatch(t, server, []string{proxy.Key()}, ctx)
		done <- batchResult{r.status, r.body, r.raw}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("speedtest target was never reached")
	}

	cancel()
	select {
	case r := <-done:
		// The request-level cancellation should cause the batch to return a
		// 408 request_cancelled, or an item-level error. Either way, the slot
		// must be released.
		_ = r
	case <-time.After(10 * time.Second):
		t.Fatal("batch did not return after request cancellation")
	}

	// Slots must be fully released after cancellation.
	if got := len(server.speedSlots); got != 0 {
		t.Fatalf("occupied speed slots after cancellation = %d, want 0", got)
	}
	server.speedMu.Lock()
	running := len(server.speedRunning)
	server.speedMu.Unlock()
	if running != 0 {
		t.Fatalf("running speedtests after cancellation = %d, want 0", running)
	}
}
