package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthResponseStatusRules(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		status   int
		accepted bool
	}{
		{name: "default exact 204", url: defaultCheckURL, status: http.StatusNoContent, accepted: true},
		{name: "default rejects generic 200", url: defaultCheckURL, status: http.StatusOK},
		{name: "custom accepts 200", url: "https://health.example/check", status: http.StatusOK, accepted: true},
		{name: "custom accepts other 2xx", url: "https://health.example/check", status: http.StatusPartialContent, accepted: true},
		{name: "custom rejects redirect", url: "https://health.example/check", status: http.StatusFound},
		{name: "custom rejects error", url: "https://health.example/check", status: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := healthResponseStatusAccepted(test.url, test.status); got != test.accepted {
				t.Fatalf("healthResponseStatusAccepted(%q, %d) = %v, want %v", test.url, test.status, got, test.accepted)
			}
		})
	}
}

func TestCheckURLDoesNotFollowRedirects(t *testing.T) {
	var connects atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		connects.Add(1)
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 302 Found\r\nLocation: http://redirected.example/final\r\nContent-Length: 0\r\n\r\n"))
	}))
	defer proxyServer.Close()

	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	px := Proxy{IP: proxyURL.Hostname(), Port: proxyURL.Port(), Protocol: "http"}
	if checkURL(px, "http://health.example/check", time.Second) {
		t.Fatal("redirect response was accepted as healthy")
	}
	if got := connects.Load(); got != 1 {
		t.Fatalf("CONNECT attempts = %d, want 1 with redirects disabled", got)
	}
}

func TestCheckCredentialCandidatesPromotesWorkingLogin(t *testing.T) {
	const (
		goodUser = "working-user"
		goodPass = "working-password"
	)
	var (
		attemptMu sync.Mutex
		attempts  []string
	)
	expectedAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte(goodUser+":"+goodPass))
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		authorization := r.Header.Get("Proxy-Authorization")
		attemptMu.Lock()
		attempts = append(attempts, authorization)
		attemptMu.Unlock()
		if authorization != expectedAuthorization {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}

		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}))
	defer proxyServer.Close()

	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	px := Proxy{
		IP:       proxyURL.Hostname(),
		Port:     proxyURL.Port(),
		Protocol: "http",
		Username: "wrong-user",
		Password: "wrong-password",
		CredentialAlternates: []ProxyCredential{
			{Username: goodUser, Password: goodPass},
		},
	}
	checked, ok, latency := checkCredentialCandidates(context.Background(), px, "http://health.example/check", time.Second)
	if !ok {
		t.Fatal("working alternate credential was not accepted")
	}
	if checked.Username != goodUser || checked.Password != goodPass {
		t.Fatalf("promoted credentials = %q/%q, want working pair", checked.Username, checked.Password)
	}
	if latency <= 0 || latency > time.Second {
		t.Fatalf("credential validation latency = %s", latency)
	}
	if len(checked.CredentialAlternates) == 0 || checked.CredentialAlternates[0].Username != "wrong-user" {
		t.Fatalf("previous primary was not retained as an alternate: %#v", checked.CredentialAlternates)
	}
	attemptMu.Lock()
	defer attemptMu.Unlock()
	if len(attempts) != 2 || attempts[1] != expectedAuthorization {
		t.Fatalf("authorization attempts = %#v, want primary then working alternate", attempts)
	}
}

func TestCheckCredentialCandidatesReservesTimeForLaterCredential(t *testing.T) {
	const goodAuthorization = "Basic Z29vZDpwYXNz" // good:pass
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Proxy-Authorization") != goodAuthorization {
			time.Sleep(900 * time.Millisecond)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}))
	defer proxyServer.Close()
	proxyURL, _ := url.Parse(proxyServer.URL)
	px := Proxy{
		IP: proxyURL.Hostname(), Port: proxyURL.Port(), Protocol: "http",
		Username: "stalled", Password: "wrong",
		CredentialAlternates: []ProxyCredential{{Username: "good", Password: "pass"}},
	}
	started := time.Now()
	checked, ok, _ := checkCredentialCandidates(context.Background(), px, "http://health.example/check", 1200*time.Millisecond)
	if !ok || checked.Username != "good" {
		t.Fatalf("later credential was starved: ok=%v checked=%+v", ok, checked)
	}
	if elapsed := time.Since(started); elapsed >= 1200*time.Millisecond {
		t.Fatalf("credential check exceeded total budget: %s", elapsed)
	}
}

func TestLookupGeoUsesBoundedValidatedUncompressedResponse(t *testing.T) {
	var requestSeen atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSeen.Store(true)
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", got)
		}
		if got := r.URL.Path; got != "/203.0.113.8" {
			t.Errorf("lookup path = %q", got)
		}
		if got := r.URL.Query().Get("fields"); got != "success,country_code,city,continent_code" {
			t.Errorf("fields = %q", got)
		}
		_, _ = io.WriteString(w, `{"success":true,"country_code":"jp","city":" Tokyo ","continent_code":"as"}`)
	}))
	defer server.Close()

	country, city, continent := lookupGeoContextWithBaseURL(context.Background(), "203.0.113.8", time.Second, server.URL)
	if !requestSeen.Load() || country != "JP" || city != "Tokyo" || continent != "AS" {
		t.Fatalf("LookupGeo = %q, %q, %q", country, city, continent)
	}
	if !strings.HasPrefix(defaultGeoLookupURL, "https://") {
		t.Fatalf("production geo endpoint is not HTTPS: %q", defaultGeoLookupURL)
	}
}

func TestLookupGeoRejectsUnsafeProviderResponses(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		contentEncoding string
	}{
		{name: "provider failure", body: `{"success":false}`},
		{name: "invalid country", body: `{"success":true,"country_code":"USA","city":"Tokyo","continent_code":"AS"}`},
		{name: "invalid continent", body: `{"success":true,"country_code":"JP","city":"Tokyo","continent_code":"XX"}`},
		{name: "city control character", body: "{\"success\":true,\"country_code\":\"JP\",\"city\":\"Tokyo\\nInjected\",\"continent_code\":\"AS\"}"},
		{name: "oversized city", body: `{"success":true,"country_code":"JP","city":"` + strings.Repeat("x", geoCityMaxBytes+1) + `","continent_code":"AS"}`},
		{name: "oversized response", body: `{"success":true,"country_code":"JP","city":"Tokyo","continent_code":"AS","padding":"` + strings.Repeat("x", int(geoResponseMaxBytes)) + `"}`},
		{name: "compressed response", body: `{"success":true,"country_code":"JP","city":"Tokyo","continent_code":"AS"}`, contentEncoding: "gzip"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.contentEncoding != "" {
					w.Header().Set("Content-Encoding", test.contentEncoding)
				}
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			country, city, continent := lookupGeoContextWithBaseURL(context.Background(), "203.0.113.8", time.Second, server.URL)
			if country != "Unknown" || city != "" || continent != "" {
				t.Fatalf("unsafe response accepted as %q, %q, %q", country, city, continent)
			}
		})
	}
}

func TestLookupGeoRejectsRedirectAndInvalidIPWithoutFollowing(t *testing.T) {
	var destinationHits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationHits.Add(1)
		_, _ = io.WriteString(w, `{"success":true,"country_code":"JP","city":"Tokyo","continent_code":"AS"}`)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer redirect.Close()

	if country, _, _ := lookupGeoContextWithBaseURL(context.Background(), "203.0.113.8", time.Second, redirect.URL); country != "Unknown" {
		t.Fatalf("redirect response accepted as country %q", country)
	}
	if got := destinationHits.Load(); got != 0 {
		t.Fatalf("geo lookup followed redirect %d time(s)", got)
	}
	if country, _, _ := lookupGeoContextWithBaseURL(context.Background(), "not-an-ip", time.Second, redirect.URL); country != "Unknown" {
		t.Fatalf("invalid IP lookup returned country %q", country)
	}
}

func TestProxyGeoLookupTargetReusesValidSourceLocation(t *testing.T) {
	tests := []struct {
		name string
		px   Proxy
		want string
	}{
		{
			name: "same exit reuses source geo",
			px:   Proxy{IP: "203.0.113.8", ExitIP: "203.0.113.8", Country: "JP", Continent: "AS"},
		},
		{
			name: "equivalent IPv4 forms reuse source geo",
			px:   Proxy{IP: "203.0.113.8", ExitIP: "::ffff:203.0.113.8", Country: "jp", Continent: "as"},
		},
		{
			name: "different exit must be looked up",
			px:   Proxy{IP: "203.0.113.8", ExitIP: "198.51.100.4", Country: "JP", Continent: "AS"},
			want: "198.51.100.4",
		},
		{
			name: "missing continent must be filled",
			px:   Proxy{IP: "203.0.113.8", ExitIP: "203.0.113.8", Country: "JP"},
			want: "203.0.113.8",
		},
		{
			name: "valid source geo without exit needs no lookup",
			px:   Proxy{IP: "203.0.113.8", Country: "JP", Continent: "AS"},
		},
		{
			name: "missing source geo looks up endpoint",
			px:   Proxy{IP: "203.0.113.8"},
			want: "203.0.113.8",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := proxyGeoLookupTarget(test.px); got != test.want {
				t.Fatalf("proxyGeoLookupTarget(%+v) = %q, want %q", test.px, got, test.want)
			}
		})
	}
}

func TestNormalizeProxyGeoFieldsSanitizesSourceMetadata(t *testing.T) {
	px := Proxy{Country: " jp ", Continent: " as ", City: "Tokyo\nInjected"}
	if !normalizeProxyGeoFields(&px) {
		t.Fatal("valid country/continent codes were rejected")
	}
	if px.Country != "JP" || px.Continent != "AS" || px.City != "" {
		t.Fatalf("normalized source geo = country %q continent %q city %q", px.Country, px.Continent, px.City)
	}

	invalid := Proxy{Country: "Japan", Continent: "Asia", City: "Tokyo"}
	if normalizeProxyGeoFields(&invalid) {
		t.Fatal("non-code source geo was accepted")
	}
	if invalid.Country != "" || invalid.Continent != "" || invalid.City != "Tokyo" {
		t.Fatalf("invalid source geo sanitization = %+v", invalid)
	}
}

func TestLookupGeoCacheCoalescesConcurrentRequestsAndExpires(t *testing.T) {
	resetGeoLookupCacheForTest()
	t.Cleanup(resetGeoLookupCacheForTest)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, `{"success":true,"country_code":"JP","city":"Tokyo","continent_code":"AS"}`)
	}))
	defer server.Close()

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make(chan string, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			country, city, continent := lookupGeoContextWithBaseURL(context.Background(), "203.0.113.8", time.Second, server.URL)
			if country != "JP" || city != "Tokyo" || continent != "AS" {
				errs <- country + "/" + city + "/" + continent
			}
		}()
	}
	wg.Wait()
	close(errs)
	for result := range errs {
		t.Errorf("coalesced lookup result = %q", result)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("concurrent provider requests = %d, want 1", got)
	}
	if country, _, _ := lookupGeoContextWithBaseURL(context.Background(), "203.0.113.8", time.Second, server.URL); country != "JP" || hits.Load() != 1 {
		t.Fatalf("fresh cache miss: country=%q hits=%d", country, hits.Load())
	}

	cacheKey := server.URL + "\x00" + "203.0.113.8"
	geoCache.Lock()
	entry, ok := geoCache.entries[cacheKey]
	if ok {
		entry.expiresAt = time.Now().Add(-time.Second)
		geoCache.entries[cacheKey] = entry
	}
	geoCache.Unlock()
	if !ok {
		t.Fatal("successful lookup was not cached")
	}
	if country, _, _ := lookupGeoContextWithBaseURL(context.Background(), "203.0.113.8", time.Second, server.URL); country != "JP" || hits.Load() != 2 {
		t.Fatalf("expired cache was not refreshed: country=%q hits=%d", country, hits.Load())
	}
}

func TestLookupGeoDoesNotCacheFailures(t *testing.T) {
	resetGeoLookupCacheForTest()
	t.Cleanup(resetGeoLookupCacheForTest)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"country_code":"JP","city":"Tokyo","continent_code":"AS"}`)
	}))
	defer server.Close()
	if country, _, _ := lookupGeoContextWithBaseURL(context.Background(), "203.0.113.8", time.Second, server.URL); country != "Unknown" {
		t.Fatalf("failed provider response returned country %q", country)
	}
	if country, _, _ := lookupGeoContextWithBaseURL(context.Background(), "203.0.113.8", time.Second, server.URL); country != "JP" || hits.Load() != 2 {
		t.Fatalf("failure was cached: country=%q hits=%d", country, hits.Load())
	}
}

func TestRefreshBaselineExitUsesDirectClientAndRefreshesValue(t *testing.T) {
	client := newDirectHTTPClient(time.Second)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("baseline transport may use environment proxy: %#v", client.Transport)
	}
	transport.CloseIdleConnections()

	baselineExitMu.Lock()
	oldBaseline := baselineExitIP
	baselineExitIP = ""
	baselineExitMu.Unlock()
	t.Cleanup(func() {
		baselineExitMu.Lock()
		baselineExitIP = oldBaseline
		baselineExitMu.Unlock()
	})

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "198.51.100.10")
			return
		}
		_, _ = io.WriteString(w, "198.51.100.11")
	}))
	defer server.Close()
	if !refreshBaselineExitWithURL(server.URL, time.Second) || BaselineExitIP() != "198.51.100.10" {
		t.Fatalf("initial baseline = %q", BaselineExitIP())
	}
	if !refreshBaselineExitWithURL(server.URL, time.Second) || BaselineExitIP() != "198.51.100.11" {
		t.Fatalf("refreshed baseline = %q", BaselineExitIP())
	}
}

func TestCheckProxiesIgnoresNonForwardingProxyIPResources(t *testing.T) {
	resource := Proxy{IP: "127.0.0.1", Port: "1", Protocol: "proxyip"}
	alive, unreachable := CheckProxies([]Proxy{resource}, 50*time.Millisecond, 1, false, "http://example.invalid/")
	if len(alive) != 0 || len(unreachable) != 0 {
		t.Fatalf("ProxyIP resource entered forwarding health result: alive=%#v unreachable=%#v", alive, unreachable)
	}
}

func TestCheckProxiesReportsFailuresByProtocolAwareKey(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	httpProxy := Proxy{IP: host, Port: port, Protocol: "http"}
	socksProxy := Proxy{IP: host, Port: port, Protocol: "socks5"}

	alive, failed := CheckProxies(
		[]Proxy{httpProxy, socksProxy},
		500*time.Millisecond,
		2,
		false,
		"http://health.test/check",
	)
	if len(alive) != 0 {
		t.Fatalf("abruptly-closed proxy endpoints reported alive: %#v", alive)
	}
	if len(failed) != 2 {
		t.Fatalf("failed keys = %#v, want one key per protocol", failed)
	}
	for _, px := range []Proxy{httpProxy, socksProxy} {
		if !failed[px.Key()] {
			t.Errorf("missing protocol-aware failed key %q in %#v", px.Key(), failed)
		}
	}
	if failed[httpProxy.Addr()] {
		t.Errorf("failure map unexpectedly contains protocol-agnostic address key %q", httpProxy.Addr())
	}
}
