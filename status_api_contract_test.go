package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestV1HealthyProxyPageAndPickContract(t *testing.T) {
	pool := NewProxyPool()
	pool.Prime([]Proxy{
		{IP: "192.0.2.10", Port: "1080", Protocol: "socks5", Username: "user", Password: "pass", Country: "JP", Available: true, LatencyMs: 20},
		{IP: "192.0.2.11", Port: "8080", Protocol: "http", Country: "US", Available: true, LatencyMs: 50},
		{IP: "192.0.2.12", Port: "1080", Protocol: "socks5", Country: "JP", Available: false},
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
		t.Fatalf("reliability-only mutation destabilized v1 page: before=%#v after=%#v", page, stablePage)
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
	if changedPage.SnapshotID == page.SnapshotID {
		t.Fatalf("output-field speed mutation reused v1 snapshot token %q", page.SnapshotID)
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

func TestUnauthenticatedManagementRequiresLoopbackHost(t *testing.T) {
	handler := NewStatusServer(NewProxyPool(), &ConfigStore{}).handler()
	for _, host := range []string{"attacker.test", "172.17.0.2:8080"} {
		request := localTestRequest(http.MethodGet, "/api/status", nil)
		request.Host = host
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"code":"untrusted_host"`) {
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

	authenticated := NewStatusServerWithAdminCredentials(NewProxyPool(), &ConfigStore{}, "admin", "secret").handler()
	request := localTestRequest(http.MethodGet, "/api/status?compact=1", nil)
	request.Host = "admin.example"
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
		for {
			select {
			case <-refreshChan:
			default:
				for {
					select {
					case <-recheckChan:
					default:
						return
					}
				}
			}
		}
	}
	reset()
	t.Cleanup(reset)
}
