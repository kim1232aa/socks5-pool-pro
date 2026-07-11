package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyIPVerifyUsesCatalogTargetAndReturnsRedactedResult(t *testing.T) {
	var requests atomic.Int64
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.URL.Query().Get("proxyip"); got != "150.230.212.247" {
			t.Errorf("probe proxyip query = %q, want bare catalog IP", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("probe Accept = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"success":true,"responseTime":228,"supports_ipv4":false,"supports_ipv6":true,
			"probe_results":{"ipv6":{"exit":{"ip":"2001:db8::1","country":"JP"}}},
			"exit":{"ip":"must-not-leak"}
		}`)
	}))
	t.Cleanup(probe.Close)
	restoreProxyIPVerifyEndpoint(t, probe.URL)

	pool := NewProxyPool()
	resource := Proxy{IP: "150.230.212.247", Port: "443", Protocol: "proxyip", Country: "JP", SourceName: "resource-feed"}
	forwarding := Proxy{IP: "198.51.100.20", Port: "1080", Protocol: "socks5", Available: true}
	pool.Prime([]Proxy{forwarding}, nil)
	seedProxyIPVerifyCatalog(pool, []Proxy{resource})
	server := NewStatusServer(pool, &ConfigStore{})

	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodPost, "/api/proxyip/verify", bytes.NewBufferString(`{"key":"proxyip://150.230.212.247:443"}`))
	server.handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST verify status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if requests.Load() != 1 {
		t.Fatalf("probe requests = %d, want 1", requests.Load())
	}

	var wire map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	wantFields := map[string]bool{
		"success": true, "response_time_ms": true, "supports_ipv4": true,
		"supports_ipv6": true, "source": true, "checked_at": true,
	}
	if len(wire) != len(wantFields) {
		t.Fatalf("verify response fields = %v", sortedJSONKeys(wire))
	}
	for field := range wire {
		if !wantFields[field] {
			t.Fatalf("unexpected verify response field %q", field)
		}
	}
	var result proxyIPVerifyResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.ResponseTimeMs != 228 || result.SupportsIPv4 || !result.SupportsIPv6 || result.Source != proxyIPVerifySource {
		t.Fatalf("verify result = %#v", result)
	}
	if _, err := time.Parse(time.RFC3339Nano, result.CheckedAt); err != nil {
		t.Fatalf("checked_at = %q: %v", result.CheckedAt, err)
	}
	if strings.Contains(recorder.Body.String(), "probe_results") || strings.Contains(recorder.Body.String(), "must-not-leak") || strings.Contains(recorder.Body.String(), "2001:db8") {
		t.Fatalf("verify response leaked upstream probe detail: %s", recorder.Body.String())
	}
	if pool.Size() != 1 || pool.All()[0].Key() != forwarding.Key() || !pool.All()[0].Available {
		t.Fatalf("verification changed forwarding pool: %#v", pool.All())
	}
	snapshot := pool.candidates.snapshot.Load()
	snapshot.mu.RLock()
	status := snapshot.records[snapshot.find("proxyip", resource.Addr())].status
	snapshot.mu.RUnlock()
	if status != candidateResource {
		t.Fatalf("verification changed candidate status to %s", status.String())
	}
}

func TestProxyIPVerifyRejectsNonCatalogAndNon443TargetsWithoutOutboundRequest(t *testing.T) {
	var requests atomic.Int64
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"success":true,"responseTime":1,"supports_ipv4":true,"supports_ipv6":false}`)
	}))
	t.Cleanup(probe.Close)
	restoreProxyIPVerifyEndpoint(t, probe.URL)

	pool := NewProxyPool()
	seedProxyIPVerifyCatalog(pool, []Proxy{
		{IP: "192.0.2.10", Port: "443", Protocol: "proxyip"},
		{IP: "192.0.2.11", Port: "80", Protocol: "proxyip"},
		{IP: "proxy.example", Port: "443", Protocol: "proxyip"},
	})
	handler := NewStatusServer(pool, &ConfigStore{}).handler()
	tests := []struct {
		name string
		body string
	}{
		{name: "missing target", body: `{}`},
		{name: "unknown field", body: `{"key":"proxyip://192.0.2.10:443","url":"http://127.0.0.1"}`},
		{name: "forwarding protocol", body: `{"key":"http://192.0.2.10:443"}`},
		{name: "not in catalog", body: `{"address":"127.0.0.1:443"}`},
		{name: "catalog wrong port", body: `{"address":"192.0.2.11:80"}`},
		{name: "catalog hostname", body: `{"address":"proxy.example:443"}`},
		{name: "key address mismatch", body: `{"key":"proxyip://192.0.2.10:443","address":"192.0.2.99:443"}`},
		{name: "trailing JSON", body: `{"address":"192.0.2.10:443"} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := localTestRequest(http.MethodPost, "/api/proxyip/verify", strings.NewReader(test.body))
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("rejected targets caused %d outbound requests", requests.Load())
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/proxyip/verify", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET verify status = %d, want 405", recorder.Code)
	}
}

func TestProxyIPVerifyRejectsBadExternalResponsesWithoutStateMutation(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{name: "non 2xx", status: http.StatusServiceUnavailable, body: `{"success":true,"responseTime":1}`},
		{name: "invalid JSON", status: http.StatusOK, body: `{not-json`},
		{name: "wrong content type", status: http.StatusOK, contentType: "text/plain", body: `{"success":false,"responseTime":1,"supports_ipv4":false,"supports_ipv6":false}`},
		{name: "missing boolean fields", status: http.StatusOK, body: `{"success":false,"responseTime":1}`},
		{name: "missing response time", status: http.StatusOK, body: `{"success":false,"supports_ipv4":false,"supports_ipv6":false}`},
		{name: "negative response time", status: http.StatusOK, body: `{"success":false,"responseTime":-1,"supports_ipv4":false,"supports_ipv6":false}`},
		{name: "oversize", status: http.StatusOK, body: `{"success":true,"responseTime":1,"padding":"` + strings.Repeat("x", maxProxyIPVerifyBodyBytes) + `"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				contentType := test.contentType
				if contentType == "" {
					contentType = "application/json"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			t.Cleanup(probe.Close)
			restoreProxyIPVerifyEndpoint(t, probe.URL)
			pool := NewProxyPool()
			resource := Proxy{IP: "192.0.2.30", Port: "443", Protocol: "proxyip"}
			seedProxyIPVerifyCatalog(pool, []Proxy{resource})
			server := NewStatusServer(pool, &ConfigStore{})

			recorder := httptest.NewRecorder()
			request := localTestRequest(http.MethodPost, "/api/proxyip/verify", strings.NewReader(`{"address":"192.0.2.30:443"}`))
			server.handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%s", recorder.Code, recorder.Body.String())
			}
			if pool.Size() != 0 {
				t.Fatalf("failed external verification changed pool: %#v", pool.All())
			}
			snapshot := pool.candidates.snapshot.Load()
			snapshot.mu.RLock()
			status := snapshot.records[snapshot.find("proxyip", resource.Addr())].status
			snapshot.mu.RUnlock()
			if status != candidateResource {
				t.Fatalf("failed verification changed candidate status to %s", status.String())
			}
		})
	}
}

func TestProxyIPVerifyDeduplicatesKeysAndLimitsOutboundConcurrencyToEight(t *testing.T) {
	entered := make(chan struct{}, 9)
	release := make(chan struct{})
	var calls, active, maxActive atomic.Int64
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		current := active.Add(1)
		for {
			previous := maxActive.Load()
			if current <= previous || maxActive.CompareAndSwap(previous, current) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		active.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":false,"responseTime":1,"supports_ipv4":false,"supports_ipv6":false}`)
	}))
	t.Cleanup(probe.Close)
	restoreProxyIPVerifyEndpoint(t, probe.URL)

	resources := make([]Proxy, 9)
	for i := range resources {
		resources[i] = Proxy{IP: "192.0.2." + strconv.Itoa(i+1), Port: "443", Protocol: "proxyip"}
	}
	pool := NewProxyPool()
	seedProxyIPVerifyCatalog(pool, resources)
	server := NewStatusServer(pool, &ConfigStore{})

	type requestResult struct {
		status int
		body   string
	}
	results := make(chan requestResult, 10)
	var workers sync.WaitGroup
	start := func(resource Proxy) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			recorder := httptest.NewRecorder()
			body := `{"key":"` + resource.Key() + `"}`
			server.handleProxyIPVerify(recorder, localTestRequest(http.MethodPost, "/api/proxyip/verify", strings.NewReader(body)))
			results <- requestResult{status: recorder.Code, body: recorder.Body.String()}
		}()
	}
	for i := 0; i < maxProxyIPVerifyConcurrent; i++ {
		start(resources[i])
	}
	for i := 0; i < maxProxyIPVerifyConcurrent; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("eight verification calls did not reach probe")
		}
	}
	start(resources[0]) // duplicate: must join the existing call
	start(resources[8]) // distinct ninth: must fail fast instead of queueing unbounded work
	time.Sleep(40 * time.Millisecond)
	if got := calls.Load(); got != maxProxyIPVerifyConcurrent {
		close(release)
		t.Fatalf("outbound calls before slot release = %d, want 8", got)
	}

	close(release)
	workers.Wait()
	close(results)
	tooMany := 0
	for result := range results {
		if result.status == http.StatusTooManyRequests {
			tooMany++
			continue
		}
		if result.status != http.StatusOK {
			t.Fatalf("concurrent verify status = %d: %s", result.status, result.body)
		}
	}
	if tooMany != 1 {
		t.Fatalf("429 responses = %d, want one distinct over-capacity request", tooMany)
	}
	if got := calls.Load(); got != maxProxyIPVerifyConcurrent {
		t.Fatalf("outbound calls = %d, want %d bounded distinct keys", got, maxProxyIPVerifyConcurrent)
	}
	if got := maxActive.Load(); got > maxProxyIPVerifyConcurrent {
		t.Fatalf("max outbound concurrency = %d, want <= 8", got)
	}
	if got := len(server.proxyIPVerifySlots); got != 0 {
		t.Fatalf("occupied verification slots after completion = %d", got)
	}
	server.proxyIPVerifyMu.Lock()
	defer server.proxyIPVerifyMu.Unlock()
	if got := len(server.proxyIPVerifyRunning); got != 0 {
		t.Fatalf("in-flight verification entries after completion = %d", got)
	}
}

func TestProxyIPVerifyCancellationReleasesSlotAndInFlightKey(t *testing.T) {
	started := make(chan struct{})
	probe := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(probe.Close)
	restoreProxyIPVerifyEndpoint(t, probe.URL)

	pool := NewProxyPool()
	resource := Proxy{IP: "192.0.2.40", Port: "443", Protocol: "proxyip"}
	seedProxyIPVerifyCatalog(pool, []Proxy{resource})
	server := NewStatusServer(pool, &ConfigStore{})
	ctx, cancel := context.WithCancel(context.Background())
	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodPost, "/api/proxyip/verify", strings.NewReader(`{"key":"proxyip://192.0.2.40:443"}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		server.handleProxyIPVerify(recorder, request)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("verification did not reach probe")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("verification handler did not return after cancellation")
	}
	if recorder.Code != http.StatusRequestTimeout {
		t.Fatalf("canceled verify status = %d, want 408; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := len(server.proxyIPVerifySlots); got != 0 {
		t.Fatalf("occupied verification slots after cancellation = %d", got)
	}
	server.proxyIPVerifyMu.Lock()
	if got := len(server.proxyIPVerifyRunning); got != 0 {
		server.proxyIPVerifyMu.Unlock()
		t.Fatalf("in-flight keys after cancellation = %d", got)
	}
	server.proxyIPVerifyMu.Unlock()
}

func seedProxyIPVerifyCatalog(pool *ProxyPool, resources []Proxy) {
	refresh := pool.candidates.begin(resources, nil, nil, 0)
	pool.candidates.complete(refresh, nil, nil, nil)
}

func restoreProxyIPVerifyEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	previous := proxyIPVerifyEndpoint
	proxyIPVerifyEndpoint = endpoint
	t.Cleanup(func() { proxyIPVerifyEndpoint = previous })
}

func sortedJSONKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
