package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestNodesPageFiltersSortsBoundsAndKeepsLegacyNodesArray(t *testing.T) {
	server, nodes := pagedNodeTestServer(t)
	handler := server.handler()

	// The legacy route remains a bare array for existing integrations.
	legacy := httptest.NewRecorder()
	handler.ServeHTTP(legacy, localTestRequest(http.MethodGet, "/api/nodes", nil))
	if got, want := legacy.Code, http.StatusOK; got != want {
		t.Fatalf("legacy /api/nodes status = %d, want %d", got, want)
	}
	var legacyNodes []NodeView
	if err := json.Unmarshal(legacy.Body.Bytes(), &legacyNodes); err != nil {
		t.Fatalf("legacy /api/nodes is no longer a JSON array: %v; body=%s", err, legacy.Body.String())
	}
	if got, want := len(legacyNodes), len(nodes); got != want {
		t.Fatalf("legacy /api/nodes length = %d, want %d", got, want)
	}

	page := getNodePage(t, handler, "/api/nodes/page?page=2&page_size=2&sort=latency")
	if page.Page != 2 || page.PageSize != 2 {
		t.Fatalf("page metadata = page %d size %d, want page 2 size 2", page.Page, page.PageSize)
	}
	if page.FilteredTotal != 5 || page.PoolTotal != 5 || page.AvailableTotal != 4 || page.UnavailableTotal != 1 {
		t.Fatalf("page totals = %#v", page)
	}
	if got, want := nodeKeys(page.Nodes), []string{nodes["socks-us-2"].Key(), nodes["http-de"].Key()}; !sameStrings(got, want) {
		t.Fatalf("latency-sorted page 2 keys = %v, want %v", got, want)
	}
	if page.Active == nil || page.Active.Key != nodes["socks-us"].Key() {
		t.Fatalf("active node = %#v, want %q", page.Active, nodes["socks-us"].Key())
	}

	countries := make(map[string]NodeCountrySummary, len(page.Countries))
	for _, country := range page.Countries {
		countries[country.Country] = country
	}
	for _, want := range []NodeCountrySummary{
		{Country: "US", Continent: "NA", Total: 2, Available: 2},
		{Country: "JP", Continent: "AS", Total: 2, Available: 1},
		{Country: "DE", Continent: "EU", Total: 1, Available: 1},
	} {
		if got, ok := countries[want.Country]; !ok || got.Total != want.Total || got.Available != want.Available || got.Continent != want.Continent {
			t.Errorf("country summary %s = %#v, want %#v", want.Country, got, want)
		}
	}

	filtered := getNodePage(t, handler, "/api/nodes/page?country=jp&only_changed=1&sort=latency")
	if got, want := nodeKeys(filtered.Nodes), []string{nodes["https-jp-dead"].Key()}; !sameStrings(got, want) {
		t.Fatalf("JP changed nodes = %v, want %v", got, want)
	}
	if filtered.FilteredTotal != 1 {
		t.Fatalf("JP changed total = %d, want 1", filtered.FilteredTotal)
	}

	available := getNodePage(t, handler, "/api/nodes/page?country=jp&only_changed=1&available=1")
	if available.FilteredTotal != 0 || len(available.Nodes) != 0 {
		t.Fatalf("available JP changed nodes = %#v, want no rows", available)
	}

	search := getNodePage(t, handler, "/api/nodes/page?search=203.0.113.4")
	if got, want := nodeKeys(search.Nodes), []string{nodes["http-de"].Key()}; !sameStrings(got, want) {
		t.Fatalf("exit-IP search keys = %v, want %v", got, want)
	}

	bounded := getNodePage(t, handler, "/api/nodes/page?page=999&page_size=100000")
	if bounded.PageSize != maxNodePageSize || bounded.Page != 1 || len(bounded.Nodes) != 5 {
		t.Fatalf("bounded page = %#v, want clamped page 1 size %d and 5 rows", bounded, maxNodePageSize)
	}
}

func TestHandleNodeVerifyUpdatesHealthStateImmediately(t *testing.T) {
	proxy, tunnels := newTestConnectProxy(t)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	px := Proxy{
		IP: proxyURL.Hostname(), Port: proxyURL.Port(), Protocol: "http",
		Country: "US", Available: false,
	}
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})

	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodPost, "/api/nodes/verify", bytes.NewBufferString(`{"key":"`+px.Key()+`"}`))
	server.handleNodeVerify(recorder, request)
	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("verify status = %d, want %d; body=%s", got, want, recorder.Body.String())
	}
	var body struct {
		Reachable           bool  `json:"reachable"`
		Available           bool  `json:"available"`
		Attempts            int   `json:"attempts"`
		ConsecutiveFailures int   `json:"consecutive_failures"`
		LatencyMs           int64 `json:"latency_ms"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode verify response: %v", err)
	}
	if !body.Reachable || !body.Available || body.Attempts != 1 || body.ConsecutiveFailures != 0 || body.LatencyMs < 0 {
		t.Fatalf("verify response = %#v, want reachable with a non-negative latency", body)
	}
	updated, ok := pool.Find(px.Key())
	if !ok || !updated.Available {
		t.Fatalf("verified proxy availability = found=%v proxy=%#v, want restored available", ok, updated)
	}
	if successes, failures := pool.StatsOf(px.Key()); successes != 1 || failures != 0 {
		t.Fatalf("verify stats = %d/%d, want 1/0", successes, failures)
	}
	if tunnels.Load() == 0 {
		t.Fatal("manual verification did not dial the proxy")
	}
}

func TestHandleNodeVerifyDebouncesThreeFailedManualObservationsWithoutExitProbe(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var accepted atomic.Int64
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close() // make the HTTP CONNECT health check fail immediately
		}
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	px := Proxy{IP: host, Port: port, Protocol: "http", Available: true}
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})

	for observation := 1; observation <= healthFailureThreshold; observation++ {
		recorder := httptest.NewRecorder()
		request := localTestRequest(http.MethodPost, "/api/nodes/verify", bytes.NewBufferString(`{"key":"`+px.Key()+`"}`))
		server.handleNodeVerify(recorder, request)
		if got, want := recorder.Code, http.StatusOK; got != want {
			t.Fatalf("verify %d status = %d, want %d; body=%s", observation, got, want, recorder.Body.String())
		}
		var body struct {
			Reachable           bool `json:"reachable"`
			Available           bool `json:"available"`
			Attempts            int  `json:"attempts"`
			ConsecutiveFailures int  `json:"consecutive_failures"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode verify response: %v", err)
		}
		wantAvailable := observation < healthFailureThreshold
		if body.Reachable || body.Available != wantAvailable || body.Attempts != manualNodeVerifyMaxAttempts || body.ConsecutiveFailures != observation {
			t.Fatalf("failed observation %d response = %#v", observation, body)
		}
		updated, ok := pool.Find(px.Key())
		if !ok || updated.Available != wantAvailable {
			t.Fatalf("observation %d availability = found=%v proxy=%#v", observation, ok, updated)
		}
		if successes, failures := pool.StatsOf(px.Key()); successes != 0 || failures != observation {
			t.Fatalf("observation %d stats = %d/%d, want 0/%d", observation, successes, failures, observation)
		}
	}
	if got, want := accepted.Load(), int64(healthFailureThreshold*manualNodeVerifyMaxAttempts); got != want {
		t.Fatalf("proxy connections = %d, want %d bounded retry attempts and no exit probe", got, want)
	}
}

func pagedNodeTestServer(t *testing.T) (*StatusServer, map[string]Proxy) {
	t.Helper()
	nodes := map[string]Proxy{
		"socks-us": {
			IP: "198.51.100.1", Port: "1080", Protocol: "socks5", Country: "US", City: "New York", Continent: "NA",
			ExitIP: "203.0.113.1", Available: true, IPChangeKnown: true, IPChanged: true, LatencyMs: 50,
		},
		"http-jp": {
			IP: "198.51.100.2", Port: "8080", Protocol: "http", Country: "JP", City: "Tokyo", Continent: "AS",
			ExitIP: "203.0.113.2", Available: true, IPChangeKnown: true, IPChanged: false, LatencyMs: 10,
		},
		"https-jp-dead": {
			IP: "198.51.100.3", Port: "443", Protocol: "https", Country: "JP", City: "Osaka", Continent: "AS",
			ExitIP: "203.0.113.3", Available: false, IPChangeKnown: true, IPChanged: true, LatencyMs: 5,
		},
		"http-de": {
			IP: "198.51.100.4", Port: "3128", Protocol: "http", Country: "DE", City: "Berlin", Continent: "EU",
			ExitIP: "203.0.113.4", Available: true, IPChangeKnown: true, IPChanged: true, LatencyMs: 30,
		},
		"socks-us-2": {
			IP: "198.51.100.5", Port: "1080", Protocol: "socks5", Country: "US", City: "Seattle", Continent: "NA",
			ExitIP: "203.0.113.5", Available: true, IPChangeKnown: false, IPChanged: true, LatencyMs: 20,
		},
	}
	pool := NewProxyPool()
	pool.Prime([]Proxy{
		nodes["socks-us"], nodes["http-jp"], nodes["https-jp-dead"], nodes["http-de"], nodes["socks-us-2"],
	}, nil)
	if !pool.ForceSticky(GroupAny, nodes["socks-us"].Key()) {
		t.Fatal("ForceSticky(ANY) = false")
	}
	return NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{Rules: []Rule{{Type: RuleMatch, Group: GroupAny}}}}), nodes
}

func getNodePage(t *testing.T, handler http.Handler, path string) NodePageResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, path, nil))
	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("GET %s status = %d, want %d; body=%s", path, got, want, recorder.Body.String())
	}
	var page NodePageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode GET %s: %v; body=%s", path, err, recorder.Body.String())
	}
	return page
}

func nodeKeys(nodes []NodeView) []string {
	keys := make([]string, 0, len(nodes))
	for _, node := range nodes {
		keys = append(keys, node.Key)
	}
	return keys
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
