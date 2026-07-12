package main

import (
	"fmt"
	"reflect"
	"testing"
)

func TestCompactStatusStreamingMatchesCompatibilitySummary(t *testing.T) {
	availableHTTP := Proxy{
		IP: "192.0.2.10", Port: "8080", Protocol: "http", Available: true,
		Country: "JP", LatencyMs: 80, SpeedKbps: 2200, SourceName: "feed-a",
	}
	availableSOCKS := Proxy{
		IP: "2001:db8::20", Port: "1080", Protocol: "socks5", Available: true,
		Country: "JP", LatencyMs: 30, SpeedKbps: 900,
		SourceName: "feed-b", SourceNames: []string{"feed-b", "shared"},
	}
	softUnavailableJP := Proxy{
		IP: "192.0.2.30", Port: "3128", Protocol: "http", Available: false,
		Country: "JP", LatencyMs: 5, SpeedKbps: 9000, SourceName: "feed-a",
	}
	softUnavailableUS := Proxy{
		IP: "192.0.2.40", Port: "3128", Protocol: "http", Available: false,
		Country: "US", LatencyMs: 10, SourceName: "feed-a",
	}
	retired := Proxy{
		IP: "192.0.2.50", Port: "80", Protocol: "http", Available: true,
		Country: "DE", SourceName: "retired", SourceRetired: true,
	}
	invalidated := Proxy{
		IP: "192.0.2.60", Port: "80", Protocol: "http", Available: true,
		Country: "FR", SourceName: "feed-a", HealthInvalidated: true,
	}
	policyExcluded := Proxy{
		IP: "192.0.2.70", Port: "80", Protocol: "http", Available: true,
		Country: "GB", SourceName: "feed-a", PolicyExcluded: true,
	}

	groups := []Group{
		{ID: "sticky", Name: "sticky-jp", Strategy: StrategySticky, Countries: []string{"JP"}},
		{ID: "latency", Name: "latency-jp", Strategy: StrategyLatency, Countries: []string{"JP"}},
		{ID: "speed", Name: "speed-jp", Strategy: StrategySpeed, Countries: []string{"JP"}},
		{ID: "score", Name: "score-jp", Strategy: StrategyScore, Countries: []string{"JP"}},
		{ID: "round", Name: "round-jp", Strategy: StrategyRoundRobin, Countries: []string{"JP"}},
		{ID: "random", Name: "random-jp", Strategy: StrategyRandom, Countries: []string{"JP"}},
		{ID: "source", Name: "shared-source", Strategy: StrategyLatency, Sources: []string{"shared"}},
		{ID: "soft", Name: "soft-us", Strategy: StrategyLatency, Countries: []string{"US"}},
		{ID: "hard", Name: "hard-retired", Strategy: StrategySticky, Nodes: []string{retired.Key()}},
		{ID: "default", Name: "empty-strategy", Countries: []string{"JP"}},
	}
	store := &ConfigStore{cfg: PoolConfig{Groups: groups}}
	pool := NewProxyPool()
	pool.Prime([]Proxy{
		availableHTTP, availableSOCKS, softUnavailableJP, softUnavailableUS,
		retired, invalidated, policyExcluded,
	}, []Proxy{{IP: "198.51.100.10", Port: "443", Protocol: "proxyip"}})
	pool.mu.Lock()
	pool.groupState[GroupAny] = &groupCursor{stickyKey: softUnavailableJP.Key(), pinned: true}
	pool.groupState["sticky-jp"] = &groupCursor{stickyKey: availableSOCKS.Key(), pinned: true}
	pool.groupState["round-jp"] = &groupCursor{lastPicked: availableSOCKS.Key()}
	pool.groupState["random-jp"] = &groupCursor{lastPicked: availableSOCKS.Key()}
	pool.stats[availableHTTP.Key()] = &nodeStats{Successes: 1, Failures: 8, LastLatencyMs: 200}
	pool.stats[availableSOCKS.Key()] = &nodeStats{Successes: 9, Failures: 1, LastLatencyMs: 20}
	pool.healthRecheckPending = true
	pool.mu.Unlock()

	server := NewStatusServer(pool, store)
	full := server.buildSummaryWithProxies(true)
	compact := server.buildSummaryWithProxies(false)
	full.Proxies = nil
	if !reflect.DeepEqual(compact, full) {
		t.Fatalf("streaming compact summary differs from compatibility summary:\ncompact=%#v\nfull=%#v", compact, full)
	}

	views := make(map[string]GroupView, len(compact.Groups))
	for _, view := range compact.Groups {
		views[view.Name] = view
	}
	if any := views[GroupAny]; any.Count != 7 || !any.Pinned || any.Current != availableHTTP.Addr() {
		t.Fatalf("ANY hard-boundary/available/pinned selection = %#v", any)
	}
	if round := views["round-jp"]; round.Count != 2 || !round.Dynamic || round.Current != availableSOCKS.Addr() {
		t.Fatalf("round-robin current = %#v", round)
	}
	if soft := views["soft-us"]; soft.Count != 1 || soft.Current != softUnavailableUS.Addr() {
		t.Fatalf("soft-unavailable fallback = %#v", soft)
	}
	if hard := views["hard-retired"]; hard.Count != 0 || hard.Current != "" {
		t.Fatalf("hard-unroutable group leaked into compact status = %#v", hard)
	}
	if compact.AvailableTotal != 2 || compact.UnavailableTotal != 5 || compact.ActiveProxy != availableHTTP.ConsumerURL() {
		t.Fatalf("compact health totals/active = available=%d unavailable=%d active=%q", compact.AvailableTotal, compact.UnavailableTotal, compact.ActiveProxy)
	}
}

var compactStatusPoolSnapshotSink compactStatusPoolSnapshot

func TestCompactStatusStreamingMemoryDoesNotScaleWithPoolSize(t *testing.T) {
	groups := []Group{{
		ID: "source", Name: "source", Strategy: StrategyLatency,
		Sources: []string{"feed"},
	}}
	measure := func(size int) int64 {
		proxies := make([]Proxy, size)
		for i := range proxies {
			proxies[i] = Proxy{
				IP: fmt.Sprintf("198.51.%d.%d", i/250, i%250+1), Port: "8080",
				Protocol: "http", Available: true, LatencyMs: int64(i + 1),
				SourceName: "feed", SourceNames: []string{"feed", "mirror"},
			}
		}
		pool := NewProxyPool()
		pool.Prime(proxies, nil)
		server := NewStatusServer(pool, &ConfigStore{})
		result := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				compactStatusPoolSnapshotSink = server.captureCompactStatusPoolSnapshot(groups)
			}
		})
		return result.AllocedBytesPerOp()
	}

	smallBytes := measure(8)
	largeBytes := measure(4096)
	t.Logf("compact snapshot allocation: 8 nodes=%d B/op, 4096 nodes=%d B/op", smallBytes, largeBytes)
	// The returned group views and selected address strings have fixed size.
	// Leave ample runtime/compiler variance while rejecting an O(pool size)
	// detached []Proxy or per-node SourceNames copy (both are hundreds of KiB).
	if largeBytes > smallBytes+32*1024 {
		t.Fatalf("compact snapshot allocation scales with pool: 8 nodes=%d B/op, 4096 nodes=%d B/op", smallBytes, largeBytes)
	}
}
