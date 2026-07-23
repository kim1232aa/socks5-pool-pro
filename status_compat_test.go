package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestStatusSummaryKeepsIPPoolCompatibilityContract(t *testing.T) {
	pool := NewProxyPool()
	pool.Prime([]Proxy{
		{
			IP: "198.51.100.10", Port: "1080", Protocol: "socks5",
			Username: "user@example", Password: "pa:ss", Available: true,
		},
		{IP: "198.51.100.11", Port: "8080", Protocol: "http", Available: true},
		{IP: "198.51.100.12", Port: "1080", Protocol: "socks5", Available: false},
		// Defensive fixture: contradictory cache bits must fail closed rather
		// than leaking a hard-retired node through the extraction contract.
		{IP: "198.51.100.13", Port: "1080", Protocol: "socks5", Available: true, SourceRetired: true},
	}, nil)
	store := &ConfigStore{cfg: PoolConfig{
		Rules: []Rule{{Type: RuleMatch, Group: GroupAny}},
	}}

	summary := NewStatusServer(pool, store).buildSummary()
	if summary.AvailableTotal != 2 || len(summary.Proxies) != 2 {
		t.Fatalf("expected two healthy proxies, got available_total=%d proxies=%d", summary.AvailableTotal, len(summary.Proxies))
	}
	if got, want := summary.ActiveProxy, "socks5://user%40example:pa%3Ass@198.51.100.10:1080"; got != want {
		t.Fatalf("active_proxy = %q, want %q", got, want)
	}
	if got, want := summary.Proxies[0].SocksURL, summary.ActiveProxy; got != want {
		t.Fatalf("socks_url = %q, want %q", got, want)
	}
	if got := summary.Proxies[0]; got.Username != "user@example" || got.Password != "pa:ss" {
		t.Fatalf("status proxy omitted credentials: %#v", got)
	}
	if got, want := summary.Proxies[1].ProxyURL, "http://198.51.100.11:8080"; got != want {
		t.Fatalf("http proxy_url = %q, want %q", got, want)
	}
	if summary.Proxies[1].SocksURL != "" {
		t.Fatalf("HTTP proxy must not be exported as SOCKS: %#v", summary.Proxies[1])
	}

	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"active_proxy", "proxies", "available_total"} {
		if _, ok := wire[field]; !ok {
			t.Fatalf("/api/status compatibility field %q is missing", field)
		}
	}
	var proxyWire struct {
		Proxies []map[string]json.RawMessage `json:"proxies"`
	}
	if err := json.Unmarshal(encoded, &proxyWire); err != nil {
		t.Fatal(err)
	}
	for _, proxy := range proxyWire.Proxies {
		for field := range proxy {
			if field != "proxy_url" && field != "socks_url" && field != "username" && field != "password" {
				t.Fatalf("unexpected field %q in /api/status proxies", field)
			}
		}
	}
}

func TestStatusCompatibilityShapeIsStableWhenPoolIsEmpty(t *testing.T) {
	server := NewStatusServer(NewProxyPool(), &ConfigStore{})
	recorder := httptest.NewRecorder()
	server.handler().ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("empty status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"active_proxy", "available_total", "proxies"} {
		if _, ok := wire[field]; !ok {
			t.Fatalf("empty /api/status omitted compatibility field %q: %s", field, recorder.Body.String())
		}
	}
	if string(wire["active_proxy"]) != `""` || string(wire["proxies"]) != `[]` || string(wire["available_total"]) != `0` {
		t.Fatalf("empty compatibility values = %s", recorder.Body.String())
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("telegram_url")) {
		t.Fatalf("empty compatibility response exposed telegram_url: %s", recorder.Body.String())
	}
}

func TestStatusCompatibilityCountsComeFromOnePoolSnapshot(t *testing.T) {
	nodes := make([]Proxy, 32)
	for i := range nodes {
		nodes[i] = Proxy{IP: fmt.Sprintf("192.0.2.%d", i+1), Port: "1080", Protocol: "socks5", Available: true}
	}
	pool := NewProxyPool()
	pool.Prime(nodes, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; i < 4000; i++ {
			pool.SetAvailable(nodes[i%len(nodes)].Key(), i%3 != 0)
		}
	}()
	for i := 0; i < 2000; i++ {
		summary := server.buildSummary()
		if summary.AvailableTotal != len(summary.Proxies) {
			t.Fatalf("available_total=%d proxies=%d", summary.AvailableTotal, len(summary.Proxies))
		}
		if summary.Total != summary.AvailableTotal+summary.UnavailableTotal {
			t.Fatalf("inconsistent totals: total=%d available=%d unavailable=%d", summary.Total, summary.AvailableTotal, summary.UnavailableTotal)
		}
		if summary.ActiveProxy != "" {
			found := false
			for _, proxy := range summary.Proxies {
				if proxy.ProxyURL == summary.ActiveProxy {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("active_proxy %q absent from healthy proxy snapshot", summary.ActiveProxy)
			}
		}
	}
	writer.Wait()
}
