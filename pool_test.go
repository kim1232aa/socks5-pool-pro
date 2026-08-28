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

	removed, _ := p.ClearUnavailable()
	if removed != 2 {
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

func TestHealthOutcomeTerminalFailureAndManualRecovery(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "192.0.2.230", "1080", true)
	p.Prime([]Proxy{px}, nil)

	if !p.EligibleForAutoRecheck(px) {
		t.Fatal("new node was not eligible for its first automatic health check")
	}
	if !p.ObserveHealthResult(px.Key(), false, 103) {
		t.Fatal("completed health failure did not find node")
	}
	got, _ := p.Find(px.Key())
	st := p.stats[px.Key()]
	if got.Available || !got.HealthInvalidated || !st.HealthFailureTerminal || st.ConsecutiveHealthFailures != 1 {
		t.Fatalf("terminal failure state = proxy=%+v stats=%+v", got, st)
	}
	if p.EligibleForAutoRecheck(got) {
		t.Fatal("terminal failure remained eligible for automatic recheck")
	}
	if _, ok, _ := p.Pick(GroupAny, nil); ok {
		t.Fatal("terminal failure remained routable through all-unavailable fallback")
	}
	if p.ObserveHealthResult(px.Key(), true, 47) {
		t.Fatal("automatic success bypassed terminal admission guard")
	}

	generation := p.HealthGeneration()
	if !p.ObserveManualHealthOutcomeAtGeneration(px.Key(), true, true, 47, generation) {
		t.Fatal("successful manual observation did not recover terminal node")
	}
	got, _ = p.Find(px.Key())
	st = p.stats[px.Key()]
	if !got.Available || got.HealthInvalidated || st.HealthFailureTerminal || st.LastHealthSuccessAt.IsZero() || p.EligibleForAutoRecheck(got) {
		t.Fatalf("manual recovery state = proxy=%+v stats=%+v", got, st)
	}
	if st.Successes != 0 || st.Failures != 0 {
		t.Fatalf("health observation changed forwarding counters: %+v", st)
	}

	if !p.ObserveManualHealthOutcomeAtGeneration(px.Key(), false, true, 0, generation) {
		t.Fatal("manual final failure was not applied")
	}
	p.RecordResult(px.Key(), true, 33)
	got, _ = p.Find(px.Key())
	st = p.stats[px.Key()]
	if !st.HealthFailureTerminal || !got.HealthInvalidated || got.Available || st.Successes != 1 {
		t.Fatalf("forwarding success cleared terminal health state: proxy=%+v stats=%+v", got, st)
	}
}

func TestFullRecheckCandidatesIncludeTerminalUnavailableAndSkipRetired(t *testing.T) {
	p := NewProxyPool()
	healthy := testProxy("http", "192.0.2.240", "8080", true)
	terminal := testProxy("socks5", "192.0.2.241", "1080", false)
	terminal.HealthInvalidated = true
	retired := testProxy("socks5", "192.0.2.242", "1080", false)
	retired.SourceRetired = true
	p.Prime([]Proxy{healthy, terminal, retired}, nil)
	p.stats[terminal.Key()] = &nodeStats{HealthFailureTerminal: true, ConsecutiveHealthFailures: 1}

	got := p.FullRecheckCandidates()
	keys := make(map[string]bool, len(got))
	for _, px := range got {
		keys[px.Key()] = true
	}
	if !keys[healthy.Key()] || !keys[terminal.Key()] || keys[retired.Key()] {
		t.Fatalf("full recheck candidates = %v; want healthy+terminal, not retired", keys)
	}
}

func TestFullHealthOutcomeRecoversTerminalNodeAndResetsFailures(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "192.0.2.243", "1080", false)
	px.HealthInvalidated = true
	p.Prime([]Proxy{px}, nil)
	p.stats[px.Key()] = &nodeStats{
		HealthFailureTerminal:          true,
		ConsecutiveHealthFailures:      2,
		ConsecutiveFullRecheckFailures: 2,
	}

	observed, removed := p.ObserveFullHealthOutcomeAtGeneration(px.Key(), true, true, 47, p.HealthGeneration())
	if !observed || removed {
		t.Fatalf("full success = observed %v removed %v", observed, removed)
	}
	got, ok := p.Find(px.Key())
	stats := p.stats[px.Key()]
	if !ok || !got.Available || got.HealthInvalidated || got.PolicyExcluded || got.LatencyMs != 47 {
		t.Fatalf("recovered proxy = %+v found=%v", got, ok)
	}
	if stats.HealthFailureTerminal || stats.ConsecutiveHealthFailures != 0 || stats.ConsecutiveFullRecheckFailures != 0 || stats.LastHealthSuccessAt.IsZero() {
		t.Fatalf("recovered stats = %+v", stats)
	}
}

func TestFullHealthOutcomeThirdConsecutiveFailureRemovesPoolStateOnly(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "192.0.2.244", "1080", true)
	px.SourceName = "source-a"
	px.SourceIDs = []string{"source-a"}
	p.Prime([]Proxy{px}, nil)
	refresh := p.candidates.begin([]Proxy{px}, map[string]string{"source-a": "Source A"}, nil, 0)
	p.candidates.complete(refresh, []Proxy{px}, []Proxy{px}, nil)
	generation := p.HealthGeneration()

	for failure := 1; failure <= 2; failure++ {
		observed, removed := p.ObserveFullHealthOutcomeAtGeneration(px.Key(), false, true, 0, generation)
		if !observed || removed {
			t.Fatalf("full failure %d = observed %v removed %v", failure, observed, removed)
		}
		if _, ok := p.Find(px.Key()); !ok {
			t.Fatalf("node removed after full failure %d", failure)
		}
		if got := p.stats[px.Key()].ConsecutiveFullRecheckFailures; got != failure {
			t.Fatalf("full failure streak = %d, want %d", got, failure)
		}
	}

	observed, removed := p.ObserveFullHealthOutcomeAtGeneration(px.Key(), false, true, 0, generation)
	if !observed || !removed {
		t.Fatalf("third full failure = observed %v removed %v", observed, removed)
	}
	if _, ok := p.Find(px.Key()); ok {
		t.Fatal("third full failure retained forwarding node")
	}
	if _, ok := p.stats[px.Key()]; ok {
		t.Fatal("third full failure retained node stats")
	}
	if _, ok := p.candidates.FindByKey(px.Key()); !ok {
		t.Fatal("forwarding removal deleted candidate-catalog membership")
	}
	assertProxyIndexInvariant(t, p)
}

func TestFullHealthOutcomeSuccessResetsRemovalStreakAndStaleGenerationDoesNotCount(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("http", "192.0.2.245", "8080", true)
	p.Prime([]Proxy{px}, nil)
	generation := p.HealthGeneration()

	for i := 0; i < 2; i++ {
		if observed, removed := p.ObserveFullHealthOutcomeAtGeneration(px.Key(), false, true, 0, generation); !observed || removed {
			t.Fatalf("initial full failure %d = observed %v removed %v", i+1, observed, removed)
		}
	}
	if observed, removed := p.ObserveFullHealthOutcomeAtGeneration(px.Key(), true, true, 25, generation); !observed || removed {
		t.Fatalf("resetting full success = observed %v removed %v", observed, removed)
	}
	if observed, removed := p.ObserveFullHealthOutcomeAtGeneration(px.Key(), false, true, 0, generation); !observed || removed {
		t.Fatalf("post-success full failure = observed %v removed %v", observed, removed)
	}
	if got := p.stats[px.Key()].ConsecutiveFullRecheckFailures; got != 1 {
		t.Fatalf("post-success full failure streak = %d, want 1", got)
	}

	p.InvalidateHealth("https://example.test/new-health")
	if observed, removed := p.ObserveFullHealthOutcomeAtGeneration(px.Key(), false, true, 0, generation); observed || removed {
		t.Fatalf("stale full outcome = observed %v removed %v", observed, removed)
	}
	if got := p.stats[px.Key()].ConsecutiveFullRecheckFailures; got != 0 {
		t.Fatalf("stale generation changed reset streak to %d", got)
	}
	if _, ok := p.Find(px.Key()); !ok {
		t.Fatal("stale generation removed node")
	}
}

func TestCommitFullHealthRecheckBatchesThirdFailureRemovals(t *testing.T) {
	p := NewProxyPool()
	nodes := []Proxy{
		testProxy("socks5", "8.8.8.101", "1080", true),
		testProxy("http", "8.8.4.101", "8080", true),
	}
	for i := range nodes {
		nodes[i].SourceName = "source-a"
		nodes[i].SourceIDs = []string{"source-a"}
	}
	p.Prime(nodes, nil)
	refresh := p.candidates.begin(nodes, map[string]string{"source-a": "Source A"}, nil, 0)
	p.candidates.complete(refresh, nodes, nodes, nil)
	for _, px := range nodes {
		p.stats[px.Key()] = &nodeStats{
			HealthFailureTerminal:          true,
			ConsecutiveHealthFailures:      2,
			ConsecutiveFullRecheckFailures: 2,
		}
	}
	p.SetCache(newPoolCache(t.TempDir()))
	var persistCalls int
	p.beforeDeletePersist = func() { persistCalls++ }

	observations := make([]fullHealthObservation, 0, len(nodes))
	for _, px := range nodes {
		observations = append(observations, fullHealthObservation{Key: px.Key(), Reachable: false, PolicyAllowed: true})
	}
	result, err := p.CommitFullHealthRecheckAtGeneration(observations, p.HealthGeneration())
	if err != nil {
		t.Fatalf("CommitFullHealthRecheckAtGeneration: %v", err)
	}
	if !result.Applied {
		t.Fatal("completed full recheck was not applied")
	}
	if persistCalls != 1 {
		t.Fatalf("full recheck persisted %d delete plans, want one batch", persistCalls)
	}
	if got := len(result.RemovedKeys); got != len(nodes) {
		t.Fatalf("removed keys = %v, want both nodes", result.RemovedKeys)
	}
	if p.Size() != 0 {
		t.Fatalf("third failures left %d forwarding nodes", p.Size())
	}
	for _, px := range nodes {
		if _, ok := p.stats[px.Key()]; ok {
			t.Fatalf("third failure retained stats for %s", px.Key())
		}
		if _, ok := p.candidates.FindByKey(px.Key()); !ok {
			t.Fatalf("third failure deleted candidate catalog entry %s", px.Key())
		}
	}
	assertProxyIndexInvariant(t, p)
}

func TestCommitFullHealthRecheckWithNoMatchingObservationsIsNoOp(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "8.8.8.102", "1080", true)
	p.Prime([]Proxy{px}, nil)
	p.SetCache(newPoolCache(t.TempDir()))
	var persistCalls int
	p.beforeDeletePersist = func() { persistCalls++ }

	result, err := p.CommitFullHealthRecheckAtGeneration([]fullHealthObservation{{
		Key: "socks5://203.0.113.99:1080", Reachable: false, PolicyAllowed: true,
	}}, p.HealthGeneration())
	if err != nil {
		t.Fatalf("CommitFullHealthRecheckAtGeneration: %v", err)
	}
	if !result.Applied || result.Observed != 0 || len(result.RemovedKeys) != 0 {
		t.Fatalf("no-match result = %+v, want applied no-op", result)
	}
	if persistCalls != 0 {
		t.Fatalf("no-match full recheck persisted %d times, want zero", persistCalls)
	}
	if got, ok := p.Find(px.Key()); !ok || got.Key() != px.Key() {
		t.Fatalf("no-match full recheck changed live pool: %+v found=%v", got, ok)
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
		got.Anonymity != "" || got.Country != old.Country ||
		got.City != old.City || got.Continent != old.Continent {
		t.Fatalf("successful health check did not clear stale unknown anonymity: %+v", got)
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
	p.Update([]Proxy{freshHTTP}, map[string]bool{socksProxy.Key(): true})

	if p.Size() != 1 {
		t.Fatalf("failed SOCKS variant was not dropped independently; size=%d", p.Size())
	}
	gotHTTP, ok := p.Find(httpProxy.Key())
	if !ok || !gotHTTP.Available || gotHTTP.Country != "CA" || gotHTTP.LatencyMs != 75 {
		t.Fatalf("HTTP variant was not independently refreshed: %+v, ok=%v", gotHTTP, ok)
	}
	if p.EligibleForAutoRecheck(gotHTTP) {
		t.Fatal("successful HTTP variant did not enter cooldown")
	}
	if _, ok := p.Find(socksProxy.Key()); ok {
		t.Fatal("failed SOCKS variant remained in forwarding pool")
	}
	if _, ok := p.stats[socksProxy.Key()]; ok {
		t.Fatal("failed SOCKS variant left terminal stats")
	}
}

func TestUpdateTerminalFailureRequiresManualRecoveryAndFailedNewCandidateStaysOut(t *testing.T) {
	p := NewProxyPool()
	known := testProxy("http", "192.0.2.26", "8080", true)
	p.Prime([]Proxy{known}, nil)

	p.Update(nil, map[string]bool{known.Key(): true})
	if _, ok := p.Find(known.Key()); ok {
		t.Fatal("unreachable known node remained in forwarding pool")
	}
	if _, ok := p.stats[known.Key()]; ok {
		t.Fatal("removed unreachable node left terminal stats")
	}

	unknown := testProxy("socks5", "192.0.2.27", "1080", false)
	p.Update(nil, map[string]bool{unknown.Key(): true})
	if _, ok := p.Find(unknown.Key()); ok {
		t.Fatal("failed new candidate was admitted to pool")
	}
	if _, ok := p.stats[unknown.Key()]; ok {
		t.Fatal("failed new candidate created orphan health stats")
	}
}

func TestUpdateDropsUnreachableNodesAndDoesNotAdmitEmptySourceFailures(t *testing.T) {
	p := NewProxyPool()
	source := Source{ID: "builtin-fyvri-http", Name: "Fyvri HTTP", Enabled: true}
	deadA := testProxy("http", "8.8.8.187", "80", true)
	deadA.SourceIDs = []string{source.ID}
	deadA.SourceNames = []string{source.Name}
	deadB := testProxy("http", "8.8.8.188", "80", true)
	deadB.SourceIDs = []string{source.ID}
	deadB.SourceNames = []string{source.Name}
	keep := testProxy("http", "8.8.8.20", "8080", true)
	keep.SourceIDs = []string{"other"}
	keep.SourceNames = []string{"Other"}
	other := Source{ID: "other", Name: "Other", Enabled: true}
	p.Prime([]Proxy{deadA, keep}, nil)
	p.groupState[GroupAny] = &groupCursor{stickyKey: deadA.Key(), lastPicked: deadA.Key(), pinned: true}

	if !p.UpdateWithEnabledSourcesAndPolicy(nil, map[string]bool{deadA.Key(): true, deadB.Key(): true}, nil, []Source{source, other}, p.HealthGeneration()) {
		t.Fatal("current-generation empty-source failure was rejected")
	}
	if _, ok := p.Find(deadA.Key()); ok {
		t.Fatal("previously pooled unreachable node remained in forwarding pool")
	}
	if _, ok := p.stats[deadA.Key()]; ok {
		t.Fatal("removed unreachable node left terminal stats that would block later revival")
	}
	if _, ok := p.Find(deadB.Key()); ok {
		t.Fatal("never-alive failed candidate from empty source was admitted")
	}
	got, ok := p.Find(keep.Key())
	if !ok || !got.Available {
		t.Fatalf("unrelated healthy node = %+v ok=%v", got, ok)
	}
	if cursor := p.groupState[GroupAny]; cursor != nil && (cursor.stickyKey == deadA.Key() || cursor.lastPicked == deadA.Key()) {
		t.Fatalf("sticky cursor still points at removed node: %+v", cursor)
	}

	revived := deadA
	revived.Available = true
	revived.LatencyMs = 41
	if !p.Update([]Proxy{revived}, nil) {
		t.Fatal("later successful observation could not re-admit dropped node")
	}
	got, ok = p.Find(deadA.Key())
	if !ok || !got.Available || got.LatencyMs != 41 {
		t.Fatalf("revived node = %+v ok=%v", got, ok)
	}
	assertProxyIndexInvariant(t, p)
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

func TestForceStickyRejectsCurrentlyUnavailableTarget(t *testing.T) {
	p := NewProxyPool()
	unavailable := testProxy("socks5", "192.0.2.23", "1080", false)
	healthy := testProxy("socks5", "192.0.2.24", "1080", true)
	p.Prime([]Proxy{unavailable, healthy}, nil)

	if result := p.forceSticky(GroupAny, unavailable.Key()); result != forceStickyUnavailable {
		t.Fatalf("forceSticky(unavailable) = %v, want forceStickyUnavailable", result)
	}
	if p.IsPinned(GroupAny) {
		t.Fatal("rejected unavailable node left ANY falsely pinned")
	}
	if p.ForceSticky(GroupAny, unavailable.Key()) {
		t.Fatal("ForceSticky reported success for an unavailable node")
	}
	if !p.ForceSticky(GroupAny, healthy.Key()) || !p.IsPinned(GroupAny) {
		t.Fatal("healthy target no longer supports explicit sticky selection")
	}
}

func TestCurrentGenerationFailureClearsWaitingForRecheckWithoutReviving(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "192.0.2.27", "1080", true)
	px.PolicyExcluded = true // stale policy annotation from the former criterion
	p.Prime([]Proxy{px}, nil)
	p.SetHealthCriterion(defaultCheckURL)
	p.InvalidateHealth("https://example.com/new-health")
	generation := p.HealthGeneration()

	if !p.ObserveHealthOutcomeAtGeneration(px.Key(), false, true, 0, generation) {
		t.Fatal("current-generation failure was not observed")
	}
	got, ok := p.Find(px.Key())
	st := p.stats[px.Key()]
	if !ok || got.Available || !got.HealthInvalidated || got.PolicyExcluded || !st.HealthFailureTerminal {
		t.Fatalf("failed current-generation result = proxy=%+v stats=%+v found=%v; want terminal unavailable", got, st, ok)
	}
	_, failures := p.StatsOf(px.Key())
	if failures != 0 {
		t.Fatalf("health failure changed forwarding failure count = %d", failures)
	}
}

func TestPromoteCandidateSpeedRejectsDisabledSourceWithoutPublishing(t *testing.T) {
	p := NewProxyPool()
	p.SetHealthCriterion(defaultCheckURL)
	candidate := testProxy("http", "192.0.2.27", "8080", false)
	candidate.SourceIDs = []string{"source-disabled"}
	candidate.SourceNames = []string{"Disabled source"}

	if p.PromoteCandidateSpeed(candidate, []Source{{ID: "source-disabled", Name: "Disabled source", Enabled: false}}, p.HealthGeneration(), 2048, 1<<20, 4000) {
		t.Fatal("disabled-source candidate was promoted")
	}
	if got := p.Size(); got != 0 {
		t.Fatalf("disabled-source candidate partially published: pool size = %d, want 0", got)
	}
}

func TestUpdateWithEnabledSourcesAndPolicyMakesHardExclusionWin(t *testing.T) {
	p := NewProxyPool()
	p.SetHealthCriterion(defaultCheckURL)
	source := Source{ID: "source-a", Name: "Source A", Enabled: true}
	filtered := testProxy("socks5", "192.0.2.25", "1080", true)
	filtered.SourceIDs = []string{source.ID}
	filtered.SourceNames = []string{source.Name}
	healthy := testProxy("socks5", "192.0.2.26", "1080", true)
	healthy.SourceIDs = []string{source.ID}
	healthy.SourceNames = []string{source.Name}
	p.Prime([]Proxy{filtered, healthy}, nil)

	// Supplying the filtered key in freshlyAlive is deliberately defensive: even
	// under inconsistent inputs, the hard policy result must win in the one
	// published pool generation rather than briefly reviving the node.
	if !p.UpdateWithEnabledSourcesAndPolicy(
		[]Proxy{filtered, healthy}, nil, map[string]bool{filtered.Key(): true},
		[]Source{source}, p.HealthGeneration(),
	) {
		t.Fatal("current-generation refresh was rejected")
	}
	got, ok := p.Find(filtered.Key())
	if !ok || got.Available || !got.PolicyExcluded || got.HealthInvalidated {
		t.Fatalf("policy-filtered retained node = %+v, ok=%v", got, ok)
	}
	selected, ok, direct := p.Pick(GroupAny, nil)
	if !ok || direct || selected.Key() != healthy.Key() {
		t.Fatalf("selection after atomic policy publication = %+v, ok=%v direct=%v; want healthy peer", selected, ok, direct)
	}
	views := apiPoolProxiesFrom(p.All())
	if len(views) != 1 || views[0].ProxyURL != healthy.ConsumerURL() || views[0].Username != healthy.Username || views[0].Password != healthy.Password {
		t.Fatalf("healthy compatibility views after atomic policy publication = %#v", views)
	}
}

func TestRecheckCandidatesSkipsSuccessCooldownAndTerminalFailure(t *testing.T) {
	p := NewProxyPool()
	newNode := testProxy("http", "192.0.2.81", "80", false)
	cooled := testProxy("http", "192.0.2.82", "80", true)
	expired := testProxy("socks5", "192.0.2.83", "1080", true)
	terminal := testProxy("socks5", "192.0.2.84", "1080", false)
	p.Prime([]Proxy{newNode, cooled, expired, terminal}, nil)
	now := time.Now().UTC()
	p.stats[cooled.Key()] = &nodeStats{LastHealthSuccessAt: now.Add(-time.Hour)}
	p.stats[expired.Key()] = &nodeStats{LastHealthSuccessAt: now.Add(-automaticHealthSuccessCooldown - time.Minute)}
	p.stats[terminal.Key()] = &nodeStats{HealthFailureTerminal: true}

	got := p.RecheckCandidates(10)
	keys := make(map[string]bool, len(got))
	for _, px := range got {
		keys[px.Key()] = true
	}
	if !keys[newNode.Key()] || !keys[expired.Key()] || keys[cooled.Key()] || keys[terminal.Key()] {
		t.Fatalf("eligible candidates = %v; want new+expired only", keys)
	}
	refreshInput := []Proxy{newNode, cooled, expired, terminal}
	refreshEligible := p.FilterAutoRecheckCandidates(refreshInput)
	refreshKeys := make(map[string]bool, len(refreshEligible))
	for _, px := range refreshEligible {
		refreshKeys[px.Key()] = true
	}
	if !refreshKeys[newNode.Key()] || !refreshKeys[expired.Key()] || refreshKeys[cooled.Key()] || refreshKeys[terminal.Key()] {
		t.Fatalf("refresh-eligible candidates = %v; want new+expired only", refreshKeys)
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

func TestInvalidateHealthRetainsNodesAndAllowsImmediateRecovery(t *testing.T) {
	p := NewProxyPool()
	a := Proxy{Protocol: "socks5", IP: "192.0.2.10", Port: "1080", Available: true}
	b := Proxy{Protocol: "http", IP: "192.0.2.20", Port: "8080", Available: true}
	p.Update([]Proxy{a, b}, nil)

	if changed := p.InvalidateHealth(); changed != 2 {
		t.Fatalf("InvalidateHealth() changed %d nodes, want 2", changed)
	}
	if got := p.Size(); got != 2 {
		t.Fatalf("pool size after invalidation = %d, want 2", got)
	}
	for _, px := range p.All() {
		if px.Available {
			t.Fatalf("node %s remained available after invalidation", px.Key())
		}
	}
	if got, ok, _ := p.Pick(GroupAny, nil); ok {
		t.Fatalf("criterion-invalid node remained routable through fallback: %+v", got)
	}

	if !p.ObserveHealthResult(a.Key(), true, 12) {
		t.Fatal("ObserveHealthResult() did not find retained node")
	}
	recovered, ok := p.Find(a.Key())
	if !ok || !recovered.Available {
		t.Fatalf("retained node did not recover: ok=%v proxy=%+v", ok, recovered)
	}
}

func TestHealthGenerationRejectsStaleAsynchronousResults(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "192.0.2.19", "1080", true)
	px.ExitIP = "198.51.100.19"
	px.Country = "US"
	px.City = "Seattle"
	px.Continent = "NA"
	p.Prime([]Proxy{px}, nil)
	oldGeneration := p.HealthGeneration()
	p.InvalidateHealth()

	if p.ObserveHealthResultAtGeneration(px.Key(), true, 8, oldGeneration) {
		t.Fatal("stale health observation was applied")
	}
	if p.UpdateGeoAtGeneration(px.Key(), "203.0.113.19", "JP", "Tokyo", "AS", true, true, oldGeneration) {
		t.Fatal("stale geo observation was applied")
	}
	if got, _ := p.Find(px.Key()); got.Available {
		t.Fatal("stale health observation restored availability")
	} else if got.ExitIP != px.ExitIP || got.Country != px.Country || got.City != px.City || got.Continent != px.Continent || got.IPChanged || got.IPChangeKnown {
		t.Fatalf("stale geo observation changed node: got %+v want original geo %+v", got, px)
	}
	if p.Update([]Proxy{px}, nil, oldGeneration) {
		t.Fatal("stale refresh update was applied")
	}

	currentGeneration := p.HealthGeneration()
	if !p.ObserveHealthResultAtGeneration(px.Key(), true, 8, currentGeneration) {
		t.Fatal("current health observation was not applied")
	}
	if !p.UpdateGeoAtGeneration(px.Key(), "203.0.113.19", "JP", "Tokyo", "AS", true, true, currentGeneration) {
		t.Fatal("current geo observation was not applied")
	}
	if got, _ := p.Find(px.Key()); !got.Available {
		t.Fatal("current health observation did not restore availability")
	} else if got.ExitIP != "203.0.113.19" || got.Country != "JP" || got.City != "Tokyo" || got.Continent != "AS" || !got.IPChanged || !got.IPChangeKnown {
		t.Fatalf("current geo observation = %+v", got)
	}
}

func TestHealthGenerationRejectsGeoAfterHealthCommit(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "192.0.2.20", "1080", false)
	px.ExitIP = "198.51.100.20"
	px.Country = "US"
	p.Prime([]Proxy{px}, nil)
	generation := p.HealthGeneration()

	if !p.ObserveHealthResultAtGeneration(px.Key(), true, 8, generation) {
		t.Fatal("current health observation was not applied")
	}
	p.InvalidateHealth()
	if p.UpdateGeoAtGeneration(px.Key(), "203.0.113.20", "JP", "Tokyo", "AS", true, true, generation) {
		t.Fatal("geo observation from the completed old-generation health check was applied")
	}
	got, ok := p.Find(px.Key())
	if !ok {
		t.Fatal("node disappeared")
	}
	if got.ExitIP != px.ExitIP || got.Country != px.Country || got.IPChanged || got.IPChangeKnown {
		t.Fatalf("stale geo observation changed node: got %+v want original geo %+v", got, px)
	}
}

func TestInvalidateHealthCancelsActiveGenerationWork(t *testing.T) {
	p := NewProxyPool()
	p.SetHealthCriterion(defaultCheckURL)
	ctx, finish, ok := p.BeginHealthWork(p.HealthGeneration())
	if !ok {
		t.Fatal("failed to start current health work")
	}
	defer finish()
	p.InvalidateHealth("https://example.com/new-health")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("criterion invalidation did not cancel active work")
	}
}

func TestReachablePolicyFilteredObservationStaysUnavailable(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "192.0.2.27", "1080", false)
	p.Prime([]Proxy{px}, nil)
	if !p.ObserveHealthOutcomeAtGeneration(px.Key(), true, false, 11, p.HealthGeneration()) {
		t.Fatal("policy-filtered observation did not find node")
	}
	got, _ := p.Find(px.Key())
	if got.Available || got.LatencyMs != 11 {
		t.Fatalf("policy-filtered node = %+v", got)
	}
	if selected, ok, _ := p.Pick(GroupAny, nil); ok {
		t.Fatalf("policy-excluded node remained routable: %+v", selected)
	}
	successes, failures := p.StatsOf(px.Key())
	if successes != 0 || failures != 0 {
		t.Fatalf("health observation changed forwarding stats = %d/%d", successes, failures)
	}
}

func TestUpdateVerifiedCredentialsPromotesWorkingAlternative(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "192.0.2.29", "1080", true)
	px.Username, px.Password = "old", "wrong"
	px.CredentialAlternates = []ProxyCredential{{Username: "new", Password: "working"}}
	p.Prime([]Proxy{px}, nil)
	verified := px.promoteCredential(Proxy{Username: "new", Password: "working", CredentialAlternates: px.CredentialAlternates})

	if !p.UpdateVerifiedCredentialsAtGeneration(px.Key(), verified, p.HealthGeneration()) {
		t.Fatal("known node credentials were not updated")
	}
	got, _ := p.Find(px.Key())
	if got.Username != "new" || got.Password != "working" {
		t.Fatalf("primary credential = %q/%q, want promoted working pair", got.Username, got.Password)
	}
	if len(got.CredentialAlternates) != 1 || got.CredentialAlternates[0].Username != "old" {
		t.Fatalf("credential alternatives = %#v, want old primary retained", got.CredentialAlternates)
	}
}

func TestApplyEnabledSourcesRetiresOnlyOrphanedProvenance(t *testing.T) {
	p := NewProxyPool()
	onlyDisabled := testProxy("socks5", "192.0.2.31", "1080", true)
	onlyDisabled.SourceName = "private"
	onlyDisabled.SourceNames = []string{"private"}
	onlyDisabled.SourceIDs = []string{"src-private"}
	shared := testProxy("http", "192.0.2.32", "8080", true)
	shared.SourceName = "private"
	shared.SourceNames = []string{"private", "public"}
	shared.SourceIDs = []string{"src-private", "src-public"}
	legacy := testProxy("http", "192.0.2.33", "8080", true)
	legacy.SourceName = "public"
	p.Prime([]Proxy{onlyDisabled, shared, legacy}, nil)

	sources := []Source{
		{ID: "src-private", Name: "private", Enabled: false},
		{ID: "src-public", Name: "public", Enabled: true},
	}
	if retired := p.ApplyEnabledSources(sources); retired != 2 {
		t.Fatalf("ApplyEnabledSources() retired %d nodes, want 2", retired)
	}
	for _, tt := range []struct {
		key       string
		available bool
		retired   bool
	}{
		{onlyDisabled.Key(), false, true},
		// Credentials are endpoint-level, so a node merged from enabled and
		// disabled feeds remains conservatively retired until a new refresh
		// reconstructs it solely from enabled provenance.
		{shared.Key(), false, true},
		{legacy.Key(), true, false},
	} {
		px, ok := p.Find(tt.key)
		if !ok || px.Available != tt.available || px.SourceRetired != tt.retired {
			t.Fatalf("node %s = %+v, ok=%v; want available=%v retired=%v", tt.key, px, ok, tt.available, tt.retired)
		}
	}
	for _, px := range p.RecheckCandidates(10) {
		if px.Key() == onlyDisabled.Key() || px.Key() == shared.Key() {
			t.Fatalf("source-retired node %s was scheduled for background recheck", px.Key())
		}
	}
	if !p.ObserveHealthResult(onlyDisabled.Key(), true, 5) {
		t.Fatal("retired node disappeared instead of being retained")
	}
	if px, _ := p.Find(onlyDisabled.Key()); px.Available {
		t.Fatal("health success bypassed source retirement")
	}
	got, ok, direct := p.Pick(GroupAny, nil)
	if !ok || direct || got.Key() != legacy.Key() {
		t.Fatalf("ANY selection = %+v, ok=%v direct=%v; want active legacy node", got, ok, direct)
	}
}

func TestSourceRetiredNodeIsNeverUsedAsUnavailableFallback(t *testing.T) {
	p := NewProxyPool()
	px := testProxy("socks5", "192.0.2.44", "1080", true)
	px.SourceIDs = []string{"removed-source"}
	px.SourceNames = []string{"removed"}
	p.Prime([]Proxy{px}, nil)
	if retired := p.ApplyEnabledSources(nil); retired != 1 {
		t.Fatalf("retired=%d, want 1", retired)
	}
	if got, ok, direct := p.Pick(GroupAny, nil); ok || direct {
		t.Fatalf("Pick returned retired node %+v, ok=%v direct=%v", got, ok, direct)
	}
	if got, ok := p.RotateSticky(GroupAny); ok {
		t.Fatalf("RotateSticky returned retired node %+v", got)
	}
	if got, ok, _ := p.EffectiveCurrent(GroupAny, nil); ok {
		t.Fatalf("EffectiveCurrent returned retired node %+v", got)
	}
	// Merely re-enabling the source does not prove that the cached credentials
	// are still valid. The node stays retired until a successful refresh installs
	// a fresh declaration/health result.
	p.ApplyEnabledSources([]Source{{ID: "removed-source", Name: "removed", Enabled: true}})
	if got, ok, _ := p.Pick(GroupAny, nil); ok {
		t.Fatalf("re-enabled but unverified source became routable: %+v", got)
	}
	fresh := px
	fresh.SourceRetired = false
	if !p.Update([]Proxy{fresh}, nil, p.HealthGeneration()) {
		t.Fatal("fresh source result was not installed")
	}
	p.ApplyEnabledSources([]Source{{ID: "removed-source", Name: "removed", Enabled: true}})
	if got, ok, direct := p.Pick(GroupAny, nil); !ok || direct || got.Key() != px.Key() {
		t.Fatalf("freshly verified source did not recover: %+v, ok=%v direct=%v", got, ok, direct)
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

func TestResolveGroupIDRemainsCaseSensitive(t *testing.T) {
	jp := testProxy("socks5", "192.0.2.42", "1080", true)
	jp.Country = "JP"
	us := testProxy("socks5", "192.0.2.43", "1080", true)
	us.Country = "US"
	groups := []Group{{ID: "Group-Tokyo-ID", Name: "Tokyo-Egress", Countries: []string{"JP"}}}

	exact, _ := resolveGroupCandidates([]Proxy{jp, us}, "Group-Tokyo-ID", groups)
	if len(exact) != 1 || exact[0].Key() != jp.Key() {
		t.Fatalf("exact group ID match = %#v, want JP node", exact)
	}
	wrongCase, _ := resolveGroupCandidates([]Proxy{jp, us}, "group-tokyo-id", groups)
	if len(wrongCase) != 2 {
		t.Fatalf("case-folded group ID unexpectedly matched: %#v", wrongCase)
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

	removed, _ := p.ClearUnavailable()
	if removed != 1 {
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
	px := testProxy("socks5", "8.8.8.50", "1080", true)
	p.Prime([]Proxy{px}, nil)
	p.SetCache(cache)

	p.RecordResult(px.Key(), true, 87)
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
				got.SpeedTestedAt >= before && st.Successes == 1 && st.Failures == 0 && st.LastLatencyMs == 87 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for debounced cache persistence")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOptimisticPickRevisionTracksRoutingAndScoreInputs(t *testing.T) {
	p := NewProxyPool()
	first := testProxy("socks5", "192.0.2.60", "1080", true)
	first.Country = "JP"
	p.Prime([]Proxy{first}, nil)

	routing := p.routingRevision
	stats := p.statsRevision
	p.RecordResult(first.Key(), true, 12)
	if p.routingRevision != routing {
		t.Fatalf("stats-only observation changed routing revision: got %d want %d", p.routingRevision, routing)
	}
	if p.statsRevision == stats {
		t.Fatal("score revision did not change after RecordResult")
	}

	routing = p.routingRevision
	p.SetAvailable(first.Key(), false)
	if p.routingRevision == routing {
		t.Fatal("availability mutation did not change routing revision")
	}
	routing = p.routingRevision
	if !p.UpdateGeo(first.Key(), "203.0.113.60", "US", "Seattle", "NA", true, true) {
		t.Fatal("UpdateGeo did not find node")
	}
	if p.routingRevision == routing {
		t.Fatal("group-membership metadata mutation did not change routing revision")
	}
	routing = p.routingRevision
	if !p.UpdateSpeed(first.Key(), 900, 3_000_000, 1000) {
		t.Fatal("UpdateSpeed did not find node")
	}
	if p.routingRevision == routing {
		t.Fatal("strategy metric mutation did not change routing revision")
	}
}

func TestOptimisticPickCommitRejectsChangedEligibilityAndMembership(t *testing.T) {
	p := NewProxyPool()
	stale := testProxy("socks5", "192.0.2.70", "1080", false)
	stale.Country = "JP"
	healthy := testProxy("socks5", "192.0.2.71", "1080", false)
	healthy.Country = "JP"
	p.Prime([]Proxy{stale, healthy}, nil)
	selector := newPoolGroupSelector("COUNTRY:JP", nil)

	p.mu.RLock()
	chosen, _, found := p.selectProxyLocked(selector, groupCursor{}, nil)
	revision := p.routingRevision
	p.mu.RUnlock()
	if !found || chosen.Key() != stale.Key() {
		t.Fatalf("initial all-unavailable selection = %+v, found=%v", chosen, found)
	}
	p.SetAvailable(healthy.Key(), true)
	p.mu.Lock()
	if p.routingRevision == revision {
		t.Fatal("concurrent availability change was invisible to optimistic picker")
	}
	if p.proxySelectableAtCommitLocked(chosen, selector, nil) {
		t.Fatal("stale unavailable choice remained committable after a healthy peer appeared")
	}
	p.mu.Unlock()

	p.SetAvailable(stale.Key(), true)
	p.mu.RLock()
	chosen, _, found = p.selectProxyLocked(selector, groupCursor{}, map[string]bool{healthy.Key(): true})
	revision = p.routingRevision
	p.mu.RUnlock()
	if !found || chosen.Key() != stale.Key() {
		t.Fatalf("country selection before geo change = %+v, found=%v", chosen, found)
	}
	if !p.UpdateGeo(stale.Key(), "203.0.113.70", "US", "Portland", "NA", true, true) {
		t.Fatal("UpdateGeo did not find selected node")
	}
	p.mu.Lock()
	if p.routingRevision == revision {
		t.Fatal("concurrent membership change was invisible to optimistic picker")
	}
	index, ok := p.proxyIndexLookupLocked(stale.Key())
	if !ok || p.proxySelectableAtCommitLocked(p.proxies[index], selector, nil) {
		t.Fatal("node that left COUNTRY:JP remained committable")
	}
	p.mu.Unlock()
}

func TestClearUnavailableReportsNonDurableCacheFailure(t *testing.T) {
	dir := t.TempDir()
	p := NewProxyPool()
	p.cache = newPoolCache(dir) // set cache directly for testing

	validProxy := Proxy{Protocol: "socks5", IP: "127.0.0.1", Port: "1080", Available: true}
	p.Update([]Proxy{validProxy}, nil)
	p.SetAvailable(validProxy.Key(), false)
	_ = p.FlushCache()

	if p.Size() != 1 {
		t.Fatalf("size=%d, want 1", p.Size())
	}

	p.cache.path = "" + ""

	removed, err := p.ClearUnavailable()
	if err == nil {
		t.Fatal("expected error from ClearUnavailable when cache fails")
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}

	if p.Size() != 1 {
		t.Fatalf("memory changed despite persistence failure: size=%d", p.Size())
	}

	p.cache.path = dir + "/cache.json.gz"

	removed, err = p.ClearUnavailable()
	if err != nil {
		t.Fatalf("unexpected error after fixing cache: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}

	if p.Size() != 0 {
		t.Fatalf("memory unchanged after successful clear: size=%d", p.Size())
	}
}

func TestRemoveKeysConcurrent(t *testing.T) {
	dir := t.TempDir()
	p := NewProxyPool()
	p.cache = newPoolCache(dir)

	proxies := []Proxy{
		{Protocol: "socks5", IP: "127.0.0.9", Port: "1080", Available: true},
		{Protocol: "socks5", IP: "127.0.0.10", Port: "1080", Available: true},
	}
	p.Update(proxies, nil)

	start := make(chan struct{})
	done := make(chan struct{}, 3)

	go func() {
		<-start
		_, _, _ = p.RemoveKeys([]string{proxies[0].Key()})
		done <- struct{}{}
	}()
	go func() {
		<-start
		_, _ = p.ClearUnavailable()
		done <- struct{}{}
	}()
	go func() {
		<-start
		_ = p.FlushCache()
		done <- struct{}{}
	}()

	close(start)
	<-done
	<-done
	<-done
}

func TestRemoveKeysDoesNotBlockHealthUpdateDuringPersistence(t *testing.T) {
	remove := testProxy("socks5", "8.8.8.90", "1080", true)
	keep := testProxy("http", "8.8.4.90", "8080", true)
	p := NewProxyPool()
	p.Prime([]Proxy{remove, keep}, nil)
	cache := newPoolCache(t.TempDir())
	p.SetCache(cache)

	persistStarted := make(chan struct{})
	releasePersist := make(chan struct{})
	var once sync.Once
	p.beforeDeletePersist = func() {
		once.Do(func() { close(persistStarted) })
		<-releasePersist
	}
	deleteDone := make(chan error, 1)
	go func() {
		_, _, err := p.RemoveKeys([]string{remove.Key()})
		deleteDone <- err
	}()
	<-persistStarted

	updateDone := make(chan struct{})
	go func() {
		p.UpdateLatency(keep.Key(), 77)
		p.SetAvailable(keep.Key(), false)
		close(updateDone)
	}()
	select {
	case <-updateDone:
	case <-time.After(2 * time.Second):
		close(releasePersist)
		t.Fatal("health update blocked behind deletion persistence")
	}
	close(releasePersist)
	if err := <-deleteDone; err != nil {
		t.Fatalf("RemoveKeys failed: %v", err)
	}

	forwarding, _, _ := cache.load()
	if len(forwarding) != 1 || forwarding[0].Key() != keep.Key() || forwarding[0].LatencyMs != 77 || forwarding[0].Available {
		t.Fatalf("durable pool lost concurrent health update: %#v", forwarding)
	}
}

func TestClearUnavailableDoesNotBlockUpdateDuringPersistence(t *testing.T) {
	dead := testProxy("socks5", "8.8.8.91", "1080", false)
	keep := testProxy("http", "8.8.4.91", "8080", true)
	p := NewProxyPool()
	p.Prime([]Proxy{dead, keep}, nil)
	cache := newPoolCache(t.TempDir())
	p.SetCache(cache)

	persistStarted := make(chan struct{})
	releasePersist := make(chan struct{})
	var once sync.Once
	p.beforeDeletePersist = func() {
		once.Do(func() { close(persistStarted) })
		<-releasePersist
	}
	clearDone := make(chan error, 1)
	go func() {
		_, err := p.ClearUnavailable()
		clearDone <- err
	}()
	<-persistStarted

	updateDone := make(chan struct{})
	go func() {
		p.UpdateLatency(keep.Key(), 88)
		close(updateDone)
	}()
	select {
	case <-updateDone:
	case <-time.After(2 * time.Second):
		close(releasePersist)
		t.Fatal("pool update blocked behind clear persistence")
	}
	close(releasePersist)
	if err := <-clearDone; err != nil {
		t.Fatalf("ClearUnavailable failed: %v", err)
	}

	forwarding, _, _ := cache.load()
	if len(forwarding) != 1 || forwarding[0].Key() != keep.Key() || forwarding[0].LatencyMs != 88 {
		t.Fatalf("durable pool lost concurrent update: %#v", forwarding)
	}
}

func TestRandomHealthyProxySkipsUnroutableNodes(t *testing.T) {
	policyExcluded := testProxy("socks5", "192.0.2.231", "1080", true)
	policyExcluded.PolicyExcluded = true
	invalidated := testProxy("socks5", "192.0.2.232", "1080", true)
	invalidated.HealthInvalidated = true
	retired := testProxy("socks5", "192.0.2.233", "1080", true)
	retired.SourceRetired = true
	unavailable := testProxy("socks5", "192.0.2.234", "1080", false)
	proxyip := testProxy("proxyip", "192.0.2.235", "443", true)

	p := NewProxyPool()
	p.Prime([]Proxy{policyExcluded, invalidated, retired, unavailable, proxyip}, nil)

	if got, ok := p.RandomHealthyProxy(); ok {
		t.Fatalf("RandomHealthyProxy() = (%v, true) when only unroutable nodes exist, want ok=false", got.Key())
	}

	healthy := testProxy("socks5", "192.0.2.236", "1080", true)
	p.Prime([]Proxy{policyExcluded, invalidated, retired, unavailable, proxyip, healthy}, nil)
	for i := 0; i < 32; i++ {
		got, ok := p.RandomHealthyProxy()
		if !ok {
			t.Fatalf("RandomHealthyProxy() iteration %d returned ok=false with a healthy node present", i)
		}
		if got.Key() != healthy.Key() {
			t.Fatalf("RandomHealthyProxy() iteration %d returned %q, want healthy node %q", i, got.Key(), healthy.Key())
		}
	}
}
