package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStatusServerDeduplicatesConcurrentSpeedTestForNode(t *testing.T) {
	server := NewStatusServer(NewProxyPool(), &ConfigStore{})
	const key = "socks5://198.51.100.1:1080"

	if err := server.beginSpeedTest(key); err != nil {
		t.Fatalf("first beginSpeedTest() error = %v", err)
	}
	if err := server.beginSpeedTest(key); err == nil {
		server.endSpeedTest(key)
		t.Fatal("duplicate beginSpeedTest() succeeded, want rejection")
	}
	if got := len(server.speedSlots); got != 1 {
		t.Fatalf("speed slot count after duplicate = %d, want 1", got)
	}

	server.endSpeedTest(key)
	server.speedMu.Lock()
	server.speedCooldownUntil[key] = time.Now().Add(-time.Second)
	server.speedMu.Unlock()
	if err := server.beginSpeedTest(key); err != nil {
		t.Fatalf("beginSpeedTest() after cooldown error = %v", err)
	}
	server.endSpeedTest(key)
}

func TestStatusServerAppliesSameNodeSpeedTestCooldown(t *testing.T) {
	server := NewStatusServer(NewProxyPool(), &ConfigStore{})
	const key = "socks5://198.51.100.2:1080"
	if err := server.beginSpeedTest(key); err != nil {
		t.Fatal(err)
	}
	server.endSpeedTest(key)
	err := server.beginSpeedTest(key)
	var cooldown *nodeOperationCooldownError
	if !errors.As(err, &cooldown) || cooldown.Remaining <= 0 || cooldown.Remaining > nodeSpeedTestCooldown {
		t.Fatalf("speed cooldown error = %#v, want remaining within %s", err, nodeSpeedTestCooldown)
	}
	server.speedMu.Lock()
	server.speedCooldownUntil[key] = time.Now().Add(-time.Second)
	server.speedMu.Unlock()
	if err := server.beginSpeedTest(key); err != nil {
		t.Fatalf("speed test after cooldown = %v", err)
	}
	server.endSpeedTest(key)
}

func TestStatusServerLimitsSpeedTestsToConfiguredGlobalSlots(t *testing.T) {
	server := NewStatusServer(NewProxyPool(), &ConfigStore{})
	attempts := maxConcurrentNodeSpeedTests + 8
	start := make(chan struct{})
	release := make(chan struct{})
	results := make(chan bool, attempts)
	var workers sync.WaitGroup

	for i := range attempts {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			key := "http://198.51.100." + strconv.Itoa(i+1) + ":8080"
			if err := server.beginSpeedTest(key); err != nil {
				results <- false
				return
			}
			results <- true
			<-release
			server.endSpeedTest(key)
		}(i)
	}
	close(start)

	succeeded := 0
	for range attempts {
		if <-results {
			succeeded++
		}
	}
	if succeeded != maxConcurrentNodeSpeedTests {
		close(release)
		workers.Wait()
		t.Fatalf("simultaneous speed tests admitted = %d, want %d", succeeded, maxConcurrentNodeSpeedTests)
	}
	if got := len(server.speedSlots); got != maxConcurrentNodeSpeedTests {
		close(release)
		workers.Wait()
		t.Fatalf("occupied global slots = %d, want %d", got, maxConcurrentNodeSpeedTests)
	}
	server.speedMu.Lock()
	if got := len(server.speedRunning); got != maxConcurrentNodeSpeedTests {
		server.speedMu.Unlock()
		close(release)
		workers.Wait()
		t.Fatalf("running-node entries = %d, want %d", got, maxConcurrentNodeSpeedTests)
	}
	server.speedMu.Unlock()

	close(release)
	workers.Wait()
	if got := len(server.speedSlots); got != 0 {
		t.Fatalf("occupied global slots after end = %d, want 0", got)
	}
	server.speedMu.Lock()
	defer server.speedMu.Unlock()
	if got := len(server.speedRunning); got != 0 {
		t.Fatalf("running-node entries after end = %d, want 0", got)
	}
}

func TestStatusSpeedTestPromotesWorkingCredentialCandidate(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, speedTestMaxBytes)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(target.Close)
	restoreSpeedTestURL(t, target.URL)

	proxy, _ := newSpeedTestConnectProxyWithAuth(t, "working", "secret")
	proxy.Username, proxy.Password = "old", "wrong"
	proxy.CredentialAlternates = []ProxyCredential{{Username: "working", Password: "secret"}}
	proxy.Available = true
	pool := NewProxyPool()
	pool.Prime([]Proxy{proxy}, nil)
	server := NewStatusServer(pool, &ConfigStore{})
	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodPost, "/api/nodes/speedtest", bytes.NewBufferString(`{"key":"`+proxy.Key()+`"}`))
	server.handleNodeSpeedtest(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("credential speedtest handler = %d %s", recorder.Code, recorder.Body.String())
	}
	got, ok := pool.Find(proxy.Key())
	if !ok || got.Username != "working" || got.Password != "secret" || got.SpeedBytes != speedTestMaxBytes {
		t.Fatalf("pool after credential speedtest = %+v found=%v", got, ok)
	}
	if len(got.CredentialAlternates) != 1 || got.CredentialAlternates[0].Username != "old" {
		t.Fatalf("pool alternatives after credential speedtest = %#v", got.CredentialAlternates)
	}
}

func TestStatusSpeedTestCancellationReleasesSlotWithoutFallback(t *testing.T) {
	started := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(primary.Close)
	var fallbackCalls atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		http.Error(w, "must not run", http.StatusInternalServerError)
	}))
	t.Cleanup(fallback.Close)
	restoreSpeedTestURLs(t, primary.URL, fallback.URL)

	proxy := newSpeedTestConnectProxy(t)
	pool := NewProxyPool()
	pool.Prime([]Proxy{proxy}, nil)
	server := NewStatusServer(pool, &ConfigStore{})
	ctx, cancel := context.WithCancel(context.Background())
	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodPost, "/api/nodes/speedtest", bytes.NewBufferString(`{"key":"`+proxy.Key()+`"}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		server.handleNodeSpeedtest(recorder, request)
		close(done)
	}()

	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("speedtest handler did not return promptly after request cancellation")
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback calls after request cancellation = %d, want 0", fallbackCalls.Load())
	}
	if got := len(server.speedSlots); got != 0 {
		t.Fatalf("occupied speed slots after cancellation = %d, want 0", got)
	}
	server.speedMu.Lock()
	if got := len(server.speedRunning); got != 0 {
		server.speedMu.Unlock()
		t.Fatalf("running-node entries after cancellation = %d, want 0", got)
	}
	server.speedMu.Unlock()
	if err := server.beginSpeedTest(proxy.Key()); err != nil {
		t.Fatalf("speed slot was not reusable after cancellation: %v", err)
	}
	server.endSpeedTest(proxy.Key())
}

func TestStatusRepeatedDialStageCancellationLeavesNoActiveDialOrOccupiedSlot(t *testing.T) {
	upstream := newStalledTestUpstream(t, "http")
	var fallbackCalls atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		http.Error(w, "must not run", http.StatusInternalServerError)
	}))
	t.Cleanup(fallback.Close)
	restoreSpeedTestURLs(t, "http://speed-target.test/payload", fallback.URL)

	pool := NewProxyPool()
	pool.Prime([]Proxy{upstream.Proxy}, nil)
	server := NewStatusServer(pool, &ConfigStore{})
	baselineGoroutines := runtime.NumGoroutine()
	const attempts = 12
	for attempt := 0; attempt < attempts; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		recorder := httptest.NewRecorder()
		request := localTestRequest(http.MethodPost, "/api/nodes/speedtest", bytes.NewBufferString(`{"key":"`+upstream.Proxy.Key()+`"}`)).WithContext(ctx)
		done := make(chan struct{})
		go func() {
			server.handleNodeSpeedtest(recorder, request)
			close(done)
		}()
		select {
		case <-upstream.RequestSeen:
		case <-time.After(time.Second):
			cancel()
			t.Fatalf("attempt %d did not reach stalled CONNECT handshake", attempt)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			t.Fatalf("attempt %d handler did not stop within 300ms", attempt)
		}
		select {
		case <-upstream.Closed:
		case <-time.After(300 * time.Millisecond):
			t.Fatalf("attempt %d upstream connection remained open", attempt)
		}
		deadline := time.Now().Add(300 * time.Millisecond)
		for upstream.Active.Load() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if active := upstream.Active.Load(); active != 0 {
			t.Fatalf("attempt %d active dials = %d, want 0", attempt, active)
		}
		if got := len(server.speedSlots); got != 0 {
			t.Fatalf("attempt %d occupied speed slots = %d, want 0", attempt, got)
		}
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback calls after repeated dial cancellation = %d, want 0", fallbackCalls.Load())
	}
	if err := server.beginSpeedTest(upstream.Proxy.Key()); err != nil {
		t.Fatalf("speed slot not reusable after repeated cancellations: %v", err)
	}
	server.endSpeedTest(upstream.Proxy.Key())

	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	if got := runtime.NumGoroutine(); got > baselineGoroutines+3 {
		t.Fatalf("goroutines after repeated cancellation = %d, baseline %d", got, baselineGoroutines)
	}
}

func TestAPIStatusKeepsPoolExtractionCompatibility(t *testing.T) {
	pool := NewProxyPool()
	pool.Prime([]Proxy{
		{
			IP: "198.51.100.10", Port: "1080", Protocol: "socks5",
			Username: "pool-user", Password: "pool-pass", Available: true,
		},
		{IP: "198.51.100.11", Port: "8080", Protocol: "http", Available: true},
		{IP: "198.51.100.12", Port: "1080", Protocol: "socks5", Available: false},
	}, nil)
	store := &ConfigStore{cfg: PoolConfig{Rules: []Rule{{Type: RuleMatch, Group: GroupAny}}}}
	server := NewStatusServer(pool, store)

	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodGet, "/api/status", nil)
	server.handleAPIStatus(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/status status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	var body struct {
		ActiveProxy    string                       `json:"active_proxy"`
		AvailableTotal int                          `json:"available_total"`
		Proxies        []map[string]json.RawMessage `json:"proxies"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/status: %v", err)
	}
	if body.ActiveProxy != "socks5://pool-user:pool-pass@198.51.100.10:1080" {
		t.Fatalf("active_proxy = %q, want healthy active SOCKS URL", body.ActiveProxy)
	}
	if body.AvailableTotal != 2 || len(body.Proxies) != 2 {
		t.Fatalf("available_total=%d proxies=%d, want 2 healthy nodes", body.AvailableTotal, len(body.Proxies))
	}

	for i, proxy := range body.Proxies {
		if _, ok := proxy["proxy_url"]; !ok {
			t.Fatalf("proxies[%d] lacks proxy_url: %#v", i, proxy)
		}
		if _, ok := proxy["telegram_url"]; ok {
			t.Fatalf("proxies[%d] unexpectedly exposes telegram_url", i)
		}
	}
	if _, ok := body.Proxies[0]["socks_url"]; !ok {
		t.Fatalf("SOCKS proxy lacks socks_url: %#v", body.Proxies[0])
	}
	if _, ok := body.Proxies[1]["socks_url"]; ok {
		t.Fatalf("HTTP proxy unexpectedly has socks_url: %#v", body.Proxies[1])
	}
}
