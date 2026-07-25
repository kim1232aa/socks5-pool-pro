package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestPageWindowLimitRejectsOverflowAndMaxInt(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, pageSize := range []int{1, 2, maxNodePageSize} {
		if got := pageWindowLimit(maxInt, pageSize); got != 0 {
			t.Errorf("pageWindowLimit(MaxInt, %d) = %d, want 0", pageSize, got)
		}
	}
	if got, want := pageWindowLimit(3, 2), 6; got != want {
		t.Fatalf("pageWindowLimit(3, 2) = %d, want %d", got, want)
	}
}

func TestPageCollectorWindowBoundsFirstAndLastPages(t *testing.T) {
	for _, total := range []int{100, 1000, 10000} {
		if limit, reverse := pageCollectorWindow(total, 0, 1); limit != 1 || reverse {
			t.Errorf("total=%d first page window = (%d, %v), want (1, false)", total, limit, reverse)
		}
		if limit, reverse := pageCollectorWindow(total, total-1, total); limit != 1 || !reverse {
			t.Errorf("total=%d last page window = (%d, %v), want (1, true)", total, limit, reverse)
		}
	}
}

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
	for _, view := range legacyNodes {
		if proxy, ok := nodes["socks-us"]; ok && view.Key == proxy.Key() {
			if view.ProxyURL != proxy.ConsumerURL() || view.Username != proxy.Username || view.Password != proxy.Password {
				t.Fatalf("legacy node credentials = %#v, want URL %q and original fields", view, proxy.ConsumerURL())
			}
		}
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

	maxInt := int(^uint(0) >> 1)
	hugePath := fmt.Sprintf("/api/nodes/page?page=%d&page_size=2&sort=latency", maxInt)
	huge := getNodePage(t, handler, hugePath)
	if huge.Page != 3 || huge.PageSize != 2 || huge.FilteredTotal != 5 || len(huge.Nodes) != 1 || huge.Nodes[0].Key != nodes["socks-us"].Key() {
		t.Fatalf("overflow-safe last page = %#v", huge)
	}
}

func TestNodePageSnapshotRejectsStaleActiveCursor(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, server *StatusServer, nodes map[string]Proxy)
	}{
		{
			name: "force sticky",
			change: func(t *testing.T, server *StatusServer, nodes map[string]Proxy) {
				if !server.pool.ForceSticky(GroupAny, nodes["http-jp"].Key()) {
					t.Fatal("ForceSticky(ANY) = false")
				}
			},
		},
		{
			name: "rotate sticky",
			change: func(t *testing.T, server *StatusServer, _ map[string]Proxy) {
				server.pool.SetAuto(GroupAny)
				if _, ok := server.pool.RotateSticky(GroupAny); !ok {
					t.Fatal("RotateSticky(ANY) = false")
				}
			},
		},
		{
			name: "pick after clearing pin",
			change: func(t *testing.T, server *StatusServer, nodes map[string]Proxy) {
				server.pool.SetAuto(GroupAny)
				if _, ok, _ := server.pool.PickExcluding(GroupAny, nil, map[string]bool{nodes["socks-us"].Key(): true}); !ok {
					t.Fatal("PickExcluding(ANY) = false")
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, nodes := pagedNodeTestServer(t)
			handler := server.handler()
			original := getNodePage(t, handler, "/api/nodes/page?page=1&page_size=2")
			test.change(t, server, nodes)

			recorder := httptest.NewRecorder()
			path := "/api/nodes/page?page=2&page_size=2&snapshot_id=" + url.QueryEscape(original.SnapshotID)
			handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, path, nil))
			if got, want := recorder.Code, http.StatusConflict; got != want {
				t.Fatalf("stale active snapshot status = %d, want %d; body=%s", got, want, recorder.Body.String())
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode stale snapshot response: %v", err)
			}
			if body.Code != "snapshot_changed" {
				t.Fatalf("stale active snapshot code = %q, want snapshot_changed", body.Code)
			}
			if got := recorder.Header().Get("X-Snapshot-ID"); got == "" || got == original.SnapshotID {
				t.Fatalf("current snapshot header = %q, want a new token", got)
			}
		})
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
	if successes, failures := pool.StatsOf(px.Key()); successes != 0 || failures != 0 {
		t.Fatalf("manual health observation changed forwarding stats = %d/%d", successes, failures)
	}
	if tunnels.Load() == 0 {
		t.Fatal("manual verification did not dial the proxy")
	}
}

func TestHandleNodeVerifyMakesFinalManualFailureTerminalWithoutExitProbe(t *testing.T) {
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
			_ = conn.Close()
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

	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodPost, "/api/nodes/verify", bytes.NewBufferString(`{"key":"`+px.Key()+`"}`))
	server.handleNodeVerify(recorder, request)
	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("verify status = %d, want %d; body=%s", got, want, recorder.Body.String())
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
	if body.Reachable || body.Available || body.Attempts != manualNodeVerifyMaxAttempts || body.ConsecutiveFailures != 1 {
		t.Fatalf("failed observation response = %#v", body)
	}
	updated, ok := pool.Find(px.Key())
	if !ok || updated.Available || !updated.HealthInvalidated {
		t.Fatalf("terminal failure state = found=%v proxy=%#v", ok, updated)
	}
	if successes, failures := pool.StatsOf(px.Key()); successes != 0 || failures != 0 {
		t.Fatalf("manual health observation changed forwarding stats = %d/%d", successes, failures)
	}
	if got, want := accepted.Load(), int64(manualNodeVerifyMaxAttempts); got != want {
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
