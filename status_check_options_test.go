package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNodesPageAnonymityFilter(t *testing.T) {
	elite := testProxy("socks5", "192.0.2.11", "1080", true)
	elite.Anonymity = "elite"
	plain := testProxy("socks5", "192.0.2.12", "1080", true)
	plain.Anonymity = "anonymous"
	unknown := testProxy("socks5", "192.0.2.13", "1080", true)
	pool := NewProxyPool()
	pool.Prime([]Proxy{elite, plain, unknown}, nil)
	handler := NewStatusServer(pool, &ConfigStore{}).handler()

	collect := func(target string) (NodePageResponse, int) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, target, nil))
		var page NodePageResponse
		if recorder.Code == http.StatusOK {
			if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
		}
		return page, recorder.Code
	}

	page, code := collect("/api/nodes/page?anonymity=elite")
	if code != http.StatusOK || page.FilteredTotal != 1 || len(page.Nodes) != 1 || page.Nodes[0].Anonymity != "elite" {
		t.Fatalf("elite filter = %d %#v", code, page)
	}
	page, code = collect("/api/nodes/page?anonymity=unknown")
	if code != http.StatusOK || page.FilteredTotal != 1 || len(page.Nodes) != 1 || page.Nodes[0].Anonymity != "" {
		t.Fatalf("unknown filter = %d %#v", code, page)
	}
	page, code = collect("/api/nodes/page")
	if code != http.StatusOK || page.FilteredTotal != 3 {
		t.Fatalf("no filter = %d total %d", code, page.FilteredTotal)
	}
	if _, code = collect("/api/nodes/page?anonymity=bogus"); code != http.StatusBadRequest {
		t.Fatalf("invalid anonymity = %d, want 400", code)
	}
}

func TestCheckOptionsRoundTripAndValidation(t *testing.T) {
	dir := t.TempDir()
	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	server := NewStatusServer(pool, store)
	server.SetCheckDefaults(10*time.Second, 20, 3000, false)
	handler := server.handler()

	post := func(body string) *httptest.ResponseRecorder {
		request := localTestRequest(http.MethodPost, "/api/settings/check-options", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	recorder := post(`{"max_concurrent":50,"check_timeout_seconds":15,"max_candidates":500}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST numeric options = %d %s", recorder.Code, recorder.Body.String())
	}
	var saved struct {
		MaxConcurrent       int  `json:"max_concurrent"`
		CheckTimeoutSeconds int  `json:"check_timeout_seconds"`
		MaxCandidates       int  `json:"max_candidates"`
		RequireIPChange     bool `json:"require_ip_change"`
		PolicyChanged       bool `json:"policy_changed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.MaxConcurrent != 50 || saved.CheckTimeoutSeconds != 15 || saved.MaxCandidates != 500 || saved.PolicyChanged {
		t.Fatalf("saved options = %#v", saved)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/settings/check-options", nil))
	var got struct {
		MaxConcurrent       int `json:"max_concurrent"`
		CheckTimeoutSeconds int `json:"check_timeout_seconds"`
		MaxCandidates       int `json:"max_candidates"`
		Overrides           struct {
			MaxConcurrent       int         `json:"max_concurrent"`
			CheckTimeoutSeconds int         `json:"check_timeout_seconds"`
			MaxCandidates       int         `json:"max_candidates"`
			RequireIPChange     interface{} `json:"require_ip_change"`
		} `json:"overrides"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxConcurrent != 50 || got.CheckTimeoutSeconds != 15 || got.MaxCandidates != 500 {
		t.Fatalf("GET effective options = %#v", got)
	}
	if got.Overrides.MaxConcurrent != 50 || got.Overrides.CheckTimeoutSeconds != 15 || got.Overrides.MaxCandidates != 500 || got.Overrides.RequireIPChange != "default" {
		t.Fatalf("GET overrides = %#v", got.Overrides)
	}

	// Reloading the store must see the persisted overrides.
	reloaded, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.MaxConcurrent(20) != 50 || reloaded.CheckTimeout(10*time.Second) != 15*time.Second || reloaded.MaxCandidates(3000) != 500 {
		t.Fatalf("reloaded overrides = %d %s %d", reloaded.MaxConcurrent(0), reloaded.CheckTimeout(0), reloaded.MaxCandidates(0))
	}

	for _, body := range []string{
		`{"max_concurrent":999}`,
		`{"check_timeout_seconds":1}`,
		`{"max_candidates":10}`,
		`{"require_ip_change":"maybe"}`,
	} {
		if recorder := post(body); recorder.Code != http.StatusBadRequest {
			t.Fatalf("POST %s = %d, want 400", body, recorder.Code)
		}
	}

	// Zero clears the numeric overrides back to the CLI fallback.
	recorder = post(`{"max_concurrent":0,"check_timeout_seconds":0,"max_candidates":0}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST clear options = %d %s", recorder.Code, recorder.Body.String())
	}
	if store.MaxConcurrent(20) != 20 || store.CheckTimeout(10*time.Second) != 10*time.Second || store.MaxCandidates(3000) != 3000 {
		t.Fatal("zero values did not clear the overrides")
	}
}

func TestCheckOptionsScheduleSettingsRoundTripWithoutHealthInvalidation(t *testing.T) {
	dir := t.TempDir()
	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCheckOptions(50, 15*time.Second, 500, nil, false); err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	pool.SetHealthCriterion("https://example.test/health")
	pool.Prime([]Proxy{testProxy("socks5", "192.0.2.18", "1080", true)}, nil)
	coordinator := newRefreshCoordinator()
	server := NewStatusServerWithCoordinator(pool, store, coordinator)
	server.SetCheckDefaults(10*time.Second, 20, 3000, false)
	server.SetScheduleDefaults(20*time.Minute, 30*time.Minute)
	handler := server.handler()
	generation := pool.HealthGeneration()

	get := func() map[string]interface{} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/settings/check-options", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET schedules = %d %s", recorder.Code, recorder.Body.String())
		}
		var body map[string]interface{}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	post := func(body string) *httptest.ResponseRecorder {
		request := localTestRequest(http.MethodPost, "/api/settings/check-options", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	initial := get()
	if initial["source_refresh_interval_seconds"] != float64(1200) || initial["full_recheck_interval_seconds"] != float64(1800) {
		t.Fatalf("initial effective schedules = %#v", initial)
	}

	recorder := post(`{"source_refresh_interval_seconds":600,"full_recheck_interval_seconds":900}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST schedules = %d %s", recorder.Code, recorder.Body.String())
	}
	if pool.HealthGeneration() != generation || coordinator.drainFullRecheckSignalForTest() {
		t.Fatal("schedule-only update invalidated health or queued a full recheck")
	}
	if store.MaxConcurrent(20) != 50 || store.CheckTimeout(10*time.Second) != 15*time.Second || store.MaxCandidates(3000) != 500 {
		t.Fatal("schedule-only update changed existing health option overrides")
	}
	if store.SourceRefreshInterval(20*time.Minute) != 10*time.Minute || store.FullRecheckInterval(30*time.Minute) != 15*time.Minute {
		t.Fatalf("stored schedules = source %s full %s", store.SourceRefreshInterval(0), store.FullRecheckInterval(0))
	}

	got := get()
	overrides, ok := got["overrides"].(map[string]interface{})
	if !ok || got["source_refresh_interval_seconds"] != float64(600) || got["full_recheck_interval_seconds"] != float64(900) || overrides["source_refresh_interval_seconds"] != float64(600) || overrides["full_recheck_interval_seconds"] != float64(900) {
		t.Fatalf("GET saved schedules = %#v", got)
	}

	for _, body := range []string{
		`{"source_refresh_interval_seconds":59}`,
		`{"source_refresh_interval_seconds":604801}`,
		`{"full_recheck_interval_seconds":59}`,
		`{"full_recheck_interval_seconds":604801}`,
	} {
		if recorder := post(body); recorder.Code != http.StatusBadRequest {
			t.Fatalf("POST %s = %d, want 400", body, recorder.Code)
		}
	}
	if store.SourceRefreshInterval(0) != 10*time.Minute || store.FullRecheckInterval(0) != 15*time.Minute {
		t.Fatal("invalid schedule update partially changed persisted values")
	}

	recorder = post(`{"source_refresh_interval_seconds":0,"full_recheck_interval_seconds":0}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST schedule reset = %d %s", recorder.Code, recorder.Body.String())
	}
	if store.SourceRefreshInterval(20*time.Minute) != 20*time.Minute || store.FullRecheckInterval(30*time.Minute) != 30*time.Minute {
		t.Fatal("zero schedule values did not restore CLI defaults")
	}

	reloaded, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.SourceRefreshInterval(20*time.Minute) != 20*time.Minute || reloaded.FullRecheckInterval(30*time.Minute) != 30*time.Minute {
		t.Fatal("schedule reset did not persist across restart")
	}
}

func TestCheckOptionsRequireIPChangeFlipInvalidatesAndRechecks(t *testing.T) {
	resetRefreshOperationsForTest(t)
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	pool.Prime([]Proxy{testProxy("socks5", "198.51.100.20", "1080", true)}, nil)
	handler := NewStatusServer(pool, store).handler()

	originalRefresh := refreshBaselineForStatus
	baselineCalls := 0
	refreshBaselineForStatus = func(context.Context, time.Duration) (bool, bool) {
		baselineCalls++
		return true, true
	}
	t.Cleanup(func() { refreshBaselineForStatus = originalRefresh })

	request := localTestRequest(http.MethodPost, "/api/settings/check-options", strings.NewReader(`{"require_ip_change":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST policy flip = %d %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		RequireIPChange bool `json:"require_ip_change"`
		PolicyChanged   bool `json:"policy_changed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.RequireIPChange || !response.PolicyChanged {
		t.Fatalf("policy flip response = %#v", response)
	}
	if baselineCalls != 1 {
		t.Fatalf("baseline refreshes = %d, want 1", baselineCalls)
	}
	if nodes := pool.All(); len(nodes) != 1 || nodes[0].Available {
		t.Fatalf("pool after policy flip = %#v", nodes)
	}
	if !store.RequireIPChange(false) {
		t.Fatal("store did not persist the require-ip-change override")
	}
	if !defaultRefreshCoordinator.drainFullRecheckSignalForTest() {
		t.Fatal("policy flip did not queue a full recheck")
	}

	// Saving the same policy again must not invalidate or recheck.
	recorder = httptest.NewRecorder()
	request = localTestRequest(http.MethodPost, "/api/settings/check-options", strings.NewReader(`{"require_ip_change":true}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.PolicyChanged {
		t.Fatal("unchanged policy reported policy_changed")
	}
	if defaultRefreshCoordinator.drainFullRecheckSignalForTest() {
		t.Fatal("unchanged policy queued an unexpected recheck")
	}

	// Invalid numeric input must not partially clear the boolean override.
	recorder = httptest.NewRecorder()
	request = localTestRequest(http.MethodPost, "/api/settings/check-options", strings.NewReader(`{"max_concurrent":999,"require_ip_change":"default"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !store.RequireIPChange(false) || !pool.RequireIPChangePolicy() {
		t.Fatalf("failed atomic update = %d store=%v pool=%v", recorder.Code, store.RequireIPChange(false), pool.RequireIPChangePolicy())
	}

	// "default" clears the override back to the CLI fallback and synchronizes
	// the live pool policy, invalidating prior health outcomes again.
	recorder = httptest.NewRecorder()
	request = localTestRequest(http.MethodPost, "/api/settings/check-options", strings.NewReader(`{"require_ip_change":"default"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || response.RequireIPChange || !response.PolicyChanged || store.RequireIPChange(false) || pool.RequireIPChangePolicy() {
		t.Fatalf("default override clear = %d response=%#v store=%v pool=%v", recorder.Code, response, store.RequireIPChange(false), pool.RequireIPChangePolicy())
	}
	if !defaultRefreshCoordinator.drainFullRecheckSignalForTest() {
		t.Fatal("default policy reset did not queue a full recheck")
	}
}

func TestNodeRotateAdvancesEvenWhenPinned(t *testing.T) {
	first := testProxy("socks5", "192.0.2.21", "1080", true)
	second := testProxy("socks5", "192.0.2.22", "1080", true)
	pool := NewProxyPool()
	pool.Prime([]Proxy{first, second}, nil)
	handler := NewStatusServer(pool, &ConfigStore{}).handler()

	if !pool.ForceSticky(GroupAny, first.Key()) || !pool.IsPinned(GroupAny) {
		t.Fatal("failed to pin the first node")
	}
	// The periodic variant must refuse while pinned.
	if _, ok := pool.RotateSticky(GroupAny); ok {
		t.Fatal("RotateSticky advanced a pinned group")
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodPost, "/api/nodes/rotate", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("rotate = %d %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Node NodeView `json:"node"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Node.Key != second.Key() {
		t.Fatalf("rotated node = %q, want %q", response.Node.Key, second.Key())
	}
	if !pool.IsPinned(GroupAny) {
		t.Fatal("forced rotate dropped the manual pin")
	}
	selected, ok, _ := pool.Pick(GroupAny, nil)
	if !ok || selected.Key() != second.Key() {
		t.Fatalf("routing after forced rotate = %+v ok=%v", selected, ok)
	}

	// Empty pool reports a conflict instead of a fake success.
	emptyHandler := NewStatusServer(NewProxyPool(), &ConfigStore{}).handler()
	recorder = httptest.NewRecorder()
	emptyHandler.ServeHTTP(recorder, localTestRequest(http.MethodPost, "/api/nodes/rotate", nil))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "no_routable_node") {
		t.Fatalf("empty pool rotate = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestBaselineExitGetAndRefresh(t *testing.T) {
	resetRefreshOperationsForTest(t)
	baselineExitMu.Lock()
	previous := baselineExitIP
	baselineExitIP = "203.0.113.8"
	baselineExitMu.Unlock()
	t.Cleanup(func() {
		baselineExitMu.Lock()
		baselineExitIP = previous
		baselineExitMu.Unlock()
	})

	pool := NewProxyPool()
	pool.SetHealthCriterion("https://example.com/health")
	pool.SetRequireIPChangePolicy(true)
	pool.Prime([]Proxy{testProxy("socks5", "192.0.2.40", "1080", true)}, nil)
	handler := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "https://example.com/health"}}).handler()

	originalRefresh := refreshBaselineForStatus
	refreshBaselineForStatus = func(context.Context, time.Duration) (bool, bool) {
		baselineExitMu.Lock()
		baselineExitIP = "203.0.113.9"
		baselineExitMu.Unlock()
		return true, true
	}
	t.Cleanup(func() { refreshBaselineForStatus = originalRefresh })

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/settings/baseline-exit", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"baseline_ip":"203.0.113.8"`) {
		t.Fatalf("baseline GET = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodPost, "/api/settings/baseline-exit", nil))
	var response struct {
		Success       bool   `json:"success"`
		BaselineIP    string `json:"baseline_ip"`
		PolicyChanged bool   `json:"policy_changed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || !response.Success || response.BaselineIP != "203.0.113.9" || !response.PolicyChanged {
		t.Fatalf("baseline refresh = %d %#v", recorder.Code, response)
	}
	if nodes := pool.All(); len(nodes) != 1 || nodes[0].Available {
		t.Fatalf("pool after baseline change = %#v", nodes)
	}
	if !defaultRefreshCoordinator.drainFullRecheckSignalForTest() {
		t.Fatal("baseline policy change did not queue a full recheck")
	}
}

func TestBaselineExitRefreshFailureKeepsExistingPolicy(t *testing.T) {
	baselineExitMu.Lock()
	previous := baselineExitIP
	baselineExitIP = "203.0.113.10"
	baselineExitMu.Unlock()
	t.Cleanup(func() {
		baselineExitMu.Lock()
		baselineExitIP = previous
		baselineExitMu.Unlock()
	})

	pool := NewProxyPool()
	pool.SetRequireIPChangePolicy(true)
	pool.Prime([]Proxy{testProxy("socks5", "192.0.2.41", "1080", true)}, nil)
	handler := NewStatusServer(pool, &ConfigStore{}).handler()
	originalRefresh := refreshBaselineForStatus
	refreshBaselineForStatus = func(context.Context, time.Duration) (bool, bool) { return false, false }
	t.Cleanup(func() { refreshBaselineForStatus = originalRefresh })

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodPost, "/api/settings/baseline-exit", nil))
	var response struct {
		Success       bool   `json:"success"`
		BaselineIP    string `json:"baseline_ip"`
		PolicyChanged bool   `json:"policy_changed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || response.Success || response.PolicyChanged || response.BaselineIP != "203.0.113.10" {
		t.Fatalf("failed baseline refresh = %d %#v", recorder.Code, response)
	}
	if nodes := pool.All(); len(nodes) != 1 || !nodes[0].Available {
		t.Fatalf("failed baseline refresh invalidated pool = %#v", nodes)
	}
}

func TestNodeStatsEndpoint(t *testing.T) {
	px := testProxy("socks5", "192.0.2.31", "1080", true)
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	handler := NewStatusServer(pool, &ConfigStore{}).handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/nodes/stats?key="+px.Key(), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("node stats = %d %s", recorder.Code, recorder.Body.String())
	}
	var stats struct {
		Key       string `json:"key"`
		Available bool   `json:"available"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Key != px.Key() || !stats.Available {
		t.Fatalf("node stats = %#v", stats)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/nodes/stats", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing key = %d, want 400", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/nodes/stats?key=socks5://203.0.113.99:1080", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown key = %d, want 404", recorder.Code)
	}
}
