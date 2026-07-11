package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func testProxy(protocol, ip, port string, available bool) Proxy {
	return Proxy{Protocol: protocol, IP: ip, Port: port, Available: available}
}

func TestProxyPoolKeyIndexTracksLifecycleAndProtocolVariants(t *testing.T) {
	p := NewProxyPool()
	httpProxy := testProxy("http", "192.0.2.200", "8080", true)
	socksProxy := testProxy("socks5", httpProxy.IP, httpProxy.Port, true)
	deadProxy := testProxy("https", "192.0.2.201", "443", false)
	p.Prime([]Proxy{httpProxy, socksProxy, deadProxy}, nil)
	assertProxyIndexInvariant(t, p)

	if got, want := p.proxyIndex[httpProxy.Key()], 0; got != want {
		t.Fatalf("HTTP index = %d, want %d", got, want)
	}
	if got, want := p.proxyIndex[socksProxy.Key()], 1; got != want {
		t.Fatalf("SOCKS index = %d, want %d", got, want)
	}
	p.SetAvailable(socksProxy.Key(), false)
	if got, ok := p.Find(httpProxy.Key()); !ok || !got.Available {
		t.Fatalf("HTTP variant changed with SOCKS update: found=%v proxy=%+v", ok, got)
	}
	if got, ok := p.Find(socksProxy.Key()); !ok || got.Available {
		t.Fatalf("SOCKS variant was not independently updated: found=%v proxy=%+v", ok, got)
	}
	assertProxyIndexInvariant(t, p)

	freshHTTP := httpProxy
	freshHTTP.Country = "CA"
	newProxy := testProxy("socks5", "192.0.2.202", "1080", true)
	p.Update([]Proxy{freshHTTP, newProxy}, nil)
	assertProxyIndexInvariant(t, p)
	if got, ok := p.Find(freshHTTP.Key()); !ok || got.Country != "CA" {
		t.Fatalf("updated HTTP node = found=%v proxy=%+v", ok, got)
	}
	if got, ok := p.Find(newProxy.Key()); !ok || got.Key() != newProxy.Key() {
		t.Fatalf("new node missing after Update: found=%v proxy=%+v", ok, got)
	}

	if removed := p.ClearUnavailable(); removed != 2 {
		t.Fatalf("ClearUnavailable removed %d nodes, want SOCKS variant and dead node", removed)
	}
	assertProxyIndexInvariant(t, p)
	for _, key := range []string{socksProxy.Key(), deadProxy.Key()} {
		if _, ok := p.Find(key); ok {
			t.Fatalf("removed key %q still resolves", key)
		}
		p.mu.RLock()
		_, indexed := p.proxyIndex[key]
		p.mu.RUnlock()
		if indexed {
			t.Fatalf("removed key %q remains in index", key)
		}
	}

	// Prime replaces the forwarding slice, so the prior index must not leak
	// into a newly restored cache snapshot.
	p.Prime([]Proxy{socksProxy}, nil)
	assertProxyIndexInvariant(t, p)
	if _, ok := p.Find(httpProxy.Key()); ok {
		t.Fatal("old HTTP key survived replacement Prime")
	}
	if got, ok := p.Find(socksProxy.Key()); !ok || got.Key() != socksProxy.Key() {
		t.Fatalf("replacement Prime lookup = found=%v proxy=%+v", ok, got)
	}
}

func TestProxyPoolKeyIndexRebuildPreservesFirstDuplicateSemantics(t *testing.T) {
	p := NewProxyPool()
	first := testProxy("http", "192.0.2.210", "8080", true)
	first.Country = "US"
	second := first
	second.Country = "CA"
	socks := testProxy("socks5", first.IP, first.Port, true)
	p.Prime([]Proxy{first, second, socks}, nil)
	assertProxyIndexInvariant(t, p)

	p.mu.RLock()
	index := p.proxyIndex[first.Key()]
	p.mu.RUnlock()
	if index != 0 {
		t.Fatalf("duplicate key index = %d, want first entry 0", index)
	}
	if got, ok := p.Find(first.Key()); !ok || got.Country != first.Country {
		t.Fatalf("Find duplicate key = found=%v proxy=%+v, want first %+v", ok, got, first)
	}
	p.SetAvailable(first.Key(), false)
	all := p.All()
	if all[0].Available || !all[1].Available {
		t.Fatalf("indexed mutation did not preserve first-match behavior: %#v", all[:2])
	}
}

func TestProxyPoolKeyIndexConcurrentReadPaths(t *testing.T) {
	p := NewProxyPool()
	proxies := []Proxy{
		testProxy("http", "192.0.2.220", "8080", true),
		testProxy("socks5", "192.0.2.221", "1080", true),
		testProxy("https", "192.0.2.222", "443", true),
	}
	p.Prime(proxies, nil)

	start := make(chan struct{})
	errs := make(chan error, 8)
	var readers sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		readers.Add(1)
		go func(reader int) {
			defer readers.Done()
			<-start
			for i := 0; i < 250; i++ {
				key := proxies[(reader+i)%len(proxies)].Key()
				if got, ok := p.Find(key); !ok || got.Key() != key {
					errs <- fmt.Errorf("Find(%q) = %+v, %v", key, got, ok)
					return
				}
				_ = p.All()
				p.StatsOf(key)
			}
		}(reader)
	}
	close(start)

	for i := 0; i < 100; i++ {
		px := proxies[i%len(proxies)]
		p.SetAvailable(px.Key(), i%2 == 0)
		p.UpdateLatency(px.Key(), int64(i+1))
		if i%10 == 0 {
			fresh := px
			fresh.Country = "US"
			p.Update([]Proxy{fresh}, nil)
		}
	}
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assertProxyIndexInvariant(t, p)
}

func assertProxyIndexInvariant(t *testing.T, p *ProxyPool) {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()

	want := make(map[string]int, len(p.proxies))
	for i, px := range p.proxies {
		if _, exists := want[px.Key()]; !exists {
			want[px.Key()] = i
		}
	}
	if len(p.proxyIndex) != len(want) {
		t.Fatalf("index size = %d, want %d; index=%v", len(p.proxyIndex), len(want), p.proxyIndex)
	}
	for key, wantIndex := range want {
		if gotIndex, ok := p.proxyIndex[key]; !ok || gotIndex != wantIndex {
			t.Fatalf("index[%q] = %d, present=%v; want %d", key, gotIndex, ok, wantIndex)
		}
	}
	for key, index := range p.proxyIndex {
		if index < 0 || index >= len(p.proxies) || p.proxies[index].Key() != key {
			t.Fatalf("stale index[%q] = %d for proxies=%#v", key, index, p.proxies)
		}
	}
}

func TestObserveHealthResultUsesConsecutiveFailureThresholdAndSuccessReset(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "192.0.2.230", "1080", true)
	p.Prime([]Proxy{px}, nil)

	for attempt := 1; attempt < healthFailureThreshold; attempt++ {
		if !p.ObserveHealthResult(px.Key(), false, int64(100+attempt)) {
			t.Fatalf("ObserveHealthResult failure %d did not find node", attempt)
		}
		got, _ := p.Find(px.Key())
		if !got.Available {
			t.Fatalf("node unavailable after transient failure %d/%d", attempt, healthFailureThreshold)
		}
		if streak := p.stats[px.Key()].ConsecutiveHealthFailures; streak != attempt {
			t.Fatalf("failure streak after attempt %d = %d", attempt, streak)
		}
	}

	if !p.ObserveHealthResult(px.Key(), false, 103) {
		t.Fatal("third failure did not find node")
	}
	got, _ := p.Find(px.Key())
	if got.Available {
		t.Fatal("node remained available after third consecutive background failure")
	}

	if !p.ObserveHealthResult(px.Key(), true, 47) {
		t.Fatal("successful observation did not find node")
	}
	got, _ = p.Find(px.Key())
	if !got.Available || got.LatencyMs != 47 {
		t.Fatalf("successful observation did not immediately restore node: %+v", got)
	}
	st := p.stats[px.Key()]
	if st.ConsecutiveHealthFailures != 0 || st.Successes != 1 || st.Failures != healthFailureThreshold || st.LastLatencyMs != 47 {
		t.Fatalf("stats after recovery = %+v", st)
	}

	// A new streak starts from zero after success; two more misses remain
	// tolerated instead of inheriting the previous failure history.
	p.ObserveHealthResult(px.Key(), false, 0)
	p.ObserveHealthResult(px.Key(), false, 0)
	got, _ = p.Find(px.Key())
	if !got.Available || p.stats[px.Key()].ConsecutiveHealthFailures != 2 {
		t.Fatalf("post-recovery failure streak was not reset: proxy=%+v stats=%+v", got, p.stats[px.Key()])
	}

	// Live client failures and policy decisions use this path and remain
	// immediate, independent of the background threshold.
	p.SetAvailable(px.Key(), false)
	got, _ = p.Find(px.Key())
	if got.Available {
		t.Fatal("SetAvailable(false) was incorrectly delayed by health threshold")
	}
	p.SetAvailable(px.Key(), true)
	if p.stats[px.Key()].ConsecutiveHealthFailures != 0 {
		t.Fatalf("explicit successful recovery did not reset streak: %+v", p.stats[px.Key()])
	}

	p.ObserveHealthResult(px.Key(), false, 0)
	p.ObserveHealthResult(px.Key(), false, 0)
	p.RecordResult(px.Key(), true, 33)
	if p.stats[px.Key()].ConsecutiveHealthFailures != 0 {
		t.Fatalf("successful real/manual result did not reset background streak: %+v", p.stats[px.Key()])
	}
	p.ObserveHealthResult(px.Key(), false, 0)
	got, _ = p.Find(px.Key())
	if !got.Available {
		t.Fatal("one background failure inherited the streak cleared by RecordResult success")
	}
}

func TestUpdatePreservesDurableMeasurementsOnPartialProbe(t *testing.T) {
	p := NewProxyPool()
	old := testProxy("socks5", "192.0.2.10", "1080", true)
	old.SourceName = "old-source"
	old.LatencyMs = 900
	old.SpeedKbps = 4321.5
	old.SpeedTestedAt = 1_700_000_000
	old.SpeedBytes = 3_000_000
	old.SpeedDurationMs = 5555
	old.ExitIP = "198.51.100.20"
	old.IPChanged = true
	old.IPChangeKnown = true
	old.Anonymity = "elite"
	old.Country = "JP"
	old.City = "Tokyo"
	old.Continent = "AS"
	p.Prime([]Proxy{old}, nil)

	fresh := testProxy("socks5", old.IP, old.Port, false)
	fresh.SourceName = "new-source"
	fresh.LatencyMs = 120
	// A scrape does not own these values. Even if a caller accidentally
	// supplies speed fields, the existing on-demand sample remains canonical.
	fresh.SpeedKbps = 9999
	fresh.SpeedTestedAt = 1
	fresh.SpeedBytes = 2
	fresh.SpeedDurationMs = 3
	p.Update([]Proxy{fresh}, nil)

	got, ok := p.Find(old.Key())
	if !ok {
		t.Fatal("updated proxy not found")
	}
	if !got.Available || got.LatencyMs != 120 || got.SourceName != "new-source" {
		t.Fatalf("fresh health data was not applied: %+v", got)
	}
	if got.SpeedKbps != old.SpeedKbps || got.SpeedTestedAt != old.SpeedTestedAt ||
		got.SpeedBytes != old.SpeedBytes || got.SpeedDurationMs != old.SpeedDurationMs {
		t.Fatalf("speed sample was erased or replaced: %+v", got)
	}
	if got.ExitIP != old.ExitIP || got.IPChanged != old.IPChanged ||
		got.Anonymity != old.Anonymity || got.Country != old.Country ||
		got.City != old.City || got.Continent != old.Continent {
		t.Fatalf("partial exit probe erased trusted metadata: %+v", got)
	}

	observed := fresh
	observed.ExitIP = "203.0.113.44"
	observed.IPChanged = false // false is meaningful when ExitIP is present.
	observed.IPChangeKnown = true
	observed.Anonymity = "anonymous"
	observed.Country = "DE"
	observed.City = ""
	observed.Continent = "EU"
	p.Update([]Proxy{observed}, nil)
	got, _ = p.Find(old.Key())
	if got.ExitIP != observed.ExitIP || got.IPChanged || !got.IPChangeKnown || got.Anonymity != "anonymous" ||
		got.Country != "DE" || got.City != "Tokyo" || got.Continent != "EU" {
		t.Fatalf("non-empty fresh metadata was not merged field-by-field: %+v", got)
	}
	if got.SpeedKbps != old.SpeedKbps || got.SpeedTestedAt != old.SpeedTestedAt ||
		got.SpeedBytes != old.SpeedBytes || got.SpeedDurationMs != old.SpeedDurationMs {
		t.Fatalf("later refresh replaced speed sample: %+v", got)
	}
}

func TestUpdateKeepsProtocolsAtSameAddressIndependent(t *testing.T) {
	p := NewProxyPool()
	httpProxy := testProxy("http", "192.0.2.25", "8080", true)
	httpProxy.Country = "US"
	socksProxy := testProxy("socks5", httpProxy.IP, httpProxy.Port, true)
	socksProxy.Country = "JP"
	p.Prime([]Proxy{httpProxy, socksProxy}, nil)

	freshHTTP := httpProxy
	freshHTTP.Country = "CA"
	freshHTTP.LatencyMs = 75
	for attempt := 1; attempt <= healthFailureThreshold; attempt++ {
		p.Update([]Proxy{freshHTTP}, map[string]bool{socksProxy.Key(): true})
		if attempt < healthFailureThreshold {
			gotSOCKS, ok := p.Find(socksProxy.Key())
			if !ok || !gotSOCKS.Available {
				t.Fatalf("SOCKS variant became unavailable after refresh failure %d/%d: %+v, ok=%v", attempt, healthFailureThreshold, gotSOCKS, ok)
			}
		}
	}

	if p.Size() != 2 {
		t.Fatalf("same-address protocol variants collapsed; size=%d", p.Size())
	}
	gotHTTP, ok := p.Find(httpProxy.Key())
	if !ok || !gotHTTP.Available || gotHTTP.Country != "CA" || gotHTTP.LatencyMs != 75 {
		t.Fatalf("HTTP variant was not independently refreshed: %+v, ok=%v", gotHTTP, ok)
	}
	gotSOCKS, ok := p.Find(socksProxy.Key())
	if !ok || gotSOCKS.Available || gotSOCKS.Country != "JP" {
		t.Fatalf("SOCKS failure affected the wrong variant or lost its data: %+v, ok=%v", gotSOCKS, ok)
	}
	if got := p.stats[socksProxy.Key()].ConsecutiveHealthFailures; got != healthFailureThreshold {
		t.Fatalf("SOCKS refresh failure streak = %d, want %d", got, healthFailureThreshold)
	}
}

func TestUpdateRefreshSuccessResetsStreakAndFailedNewCandidateStaysOut(t *testing.T) {
	p := NewProxyPool()
	known := testProxy("http", "192.0.2.26", "8080", true)
	p.Prime([]Proxy{known}, nil)

	p.Update(nil, map[string]bool{known.Key(): true})
	p.Update(nil, map[string]bool{known.Key(): true})
	if got, _ := p.Find(known.Key()); !got.Available {
		t.Fatal("known node became unavailable before refresh threshold")
	}
	if got := p.stats[known.Key()].ConsecutiveHealthFailures; got != 2 {
		t.Fatalf("pre-recovery refresh streak = %d, want 2", got)
	}

	fresh := known
	fresh.Available = false // Update owns the successful observation state.
	fresh.LatencyMs = 42
	p.Update([]Proxy{fresh}, nil)
	got, _ := p.Find(known.Key())
	if !got.Available || got.LatencyMs != 42 || p.stats[known.Key()].ConsecutiveHealthFailures != 0 {
		t.Fatalf("successful refresh did not revive/reset node: proxy=%+v stats=%+v", got, p.stats[known.Key()])
	}
	if p.stats[known.Key()].Successes != 0 || p.stats[known.Key()].Failures != 0 {
		t.Fatalf("refresh unexpectedly changed score counters: %+v", p.stats[known.Key()])
	}

	unknown := testProxy("socks5", "192.0.2.27", "1080", false)
	p.Update(nil, map[string]bool{known.Key(): true, unknown.Key(): true})
	got, _ = p.Find(known.Key())
	if !got.Available || p.stats[known.Key()].ConsecutiveHealthFailures != 1 {
		t.Fatalf("new refresh streak after success = proxy=%+v stats=%+v", got, p.stats[known.Key()])
	}
	if _, ok := p.Find(unknown.Key()); ok {
		t.Fatal("failed new candidate was admitted to pool")
	}
	if _, ok := p.stats[unknown.Key()]; ok {
		t.Fatal("failed new candidate created orphan health stats")
	}
}

func TestUpdateKeepsProxyIPInventoryAcrossPartialOrEmptyRefreshes(t *testing.T) {
	p := NewProxyPool()
	old := testProxy("proxyip", "192.0.2.60", "443", false)
	old.Country = "US"
	p.Prime(nil, []Proxy{old})

	fresh := testProxy("proxyip", "192.0.2.61", "443", false)
	fresh.Country = "JP"
	p.Update([]Proxy{fresh}, nil)
	got := p.ProxyIPNodes()
	if len(got) != 2 {
		t.Fatalf("ProxyIP inventory after partial refresh = %#v, want old and fresh entries", got)
	}

	p.Update(nil, nil)
	got = p.ProxyIPNodes()
	if len(got) != 2 {
		t.Fatalf("ProxyIP inventory disappeared after empty refresh: %#v", got)
	}
}

func TestRotateStickyAnySkipsUnavailableNodes(t *testing.T) {
	p := NewProxyPool()
	a := testProxy("socks5", "192.0.2.1", "1", true)
	b := testProxy("socks5", "192.0.2.2", "2", false)
	c := testProxy("socks5", "192.0.2.3", "3", true)
	p.Prime([]Proxy{a, b, c}, nil)
	p.groupState[GroupAny] = &groupCursor{stickyKey: a.Key(), lastPicked: a.Key()}

	got, ok := p.RotateSticky(GroupAny)
	if !ok || got.Key() != c.Key() {
		t.Fatalf("rotation selected unavailable node: got=%+v ok=%v", got, ok)
	}
	got, ok = p.RotateSticky(GroupAny)
	if !ok || got.Key() != a.Key() {
		t.Fatalf("available-only rotation did not wrap: got=%+v ok=%v", got, ok)
	}

	// If the former anchor itself is dead, preserve list-order semantics and
	// choose the next healthy entry after it.
	p.groupState[GroupAny].stickyKey = b.Key()
	got, ok = p.RotateSticky(GroupAny)
	if !ok || got.Key() != c.Key() {
		t.Fatalf("rotation from dead anchor did not find next healthy node: got=%+v ok=%v", got, ok)
	}

	p.SetAvailable(a.Key(), false)
	p.SetAvailable(c.Key(), false)
	got, ok = p.RotateSticky(GroupAny)
	if !ok {
		t.Fatal("all-unavailable fallback unexpectedly failed")
	}
}

func TestUnavailableStickyNodeFallsBackToHealthyPeer(t *testing.T) {
	p := NewProxyPool()
	failed := testProxy("socks5", "192.0.2.21", "1080", true)
	healthy := testProxy("socks5", "192.0.2.22", "1080", true)
	p.Prime([]Proxy{failed, healthy}, nil)
	if !p.ForceSticky(GroupAny, failed.Key()) {
		t.Fatal("failed to pin initial sticky node")
	}

	p.SetAvailable(failed.Key(), false)
	got, ok, direct := p.Pick(GroupAny, nil)
	if !ok || direct || got.Key() != healthy.Key() {
		t.Fatalf("Pick after sticky failure = %+v, ok=%v direct=%v; want healthy peer", got, ok, direct)
	}
}

func TestRecheckCandidatesRotatesBoundedKnownPool(t *testing.T) {
	p := NewProxyPool()
	all := []Proxy{
		testProxy("http", "192.0.2.71", "80", true),
		testProxy("http", "192.0.2.72", "80", false),
		testProxy("socks5", "192.0.2.73", "1080", true),
		testProxy("http", "192.0.2.74", "80", true),
		testProxy("socks5", "192.0.2.75", "1080", false),
	}
	p.Prime(all, nil)

	first := p.RecheckCandidates(2)
	second := p.RecheckCandidates(2)
	third := p.RecheckCandidates(2)
	if len(first) != 2 || len(second) != 2 || len(third) != 2 {
		t.Fatalf("bounded recheck sizes = %d/%d/%d, want 2", len(first), len(second), len(third))
	}
	seen := make(map[string]bool)
	for _, batch := range [][]Proxy{first, second, third} {
		for _, px := range batch {
			seen[px.Key()] = true
		}
	}
	for _, px := range all {
		if !seen[px.Key()] {
			t.Errorf("known node %q was starved by rotating recheck", px.Key())
		}
	}
	if second[0].Key() == first[0].Key() {
		t.Fatalf("second bounded recheck restarted at the first node: first=%#v second=%#v", first, second)
	}
}

func TestResolveGroupMatchesAnyMergedSourceName(t *testing.T) {
	merged := testProxy("socks5", "192.0.2.30", "1080", true)
	merged.SourceName = "alpha"
	merged.SourceNames = []string{"alpha", "beta"}
	other := testProxy("http", "192.0.2.31", "8080", true)
	other.SourceName = "gamma"
	other.SourceNames = []string{"gamma"}
	groups := []Group{{
		ID: "by-source", Name: "secondary source", Strategy: StrategyLatency,
		Sources: []string{"BETA"},
	}}

	got, strategy := resolveGroupCandidates([]Proxy{merged, other}, "by-source", groups)
	if strategy != StrategyLatency {
		t.Fatalf("strategy=%q, want %q", strategy, StrategyLatency)
	}
	if len(got) != 1 || got[0].Key() != merged.Key() {
		t.Fatalf("secondary source attribution did not match group: %+v", got)
	}
}

func TestResolveGroupNodesPrefersProtocolAwareKeysWithLegacyAddressFallback(t *testing.T) {
	httpProxy := testProxy("http", "192.0.2.35", "8080", true)
	socksProxy := testProxy("socks5", "192.0.2.35", "8080", true)
	all := []Proxy{httpProxy, socksProxy}

	exact, _ := resolveGroupCandidates(all, "exact", []Group{{
		ID: "exact", Name: "exact", Nodes: []string{socksProxy.Key()},
	}})
	if len(exact) != 1 || exact[0].Key() != socksProxy.Key() {
		t.Fatalf("protocol-aware Nodes match = %#v, want only SOCKS variant", exact)
	}

	legacy, _ := resolveGroupCandidates(all, "legacy", []Group{{
		ID: "legacy", Name: "legacy", Nodes: []string{httpProxy.Addr()},
	}})
	if len(legacy) != 2 {
		t.Fatalf("legacy address Nodes match = %#v, want both protocol variants", legacy)
	}
}

func TestClearUnavailablePrunesOrphanStatsAndCursors(t *testing.T) {
	p := NewProxyPool()
	live := testProxy("http", "192.0.2.40", "80", true)
	dead := testProxy("socks5", "192.0.2.41", "1080", false)
	p.Prime([]Proxy{live, dead}, nil)
	p.stats[live.Key()] = &nodeStats{Successes: 3}
	p.stats[dead.Key()] = &nodeStats{Failures: 4}
	p.stats["http://orphan.invalid:1"] = &nodeStats{Failures: 9}
	p.groupState["dead-only"] = &groupCursor{
		stickyKey: dead.Key(), lastPicked: dead.Key(), pinned: true,
	}
	p.groupState["mixed"] = &groupCursor{
		stickyKey: live.Key(), lastPicked: dead.Key(), pinned: true,
	}
	p.groupState["live"] = &groupCursor{
		stickyKey: live.Key(), lastPicked: live.Key(), pinned: true,
	}
	p.groupState["nil"] = nil

	if removed := p.ClearUnavailable(); removed != 1 {
		t.Fatalf("removed=%d, want 1", removed)
	}
	if p.Size() != 1 {
		t.Fatalf("size=%d, want 1", p.Size())
	}
	if _, ok := p.stats[dead.Key()]; ok {
		t.Fatal("stats for removed node survived")
	}
	if _, ok := p.stats["http://orphan.invalid:1"]; ok {
		t.Fatal("pre-existing orphan stats survived")
	}
	if got := p.stats[live.Key()]; got == nil || got.Successes != 3 {
		t.Fatalf("live stats were removed: %+v", got)
	}
	if _, ok := p.groupState["dead-only"]; ok {
		t.Fatal("cursor anchored only to removed node survived")
	}
	if _, ok := p.groupState["nil"]; ok {
		t.Fatal("nil cursor survived cleanup")
	}
	if got := p.groupState["mixed"]; got == nil || got.stickyKey != live.Key() || got.lastPicked != "" || !got.pinned {
		t.Fatalf("mixed cursor was not repaired safely: %+v", got)
	}
	if got := p.groupState["live"]; got == nil || got.stickyKey != live.Key() || got.lastPicked != live.Key() {
		t.Fatalf("live cursor was altered: %+v", got)
	}
}

func TestImportantMutationsArePersistedInBatch(t *testing.T) {
	dir := t.TempDir()
	cache := newPoolCache(dir)
	p := NewProxyPool()
	p.persistDebounce = 5 * time.Millisecond
	px := testProxy("socks5", "192.0.2.50", "1080", true)
	p.Prime([]Proxy{px}, nil)
	p.SetCache(cache)

	p.RecordResult(px.Key(), true, 87)
	p.ObserveHealthResult(px.Key(), false, 0)
	p.ObserveHealthResult(px.Key(), false, 0)
	p.SetAvailable(px.Key(), false)
	p.UpdateLatency(px.Key(), 91)
	if !p.UpdateGeo(px.Key(), "203.0.113.50", "SG", "Singapore", "AS", true, true) {
		t.Fatal("UpdateGeo did not find node")
	}
	before := time.Now().Unix()
	if !p.UpdateSpeed(px.Key(), 2500.5, 3_000_000, 9600) {
		t.Fatal("UpdateSpeed did not find node")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		forwarding, _, stats := cache.load()
		if len(forwarding) == 1 {
			got := forwarding[0]
			st := stats[px.Key()]
			if !got.Available && got.LatencyMs == 91 && got.ExitIP == "203.0.113.50" &&
				got.Country == "SG" && got.IPChanged && got.IPChangeKnown && got.SpeedKbps == 2500.5 &&
				got.SpeedBytes == 3_000_000 && got.SpeedDurationMs == 9600 &&
				got.SpeedTestedAt >= before && st.Successes == 1 && st.Failures == 2 && st.LastLatencyMs == 87 &&
				st.ConsecutiveHealthFailures == 2 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for debounced cache persistence")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
