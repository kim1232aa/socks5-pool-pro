package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAPIRoutesMethodsErrorsAndSecurityHeaders(t *testing.T) {
	server := NewStatusServer(NewProxyPool(), &ConfigStore{})
	handler := server.handler()

	method := httptest.NewRecorder()
	handler.ServeHTTP(method, localTestRequest(http.MethodPost, "/api/status", strings.NewReader(`{}`)))
	if method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST /api/status = %d Allow=%q", method.Code, method.Header().Get("Allow"))
	}
	var methodError apiErrorResponse
	if err := json.Unmarshal(method.Body.Bytes(), &methodError); err != nil {
		t.Fatalf("decode method error: %v; body=%s", err, method.Body.String())
	}
	if methodError.Error == "" || methodError.Code != "method_not_allowed" || methodError.RequestID == "" || methodError.RequestID != method.Header().Get("X-Request-ID") {
		t.Fatalf("structured method error = %#v headers=%v", methodError, method.Header())
	}
	for name, want := range map[string]string{
		"Cache-Control":          "no-store, private",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := method.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	for _, path := range []string{"/api", "/api/does-not-exist"} {
		missing := httptest.NewRecorder()
		handler.ServeHTTP(missing, localTestRequest(http.MethodGet, path, nil))
		if missing.Code != http.StatusNotFound || !strings.HasPrefix(missing.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("unknown API route %s = %d type=%q body=%s", path, missing.Code, missing.Header().Get("Content-Type"), missing.Body.String())
		}
		var missingError apiErrorResponse
		if err := json.Unmarshal(missing.Body.Bytes(), &missingError); err != nil || missingError.Code != "route_not_found" {
			t.Fatalf("unknown API error %s = %#v err=%v", path, missingError, err)
		}
	}

	exportMethod := httptest.NewRecorder()
	handler.ServeHTTP(exportMethod, localTestRequest(http.MethodPost, "/api/nodes/export", nil))
	if exportMethod.Code != http.StatusMethodNotAllowed || exportMethod.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST export = %d Allow=%q", exportMethod.Code, exportMethod.Header().Get("Allow"))
	}

	legacyNodes := httptest.NewRecorder()
	handler.ServeHTTP(legacyNodes, localTestRequest(http.MethodGet, "/api/nodes", nil))
	if legacyNodes.Code != http.StatusOK || legacyNodes.Header().Get("Deprecation") != "true" || legacyNodes.Header().Get("Sunset") == "" || !strings.Contains(legacyNodes.Header().Get("Link"), "/api/nodes/page") {
		t.Fatalf("legacy /api/nodes migration headers = %d %v", legacyNodes.Code, legacyNodes.Header())
	}
}

func TestGzipNegotiationHonorsExactCodingAndQuality(t *testing.T) {
	handler := gzipIfAccepted(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	}))
	for _, encoding := range []string{"gzip;q=0", "xgzip", "br, gzip; q=0"} {
		recorder := httptest.NewRecorder()
		request := localTestRequest(http.MethodGet, "/api/status", nil)
		request.Header.Set("Accept-Encoding", encoding)
		handler.ServeHTTP(recorder, request)
		if got := recorder.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Accept-Encoding %q produced %q", encoding, got)
		}
		if !strings.Contains(recorder.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("Accept-Encoding %q missing Vary header", encoding)
		}
	}

	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodGet, "/api/status", nil)
	request.Header.Set("Accept-Encoding", "br, gzip;q=0.5")
	handler.ServeHTTP(recorder, request)
	if got := recorder.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("positive gzip quality produced %q", got)
	}
}

func TestManagementJSONRejectsUnknownFields(t *testing.T) {
	handler := NewStatusServer(NewProxyPool(), &ConfigStore{}).handler()
	request := localTestRequest(http.MethodPost, "/api/settings/check-url", strings.NewReader(`{
		"url":"https://example.com/health",
		"unexpected":true
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf("unknown management JSON field = %d %s", recorder.Code, recorder.Body.String())
	}

	groupRequest := localTestRequest(http.MethodPost, "/api/groups", strings.NewReader(`{
		"name":"mistyped-filter",
		"strategy":"sticky",
		"countriez":["JP"]
	}`))
	groupRequest.Header.Set("Content-Type", "application/json")
	groupRecorder := httptest.NewRecorder()
	handler.ServeHTTP(groupRecorder, groupRequest)
	if groupRecorder.Code != http.StatusBadRequest || !strings.Contains(groupRecorder.Body.String(), "unknown group field") {
		t.Fatalf("unknown group field = %d %s", groupRecorder.Code, groupRecorder.Body.String())
	}
}

func TestConfigPersistenceFailureIsAnInternalAPIError(t *testing.T) {
	store := &ConfigStore{
		path: "/proc/socks5-pool-test/pool_config.json",
		cfg:  defaultPoolConfig(),
	}
	handler := NewStatusServer(NewProxyPool(), store).handler()
	request := localTestRequest(http.MethodPost, "/api/sources", strings.NewReader(`{
		"name":"persistence failure test",
		"url":"https://example.com/persistence-test.txt",
		"format":"plain-list",
		"protocol":"socks5"
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), `"code":"config_persistence_failed"`) {
		t.Fatalf("config persistence API failure = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestReadinessRequiresPublishedCandidateSnapshot(t *testing.T) {
	pool := NewProxyPool()
	handler := NewStatusServer(pool, &ConfigStore{}).handler()

	before := httptest.NewRecorder()
	handler.ServeHTTP(before, localTestRequest(http.MethodGet, "/readyz", nil))
	if before.Code != http.StatusServiceUnavailable || before.Header().Get("Retry-After") == "" {
		t.Fatalf("readiness before inventory = %d headers=%v body=%s", before.Code, before.Header(), before.Body.String())
	}

	pool.candidates.begin([]Proxy{{IP: "192.0.2.1", Port: "443", Protocol: "proxyip"}}, nil, nil, 0)
	after := httptest.NewRecorder()
	handler.ServeHTTP(after, localTestRequest(http.MethodGet, "/readyz", nil))
	if after.Code != http.StatusOK || after.Body.String() != "ready\n" {
		t.Fatalf("readiness after inventory = %d body=%q", after.Code, after.Body.String())
	}
}

func TestNodePaginationSnapshotAndStrictCountryContract(t *testing.T) {
	server, _ := pagedNodeTestServer(t)
	handler := server.handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/nodes/page?page_size=2", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("node page = %d: %s", recorder.Code, recorder.Body.String())
	}
	var page NodePageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.SnapshotID == "" || page.PageCount != 3 || !page.HasNext || recorder.Header().Get("X-Snapshot-ID") != page.SnapshotID {
		t.Fatalf("node page metadata = %#v headers=%v", page, recorder.Header())
	}

	stale := httptest.NewRecorder()
	handler.ServeHTTP(stale, localTestRequest(http.MethodGet, "/api/nodes/page?snapshot_id=pool-stale", nil))
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"snapshot_changed"`) {
		t.Fatalf("stale node snapshot = %d %s", stale.Code, stale.Body.String())
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, localTestRequest(http.MethodGet, "/api/nodes/page?country=Japan", nil))
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"invalid_country"`) {
		t.Fatalf("invalid node country = %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestCandidatePaginationSnapshotMetadata(t *testing.T) {
	pool := NewProxyPool()
	refresh := pool.candidates.begin([]Proxy{
		{IP: "192.0.2.1", Port: "80", Protocol: "http"},
		{IP: "192.0.2.2", Port: "80", Protocol: "http"},
	}, nil, nil, 0)
	pool.candidates.complete(refresh, nil, nil, nil)
	handler := NewStatusServer(pool, &ConfigStore{}).handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/candidates/page?page_size=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("candidate page = %d: %s", recorder.Code, recorder.Body.String())
	}
	var page CandidatePageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.SnapshotID == "" || page.PageCount != 2 || !page.HasNext || recorder.Header().Get("X-Snapshot-ID") != page.SnapshotID {
		t.Fatalf("candidate page metadata = %#v headers=%v", page, recorder.Header())
	}

	stale := httptest.NewRecorder()
	handler.ServeHTTP(stale, localTestRequest(http.MethodGet, "/api/candidates/page?snapshot_id=candidate-stale", nil))
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"snapshot_changed"`) {
		t.Fatalf("stale candidate snapshot = %d %s", stale.Code, stale.Body.String())
	}
}

func TestCompactStatusUsesRetainedCandidateSnapshotMetadata(t *testing.T) {
	pool := NewProxyPool()
	labels := map[string]string{"source-a": "Source A", "source-b": "Source B"}
	oldA := candidateFromSource("192.0.2.31", "source-a", "Source A")
	oldB := candidateFromSource("192.0.2.32", "source-b", "Source B")
	first := pool.candidates.begin([]Proxy{oldA, oldB}, labels, nil, 0)
	pool.candidates.complete(first, nil, nil, nil)
	partial := pool.candidates.begin([]Proxy{oldB}, labels, map[string]bool{"source-a": true}, 1)
	pool.candidates.complete(partial, nil, nil, nil)

	recorder := httptest.NewRecorder()
	NewStatusServer(pool, &ConfigStore{}).handler().ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/status?compact=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("compact status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body compactStatusSummary
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.CandidateTotal != 2 || body.CandidatePhase != "partial" || body.CandidateSourceErrors != 1 || body.CandidateUpdatedAt == "" {
		t.Fatalf("compact candidate metadata = %#v", body)
	}
}

func TestStatusAddsRFC3339UTCTimestampsWithoutRemovingLegacyFields(t *testing.T) {
	scrapeMu.Lock()
	previousLast, previousNext := lastScrapeTime, nextScrapeTime
	lastScrapeTime = time.Date(2026, time.July, 12, 5, 1, 2, 0, time.UTC)
	nextScrapeTime = time.Date(2026, time.July, 12, 5, 21, 2, 0, time.UTC)
	scrapeMu.Unlock()
	t.Cleanup(func() {
		scrapeMu.Lock()
		lastScrapeTime, nextScrapeTime = previousLast, previousNext
		scrapeMu.Unlock()
	})

	summary := NewStatusServer(NewProxyPool(), &ConfigStore{}).buildSummary()
	if summary.LastScrape == "" || summary.NextScrape == "" {
		t.Fatalf("legacy scrape fields were removed: %#v", summary)
	}
	if summary.LastScrapeAt != "2026-07-12T05:01:02Z" || summary.NextScrapeAt != "2026-07-12T05:21:02Z" {
		t.Fatalf("RFC3339 scrape fields = %q %q", summary.LastScrapeAt, summary.NextScrapeAt)
	}
}

func TestV1HealthyProxyPageAndPickContract(t *testing.T) {
	pool := NewProxyPool()
	pool.Prime([]Proxy{
		{IP: "192.0.2.10", Port: "1080", Protocol: "socks5", Username: "user", Password: "pass", Country: "JP", Available: true, LatencyMs: 20},
		{IP: "192.0.2.11", Port: "8080", Protocol: "http", Country: "US", Available: true, LatencyMs: 50},
		{IP: "192.0.2.12", Port: "1080", Protocol: "socks5", Country: "JP", Available: false},
		{IP: "192.0.2.13", Port: "1080", Protocol: "socks5", Country: "JP", Available: true, HealthInvalidated: true},
	}, nil)
	handler := NewStatusServer(pool, &ConfigStore{}).handler()

	pageRecorder := httptest.NewRecorder()
	handler.ServeHTTP(pageRecorder, localTestRequest(http.MethodGet, "/api/v1/proxies?page_size=1", nil))
	if pageRecorder.Code != http.StatusOK {
		t.Fatalf("v1 proxy page = %d: %s", pageRecorder.Code, pageRecorder.Body.String())
	}
	var page V1ProxyPage
	if err := json.Unmarshal(pageRecorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.APIVersion != "v1" || page.AvailableTotal != 2 || page.FilteredTotal != 2 || page.PageCount != 2 || !page.HasNext || len(page.Proxies) != 1 {
		t.Fatalf("v1 proxy page = %#v", page)
	}
	if page.Proxies[0].Key != "http://192.0.2.11:8080" {
		t.Fatalf("v1 page is not key-stable: %#v", page.Proxies)
	}
	if strings.Contains(pageRecorder.Body.String(), "telegram_url") {
		t.Fatalf("v1 proxy page exposed telegram_url: %s", pageRecorder.Body.String())
	}
	if strings.Contains(pageRecorder.Body.String(), `"score"`) {
		t.Fatalf("v1 page exposed volatile score: %s", pageRecorder.Body.String())
	}

	pool.RecordResult("socks5://192.0.2.10:1080", true, 1)
	stableRecorder := httptest.NewRecorder()
	handler.ServeHTTP(stableRecorder, localTestRequest(http.MethodGet, "/api/v1/proxies?page_size=1", nil))
	var stablePage V1ProxyPage
	if err := json.Unmarshal(stableRecorder.Body.Bytes(), &stablePage); err != nil {
		t.Fatal(err)
	}
	if stablePage.SnapshotID != page.SnapshotID || len(stablePage.Proxies) != 1 || stablePage.Proxies[0].Key != page.Proxies[0].Key {
		t.Fatalf("hidden score mutation destabilized key-sorted v1 pagination: before=%#v after=%#v", page, stablePage)
	}
	if !pool.UpdateSpeed("socks5://192.0.2.10:1080", 1234, 3_000_000, 1000) {
		t.Fatal("failed to update v1 speed fixture")
	}
	changedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(changedRecorder, localTestRequest(http.MethodGet, "/api/v1/proxies?page_size=1", nil))
	var changedPage V1ProxyPage
	if err := json.Unmarshal(changedRecorder.Body.Bytes(), &changedPage); err != nil {
		t.Fatal(err)
	}
	if changedPage.SnapshotID == stablePage.SnapshotID {
		t.Fatalf("output-field speed mutation reused v1 snapshot token %q", stablePage.SnapshotID)
	}

	pickRecorder := httptest.NewRecorder()
	handler.ServeHTTP(pickRecorder, localTestRequest(http.MethodGet, "/api/v1/proxies/pick?protocol=socks5&country=JP", nil))
	if pickRecorder.Code != http.StatusOK {
		t.Fatalf("v1 proxy pick = %d: %s", pickRecorder.Code, pickRecorder.Body.String())
	}
	var pick V1ProxyPickResponse
	if err := json.Unmarshal(pickRecorder.Body.Bytes(), &pick); err != nil {
		t.Fatal(err)
	}
	if pick.Proxy.Protocol != "socks5" || pick.Proxy.Country != "JP" || pick.Proxy.ProxyURL == "" || pick.Proxy.SocksURL != pick.Proxy.ProxyURL {
		t.Fatalf("v1 proxy pick = %#v", pick)
	}
	if strings.Contains(pickRecorder.Body.String(), "telegram_url") {
		t.Fatalf("v1 proxy pick exposed telegram_url: %s", pickRecorder.Body.String())
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, localTestRequest(http.MethodGet, "/api/v1/proxies?protocol=proxyip", nil))
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"invalid_protocol"`) {
		t.Fatalf("invalid v1 protocol = %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestV1PickSnapshotTracksHiddenScore(t *testing.T) {
	pool := NewProxyPool()
	first := Proxy{IP: "192.0.2.30", Port: "1080", Protocol: "socks5", Country: "JP", Available: true, LatencyMs: 20}
	second := Proxy{IP: "192.0.2.31", Port: "1080", Protocol: "socks5", Country: "JP", Available: true, LatencyMs: 20}
	pool.Prime([]Proxy{first, second}, nil)
	handler := NewStatusServer(pool, &ConfigStore{}).handler()

	initialRecorder := httptest.NewRecorder()
	handler.ServeHTTP(initialRecorder, localTestRequest(http.MethodGet, "/api/v1/proxies/pick?country=JP", nil))
	if initialRecorder.Code != http.StatusOK {
		t.Fatalf("initial pick = %d %s", initialRecorder.Code, initialRecorder.Body.String())
	}
	var initial V1ProxyPickResponse
	if err := json.Unmarshal(initialRecorder.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if initial.Proxy.Key != first.Key() {
		t.Fatalf("equal-score initial pick = %q, want key-stable %q", initial.Proxy.Key, first.Key())
	}

	pool.RecordResult(second.Key(), true, 1)
	staleRecorder := httptest.NewRecorder()
	handler.ServeHTTP(staleRecorder, localTestRequest(http.MethodGet, "/api/v1/proxies/pick?country=JP&snapshot_id="+initial.SnapshotID, nil))
	if staleRecorder.Code != http.StatusConflict || !strings.Contains(staleRecorder.Body.String(), `"code":"snapshot_changed"`) {
		t.Fatalf("score-stale pick = %d %s", staleRecorder.Code, staleRecorder.Body.String())
	}

	currentRecorder := httptest.NewRecorder()
	handler.ServeHTTP(currentRecorder, localTestRequest(http.MethodGet, "/api/v1/proxies/pick?country=JP", nil))
	var current V1ProxyPickResponse
	if err := json.Unmarshal(currentRecorder.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if currentRecorder.Code != http.StatusOK || current.SnapshotID == initial.SnapshotID || current.Proxy.Key != second.Key() {
		t.Fatalf("score-aware current pick = %d %#v, initial=%#v", currentRecorder.Code, current, initial)
	}
}

func TestRefreshEndpointReturnsTrackableAcceptedOperation(t *testing.T) {
	resetRefreshOperationsForTest(t)
	handler := NewStatusServer(NewProxyPool(), &ConfigStore{}).handler()

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, localTestRequest(http.MethodPost, "/api/refresh", nil))
	if first.Code != http.StatusAccepted || first.Header().Get("Location") != "/api/refresh/status" {
		t.Fatalf("first refresh = %d headers=%v body=%s", first.Code, first.Header(), first.Body.String())
	}
	var firstBody struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Accepted  bool   `json:"accepted"`
		Coalesced bool   `json:"coalesced"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	if firstBody.ID == "" || firstBody.Status != "queued" || !firstBody.Accepted || firstBody.Coalesced {
		t.Fatalf("first refresh body = %#v", firstBody)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, localTestRequest(http.MethodPost, "/api/refresh", nil))
	var secondBody struct {
		ID        string `json:"id"`
		Accepted  bool   `json:"accepted"`
		Coalesced bool   `json:"coalesced"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatal(err)
	}
	if second.Code != http.StatusAccepted || secondBody.ID != firstBody.ID || secondBody.Accepted || !secondBody.Coalesced {
		t.Fatalf("coalesced refresh = %d %#v", second.Code, secondBody)
	}

	status := httptest.NewRecorder()
	handler.ServeHTTP(status, localTestRequest(http.MethodGet, "/api/refresh/status", nil))
	var operationStatus RefreshOperationStatus
	if err := json.Unmarshal(status.Body.Bytes(), &operationStatus); err != nil {
		t.Fatal(err)
	}
	if status.Code != http.StatusOK || operationStatus.State != "queued" || operationStatus.Pending == nil || operationStatus.Pending.ID != firstBody.ID {
		t.Fatalf("refresh status = %d %#v", status.Code, operationStatus)
	}
	runningID := beginRefreshOperation()
	if running := getRefreshOperationStatus(); running.State != "running" || running.Active == nil || running.Active.ID != runningID {
		t.Fatalf("running refresh status = %#v", running)
	}
	finishRefreshOperation(runningID, refreshRunResult{Status: "partial", SourceErrors: 2})
	if completed := getRefreshOperationStatus(); completed.State != "idle" || completed.Last == nil || completed.Last.Status != "partial" || completed.Last.SourceErrors != 2 || completed.Last.CompletedAt == "" {
		t.Fatalf("completed refresh status = %#v", completed)
	}
}

func TestStateChangingAPIsRejectCrossSiteBrowsersButAllowCompatibleClients(t *testing.T) {
	resetRefreshOperationsForTest(t)
	handler := NewStatusServer(NewProxyPool(), &ConfigStore{}).handler()

	crossSite := localTestRequest(http.MethodPost, "/api/refresh", nil)
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	crossRecorder := httptest.NewRecorder()
	handler.ServeHTTP(crossRecorder, crossSite)
	if crossRecorder.Code != http.StatusForbidden || !strings.Contains(crossRecorder.Body.String(), `"code":"cross_site_request"`) {
		t.Fatalf("cross-site write = %d %s", crossRecorder.Code, crossRecorder.Body.String())
	}

	wrongOrigin := localTestRequest(http.MethodPost, "/api/refresh", nil)
	wrongOrigin.Header.Set("Origin", "https://attacker.example")
	wrongRecorder := httptest.NewRecorder()
	handler.ServeHTTP(wrongRecorder, wrongOrigin)
	if wrongRecorder.Code != http.StatusForbidden || !strings.Contains(wrongRecorder.Body.String(), `"code":"origin_mismatch"`) {
		t.Fatalf("wrong-origin write = %d %s", wrongRecorder.Code, wrongRecorder.Body.String())
	}

	sameOrigin := localTestRequest(http.MethodPost, "/api/refresh", nil)
	sameOrigin.Header.Set("Origin", "http://localhost")
	sameRecorder := httptest.NewRecorder()
	handler.ServeHTTP(sameRecorder, sameOrigin)
	if sameRecorder.Code != http.StatusAccepted {
		t.Fatalf("same-origin write = %d %s", sameRecorder.Code, sameRecorder.Body.String())
	}
}

func TestUnauthenticatedManagementRequiresLoopbackHostAndRemote(t *testing.T) {
	handler := NewStatusServer(NewProxyPool(), &ConfigStore{}).handler()
	for _, host := range []string{"attacker.test", "172.17.0.2:8080"} {
		request := localTestRequest(http.MethodGet, "/api/status", nil)
		request.Host = host
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"code":"untrusted_client"`) {
			t.Errorf("unauthenticated Host %q = %d %s", host, recorder.Code, recorder.Body.String())
		}
	}
	for _, host := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"} {
		request := localTestRequest(http.MethodGet, "/api/status?compact=1", nil)
		request.Host = host
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Errorf("loopback Host %q = %d %s", host, recorder.Code, recorder.Body.String())
		}
	}

	for _, remoteAddr := range []string{"203.0.113.9:54321", "[2001:db8::9]:54321", "not-an-address"} {
		request := localTestRequest(http.MethodGet, "/api/status?compact=1", nil)
		request.RemoteAddr = remoteAddr
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"code":"untrusted_client"`) {
			t.Errorf("unauthenticated RemoteAddr %q = %d %s", remoteAddr, recorder.Code, recorder.Body.String())
		}
	}

	trustedServer := NewStatusServer(NewProxyPool(), &ConfigStore{})
	if err := trustedServer.SetTrustedManagementProxies([]string{"172.18.0.1"}); err != nil {
		t.Fatal(err)
	}
	trustedRequest := localTestRequest(http.MethodGet, "/api/status?compact=1", nil)
	trustedRequest.RemoteAddr = "172.18.0.1:43210"
	trustedRecorder := httptest.NewRecorder()
	trustedServer.handler().ServeHTTP(trustedRecorder, trustedRequest)
	if trustedRecorder.Code != http.StatusOK {
		t.Fatalf("exact trusted management proxy = %d %s", trustedRecorder.Code, trustedRecorder.Body.String())
	}
	untrustedRequest := localTestRequest(http.MethodGet, "/api/status?compact=1", nil)
	untrustedRequest.RemoteAddr = "172.18.0.2:43210"
	untrustedRecorder := httptest.NewRecorder()
	trustedServer.handler().ServeHTTP(untrustedRecorder, untrustedRequest)
	if untrustedRecorder.Code != http.StatusForbidden {
		t.Fatalf("neighbor of trusted management proxy = %d %s", untrustedRecorder.Code, untrustedRecorder.Body.String())
	}
	for _, invalid := range []string{"172.18.0.0/16", "proxy.internal", "172.18.0.1:8080"} {
		if err := trustedServer.SetTrustedManagementProxies([]string{invalid}); err == nil {
			t.Errorf("trusted management proxy %q unexpectedly accepted", invalid)
		}
	}

	authenticated := NewStatusServerWithAdminCredentials(NewProxyPool(), &ConfigStore{}, "admin", "secret").handler()
	request := localTestRequest(http.MethodGet, "/api/status?compact=1", nil)
	request.Host = "admin.example"
	request.RemoteAddr = "203.0.113.20:54321"
	request.SetBasicAuth("admin", "secret")
	recorder := httptest.NewRecorder()
	authenticated.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated non-loopback Host = %d %s", recorder.Code, recorder.Body.String())
	}
}

func resetRefreshOperationsForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		refreshOpMu.Lock()
		refreshOpSeq = 0
		refreshActive = nil
		refreshPending = nil
		refreshLast = nil
		refreshOpMu.Unlock()
		healthRecheckOpMu.Lock()
		healthRecheckOpSeq = 0
		healthRecheckActive = nil
		healthRecheckPending = nil
		healthRecheckLast = nil
		healthRecheckOpMu.Unlock()
		for {
			drained := false
			select {
			case <-refreshChan:
				drained = true
			default:
			}
			select {
			case <-recheckChan:
				drained = true
			default:
			}
			select {
			case <-fullRecheckChan:
				drained = true
			default:
			}
			if !drained {
				return
			}
		}
	}
	reset()
	t.Cleanup(reset)
}

func TestCheckURLChangeInvalidatesHealthAndQueuesOnlyFullRecheck(t *testing.T) {
	resetRefreshOperationsForTest(t)
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	pool.Prime([]Proxy{{
		IP: "198.51.100.10", Port: "1080", Protocol: "socks5", Available: true,
	}}, nil)
	handler := NewStatusServer(pool, store).handler()

	request := localTestRequest(http.MethodPost, "/api/settings/check-url", strings.NewReader(`{"url":"https://example.com/health"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST check URL = %d %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Status           string `json:"status"`
		URL              string `json:"url"`
		InvalidatedTotal int    `json:"invalidated_total"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || response.URL != "https://example.com/health" || response.InvalidatedTotal != 1 {
		t.Fatalf("check URL response = %#v", response)
	}
	if nodes := pool.All(); len(nodes) != 1 || nodes[0].Available {
		t.Fatalf("pool after check URL change = %#v", nodes)
	}
	select {
	case <-fullRecheckChan:
	default:
		t.Fatal("check URL change did not queue a full recheck")
	}
	select {
	case <-refreshChan:
		t.Fatal("check URL change unexpectedly queued a source refresh")
	default:
	}
	select {
	case <-recheckChan:
		t.Fatal("check URL change unexpectedly queued the legacy incremental recheck")
	default:
	}
}

func TestSavingIdenticalCheckURLIsNoOp(t *testing.T) {
	resetRefreshOperationsForTest(t)
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	px := testProxy("http", "198.51.100.11", "8080", true)
	pool.Prime([]Proxy{px}, nil)
	handler := NewStatusServer(pool, store).handler()

	request := localTestRequest(http.MethodPost, "/api/settings/check-url", strings.NewReader(fmt.Sprintf(`{"url":%q}`, store.CheckURL())))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"changed":false`) {
		t.Fatalf("identical check URL = %d %s", recorder.Code, recorder.Body.String())
	}
	if got, _ := pool.Find(px.Key()); !got.Available || pool.HealthGeneration() != 0 {
		t.Fatalf("identical URL invalidated node: %+v generation=%d", got, pool.HealthGeneration())
	}
	select {
	case <-fullRecheckChan:
		t.Fatal("identical URL queued full recheck")
	default:
	}
}

func TestClearUnavailableBlockedWhileCriterionRecheckPending(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	px := testProxy("http", "198.51.100.12", "8080", true)
	pool.Prime([]Proxy{px}, nil)
	generation := pool.HealthGeneration()
	pool.InvalidateHealth("https://example.com/new-health")
	handler := NewStatusServer(pool, store).handler()

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, localTestRequest(http.MethodPost, "/api/nodes/clear-unavailable", strings.NewReader(`{}`)))
	if blocked.Code != http.StatusConflict || !strings.Contains(blocked.Body.String(), `"code":"health_recheck_in_progress"`) {
		t.Fatalf("clear during recheck = %d %s", blocked.Code, blocked.Body.String())
	}
	if pool.Size() != 1 {
		t.Fatal("blocked clear removed retained node")
	}
	if pool.CompleteHealthRecheck(generation) {
		t.Fatal("stale generation cleared recheck guard")
	}
	if !pool.CompleteHealthRecheck(pool.HealthGeneration()) {
		t.Fatal("current generation did not clear recheck guard")
	}
	cleared := httptest.NewRecorder()
	handler.ServeHTTP(cleared, localTestRequest(http.MethodPost, "/api/nodes/clear-unavailable", strings.NewReader(`{}`)))
	if cleared.Code != http.StatusOK || pool.Size() != 0 {
		t.Fatalf("clear after recheck = %d %s size=%d", cleared.Code, cleared.Body.String(), pool.Size())
	}
}

func TestHealthRecheckOperationReportsProgress(t *testing.T) {
	resetRefreshOperationsForTest(t)
	pool := NewProxyPool()
	pool.SetHealthCriterion(defaultCheckURL)
	operation, accepted := TriggerFullRecheck(pool)
	if !accepted || operation.Status != "queued" {
		t.Fatalf("queued operation = %#v accepted=%v", operation, accepted)
	}
	running := beginHealthRecheckOperation(pool, 3)
	recordHealthRecheckOutcome(running.ID, true, false)
	recordHealthRecheckOutcome(running.ID, true, true)
	recordHealthRecheckOutcome(running.ID, false, false)
	state := getHealthRecheckOperationStatus()
	if state.State != "running" || state.Active == nil || state.Active.Completed != 3 || state.Active.Reachable != 2 || state.Active.Failed != 1 || state.Active.PolicyFiltered != 1 {
		t.Fatalf("running recheck status = %#v", state)
	}
	finishHealthRecheckOperation(running.ID, true)
	state = getHealthRecheckOperationStatus()
	if state.State != "idle" || state.Last == nil || state.Last.Status != "complete" || state.Last.ID != operation.ID {
		t.Fatalf("completed recheck status = %#v", state)
	}
}

func TestSourceToggleImmediatelyRetiresItsOnlyRoutingNodeAndQueuesRefresh(t *testing.T) {
	resetRefreshOperationsForTest(t)
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var source Source
	for _, candidate := range store.Sources() {
		if candidate.Enabled {
			source = candidate
			break
		}
	}
	if source.ID == "" {
		t.Fatal("default config has no enabled source")
	}
	pool := NewProxyPool()
	pool.Prime([]Proxy{{
		IP: "198.51.100.20", Port: "8080", Protocol: "http", Available: true,
		SourceIDs: []string{source.ID}, SourceNames: []string{source.Name},
	}}, nil)
	handler := NewStatusServer(pool, store).handler()

	request := localTestRequest(http.MethodPost, "/api/sources/toggle", strings.NewReader(fmt.Sprintf(`{"id":%q,"enabled":false}`, source.ID)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"retired_total":1`) {
		t.Fatalf("toggle source = %d %s", recorder.Code, recorder.Body.String())
	}
	if nodes := pool.All(); len(nodes) != 1 || nodes[0].Available || !nodes[0].SourceRetired {
		t.Fatalf("pool after source toggle = %#v", nodes)
	}
	// A refresh that captured the source before the toggle must not revive it
	// when its stale successful health result arrives afterwards.
	stale := pool.All()[0]
	stale.Available = true
	stale.SourceRetired = false
	if !applyRefreshHealthResults(pool, store, []Proxy{stale}, nil, nil, pool.HealthGeneration()) {
		t.Fatal("stale refresh result was rejected for an unrelated generation")
	}
	if nodes := pool.All(); len(nodes) != 1 || nodes[0].Available || !nodes[0].SourceRetired {
		t.Fatalf("stale refresh revived disabled source node: %#v", nodes)
	}
	if got, ok, direct := pool.Pick(GroupAny, nil); ok || direct {
		t.Fatalf("disabled source remained routable: %+v, ok=%v direct=%v", got, ok, direct)
	}
	select {
	case <-refreshChan:
	default:
		t.Fatal("source toggle did not queue a refresh")
	}
}

func TestNodeSwitchRejectsUnavailableWithoutLeavingFalsePin(t *testing.T) {
	pool := NewProxyPool()
	unavailable := Proxy{IP: "198.51.100.21", Port: "1080", Protocol: "socks5", Available: false}
	healthy := Proxy{IP: "198.51.100.22", Port: "1080", Protocol: "socks5", Available: true}
	pool.Prime([]Proxy{unavailable, healthy}, nil)
	handler := NewStatusServer(pool, &ConfigStore{}).handler()

	request := localTestRequest(http.MethodPost, "/api/nodes/switch", strings.NewReader(fmt.Sprintf(`{"key":%q}`, unavailable.Key())))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("switch unavailable status = %d body=%s, want 409", recorder.Code, recorder.Body.String())
	}
	var response apiErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode unavailable switch response: %v", err)
	}
	if response.Code != "node_unavailable" || response.Error == "" {
		t.Fatalf("unavailable switch error = %#v", response)
	}
	if pool.IsPinned(GroupAny) {
		t.Fatal("409 unavailable switch left ANY falsely pinned")
	}

	request = localTestRequest(http.MethodPost, "/api/nodes/switch", strings.NewReader(fmt.Sprintf(`{"key":%q}`, healthy.Key())))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"pinned":"true"`) || !pool.IsPinned(GroupAny) {
		t.Fatalf("switch healthy = %d body=%s pinned=%v", recorder.Code, recorder.Body.String(), pool.IsPinned(GroupAny))
	}
}

func TestSourceDeleteImmediatelyRetiresItsOnlyRoutingNodeAndQueuesRefresh(t *testing.T) {
	resetRefreshOperationsForTest(t)
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(Source{
		Name: "test delete source", URL: "https://example.com/test-proxies.txt",
		Format: FormatPlainList, Protocol: "socks5",
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	pool.Prime([]Proxy{{
		IP: "198.51.100.30", Port: "1080", Protocol: "socks5", Available: true,
		SourceIDs: []string{source.ID}, SourceNames: []string{source.Name},
	}}, nil)
	handler := NewStatusServer(pool, store).handler()

	request := localTestRequest(http.MethodPost, "/api/sources/delete", strings.NewReader(fmt.Sprintf(`{"id":%q}`, source.ID)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"retired_total":1`) {
		t.Fatalf("delete source = %d %s", recorder.Code, recorder.Body.String())
	}
	if nodes := pool.All(); len(nodes) != 1 || nodes[0].Available || !nodes[0].SourceRetired {
		t.Fatalf("pool after source delete = %#v", nodes)
	}
	select {
	case <-refreshChan:
	default:
		t.Fatal("source delete did not queue a refresh")
	}
}
