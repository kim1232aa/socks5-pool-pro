package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

func TestDedupeCandidatesKeepsProtocolVariantsAtSameAddress(t *testing.T) {
	in := []Proxy{
		{IP: "192.0.2.10", Port: "8080", Protocol: "http", SourceName: "http-feed"},
		{IP: "192.0.2.10", Port: "8080", Protocol: "socks5", SourceName: "socks-feed"},
	}

	got := dedupeCandidates(in)
	if len(got) != 2 {
		t.Fatalf("dedupeCandidates returned %d candidates, want both protocol variants: %#v", len(got), got)
	}

	protocols := []string{got[0].Protocol, got[1].Protocol}
	sort.Strings(protocols)
	if want := []string{"http", "socks5"}; !reflect.DeepEqual(protocols, want) {
		t.Fatalf("protocols = %v, want %v", protocols, want)
	}
	if got[0].Key() == got[1].Key() {
		t.Fatalf("protocol variants unexpectedly share identity %q", got[0].Key())
	}
}

func TestDedupeCandidatesMergesSourcesUniquelyWithStablePrimary(t *testing.T) {
	inputs := [][]Proxy{
		{
			{IP: "192.0.2.20", Port: "1080", Protocol: "socks5", SourceName: "zeta", SourceNames: []string{"zeta", "mirror"}},
			{IP: "192.0.2.20", Port: "1080", Protocol: "socks5", SourceName: "alpha", SourceNames: []string{"alpha", "mirror"}},
		},
		{
			{IP: "192.0.2.20", Port: "1080", Protocol: "socks5", SourceName: "alpha", SourceNames: []string{"alpha", "mirror"}},
			{IP: "192.0.2.20", Port: "1080", Protocol: "socks5", SourceName: "zeta", SourceNames: []string{"zeta", "mirror"}},
		},
	}

	for i, in := range inputs {
		got := dedupeCandidates(in)
		if len(got) != 1 {
			t.Fatalf("order %d: got %d candidates, want 1", i, len(got))
		}
		if got[0].SourceName != "alpha" {
			t.Errorf("order %d: primary source = %q, want deterministic alpha", i, got[0].SourceName)
		}
		if want := []string{"alpha", "mirror", "zeta"}; !reflect.DeepEqual(got[0].SourceNames, want) {
			t.Errorf("order %d: SourceNames = %v, want unique sorted %v", i, got[0].SourceNames, want)
		}
	}
}

func TestDedupeCandidatesFillsMissingMetadataFromDuplicate(t *testing.T) {
	got := dedupeCandidates([]Proxy{
		{
			IP: "192.0.2.30", Port: "1080", Protocol: "socks5", SourceName: "zeta",
			Username: "user", Password: "secret", Country: "JP", City: "Tokyo", Continent: "AS",
		},
		{
			IP: "192.0.2.30", Port: "1080", Protocol: "socks5", SourceName: "alpha",
			Country: "Unknown",
		},
	})
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	px := got[0]
	if px.SourceName != "alpha" {
		t.Fatalf("primary source = %q, want alpha", px.SourceName)
	}
	if px.Username != "" || px.Password != "" {
		t.Errorf("stable primary credentials changed: username=%q password=%q", px.Username, px.Password)
	}
	if want := []ProxyCredential{{Username: "user", Password: "secret"}}; !reflect.DeepEqual(px.CredentialAlternates, want) {
		t.Errorf("credential alternatives = %#v, want %#v", px.CredentialAlternates, want)
	}
	if px.Country != "JP" || px.City != "Tokyo" || px.Continent != "AS" {
		t.Errorf("geo metadata was not merged: country=%q city=%q continent=%q", px.Country, px.City, px.Continent)
	}
}

func TestDedupeCandidatesRetainsBoundedCredentialVariants(t *testing.T) {
	input := make([]Proxy, 0, maxCredentialAlternates+4)
	for i := 0; i < maxCredentialAlternates+4; i++ {
		input = append(input, Proxy{
			IP: "192.0.2.44", Port: "1080", Protocol: "socks5", SourceName: "feed",
			Username: fmt.Sprintf("user-%02d", i), Password: fmt.Sprintf("pass-%02d", i),
		})
	}
	got := dedupeCandidates(input)
	if len(got) != 1 {
		t.Fatalf("dedupeCandidates() returned %d endpoints, want 1", len(got))
	}
	if len(got[0].CredentialAlternates) != maxCredentialAlternates {
		t.Fatalf("credential alternatives = %d, want cap %d", len(got[0].CredentialAlternates), maxCredentialAlternates)
	}
	if candidates := got[0].credentialCandidates(); len(candidates) != 1+maxCredentialAlternates {
		t.Fatalf("credentialCandidates() = %d, want %d", len(candidates), 1+maxCredentialAlternates)
	}
}

func TestSampleBalancedRespectsLimitAndCoversEveryBucket(t *testing.T) {
	var candidates []Proxy
	buckets := []struct {
		source   string
		protocol string
	}{
		{"alpha", "http"},
		{"alpha", "socks5"},
		{"beta", "http"},
		{"beta", "socks5"},
		{"gamma", "https"},
	}
	for bucketIndex, bucket := range buckets {
		for i := 0; i < 8; i++ {
			candidates = append(candidates, Proxy{
				IP:         fmt.Sprintf("192.0.%d.%d", bucketIndex+2, i+1),
				Port:       fmt.Sprintf("%d", 10000+bucketIndex*100+i),
				Protocol:   bucket.protocol,
				SourceName: bucket.source,
			})
		}
	}

	const limit = 10
	got := sampleBalanced(candidates, limit)
	if len(got) != limit {
		t.Fatalf("sample size = %d, want exact limit %d", len(got), limit)
	}

	covered := make(map[string]int)
	seen := make(map[string]bool)
	for _, px := range got {
		bucket := px.SourceName + "\x00" + px.Protocol
		covered[bucket]++
		if seen[px.Key()] {
			t.Fatalf("sample contains duplicate candidate key %q", px.Key())
		}
		seen[px.Key()] = true
	}
	for _, bucket := range buckets {
		key := bucket.source + "\x00" + bucket.protocol
		if covered[key] == 0 {
			t.Errorf("bucket %q received no minimum coverage: counts=%v", key, covered)
		}
	}
}

func TestSampleBalancedEdgeLimits(t *testing.T) {
	in := []Proxy{
		{IP: "192.0.2.1", Port: "1", Protocol: "http", SourceName: "a"},
		{IP: "192.0.2.2", Port: "2", Protocol: "http", SourceName: "a"},
	}
	if got := sampleBalanced(in, 0); got != nil {
		t.Fatalf("zero limit returned %#v, want nil", got)
	}
	if got := sampleBalanced(in, -1); got != nil {
		t.Fatalf("negative limit returned %#v, want nil", got)
	}
	if got := sampleBalanced(in, 1); len(got) != 1 {
		t.Fatalf("limit 1 returned %d candidates", len(got))
	}
	got := sampleBalanced(in, len(in)+1)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("non-truncating sample = %#v, want input order %#v", got, in)
	}
	got[0].IP = "203.0.113.1"
	if in[0].IP == got[0].IP {
		t.Fatal("non-truncating sample aliases the caller's slice")
	}
}

// This is an integration-level assertion around refreshPool's prioritisation,
// rather than a copy of its slice-partitioning logic. A bounded scrape spends
// its scarce slots discovering unseen nodes; already-known nodes are maintained
// by the independent full-pool recheck and must not crowd out discovery here.
func TestRefreshPoolPrioritizesUnseenCandidateWhenCapped(t *testing.T) {
	knownProxy, knownTunnels := newTestConnectProxy(t)
	unseenProxy, unseenTunnels := newTestConnectProxy(t)

	knownURL, err := url.Parse(knownProxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	unseenURL, err := url.Parse(unseenProxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	knownFeedURL := *knownURL
	knownFeedURL.Host = "localhost:" + knownURL.Port()
	unseenFeedURL := *unseenURL
	unseenFeedURL.Host = "localhost:" + unseenURL.Port()

	feed := []map[string]any{
		{"proxy": knownFeedURL.String(), "country": "US", "city": "Known"},
		{"proxy": unseenFeedURL.String(), "country": "US", "city": "Unseen"},
	}
	feedBody, err := json.Marshal(feed)
	if err != nil {
		t.Fatal(err)
	}
	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(feedBody)
	}))
	defer feedServer.Close()

	store := &ConfigStore{cfg: PoolConfig{
		Sources: []Source{{
			ID: "test-feed", Name: "test-feed", URL: feedServer.URL,
			Format: FormatEDTJSON, Enabled: true, AllowPrivate: true,
		}},
		CheckURL: "http://health.test/check",
	}}
	cfg := &Config{
		CheckTimeout:   time.Second,
		MaxConcurrent:  1,
		MaxCandidates:  1,
		ScrapeInterval: time.Minute,
	}
	known := Proxy{
		IP: "localhost", Port: knownURL.Port(), Protocol: "http",
		SourceName: "test-feed", Country: "US", Available: false,
	}
	pool := NewProxyPool()
	pool.Prime([]Proxy{known}, nil)

	refreshPool(cfg, store, pool)

	if knownTunnels.Load() != 0 {
		t.Errorf("known candidate consumed the unseen candidate's discovery slot (%d tunneled requests)", knownTunnels.Load())
	}
	if unseenTunnels.Load() == 0 {
		t.Error("unseen candidate was not checked when one discovery slot was available")
	}
	if got, ok := pool.Find(known.Key()); !ok || got.Available {
		t.Errorf("known candidate should remain untouched for the independent recheck: found=%v proxy=%#v", ok, got)
	}
	if unseenKey := "http://" + unseenFeedURL.Host; !poolHasKey(pool, unseenKey) {
		t.Errorf("unseen candidate %q was not added by the capped discovery refresh", unseenKey)
	}
}

func TestRefreshPoolRequiresThreeKnownNodeFailuresBeforeUnavailable(t *testing.T) {
	var proxyAttempts atomic.Int64
	failingProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyAttempts.Add(1)
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, "temporary proxy failure", http.StatusBadGateway)
	}))
	defer failingProxy.Close()

	feedBody, err := json.Marshal([]map[string]any{{
		"proxy": failingProxy.URL, "country": "US", "city": "Threshold",
	}})
	if err != nil {
		t.Fatal(err)
	}
	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(feedBody)
	}))
	defer feedServer.Close()

	proxyURL, err := url.Parse(failingProxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	feedProxyURL := *proxyURL
	feedProxyURL.Host = "localhost:" + proxyURL.Port()
	feedBody, err = json.Marshal([]map[string]any{{
		"proxy": feedProxyURL.String(), "country": "US", "city": "Threshold",
	}})
	if err != nil {
		t.Fatal(err)
	}
	known := Proxy{
		IP: "localhost", Port: proxyURL.Port(), Protocol: "http",
		SourceName: "threshold-feed", Country: "US", Available: true,
	}
	pool := NewProxyPool()
	pool.Prime([]Proxy{known}, nil)
	store := &ConfigStore{cfg: PoolConfig{
		Sources: []Source{{
			ID: "threshold-feed", Name: "threshold-feed", URL: feedServer.URL,
			Format: FormatEDTJSON, Enabled: true, AllowPrivate: true,
		}},
		CheckURL: "http://health.test/check",
	}}
	cfg := &Config{
		DataDir: t.TempDir(), CheckTimeout: 200 * time.Millisecond,
		MaxConcurrent: 1, MaxCandidates: 10, ScrapeInterval: time.Minute,
	}

	for attempt := 1; attempt <= healthFailureThreshold; attempt++ {
		refreshPool(cfg, store, pool)
		got, ok := pool.Find(known.Key())
		if !ok {
			t.Fatalf("known node disappeared after refresh %d", attempt)
		}
		wantAvailable := attempt < healthFailureThreshold
		if got.Available != wantAvailable {
			t.Fatalf("availability after refresh failure %d/%d = %v, want %v", attempt, healthFailureThreshold, got.Available, wantAvailable)
		}
	}
	if got := pool.stats[known.Key()].ConsecutiveHealthFailures; got != healthFailureThreshold {
		t.Fatalf("refresh failure streak = %d, want %d", got, healthFailureThreshold)
	}
	if got := proxyAttempts.Load(); got != int64(healthFailureThreshold) {
		t.Fatalf("proxy attempts = %d, want one per refresh (%d)", got, healthFailureThreshold)
	}
}

func poolHasKey(pool *ProxyPool, key string) bool {
	_, ok := pool.Find(key)
	return ok
}

func TestPeriodicRecheckDoesNotReviveTransparentProxy(t *testing.T) {
	proxyServer, _ := newTestConnectProxy(t)
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	px := Proxy{
		IP: proxyURL.Hostname(), Port: proxyURL.Port(), Protocol: "http",
		Available: false,
	}
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}}
	pool := NewProxyPool()
	pool.SetHealthCriterion(store.CheckURL())
	pool.Prime([]Proxy{px}, nil)
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 1, RequireIPChange: true}

	baselineExitMu.Lock()
	previousBaseline := baselineExitIP
	baselineExitIP = "203.0.113.77"
	baselineExitMu.Unlock()
	previousProbe := recheckProbeExitIP
	recheckProbeExitIP = func(context.Context, Proxy, time.Duration) string { return "203.0.113.77" }
	t.Cleanup(func() {
		recheckProbeExitIP = previousProbe
		baselineExitMu.Lock()
		baselineExitIP = previousBaseline
		baselineExitMu.Unlock()
	})

	if _, completed := reCheckNodes(cfg, store, pool, []Proxy{px}, 1, "test-recheck", ""); !completed {
		t.Fatal("periodic recheck did not complete")
	}
	got, _ := pool.Find(px.Key())
	if got.Available || !got.IPChangeKnown || got.IPChanged {
		t.Fatalf("transparent proxy was revived: %+v", got)
	}
}

// newTestConnectProxy implements just enough of an HTTP CONNECT proxy for
// checkURL/probeExitIP/probeAnonymity. Each tunnel returns a valid 204 HTTP
// response, keeping the test entirely local and avoiding geo lookups because
// the feed supplies Country metadata.
func newTestConnectProxy(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var tunneled atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack CONNECT: %v", err)
			return
		}
		defer conn.Close()

		if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		tunneled.Add(1)
		_, _ = fmt.Fprint(conn, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}))
	t.Cleanup(server.Close)
	return server, &tunneled
}
