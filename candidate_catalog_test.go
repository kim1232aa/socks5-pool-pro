package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func TestCandidateCatalogPageClassificationFilteringAndRedaction(t *testing.T) {
	available := Proxy{IP: "192.0.2.1", Port: "8080", Protocol: "http", Available: true}
	unavailable := Proxy{IP: "192.0.2.1", Port: "8080", Protocol: "socks5", Available: false}
	failed := Proxy{
		IP: "192.0.2.2", Port: "3128", Protocol: "http", Country: "JP", City: "Tokyo", Continent: "AS",
		SourceName: "alpha", SourceNames: []string{"alpha", "beta"}, Username: "catalog-user", Password: "do-not-leak",
	}
	policy := Proxy{IP: "192.0.2.3", Port: "443", Protocol: "https", Country: "JP", Continent: "AS", SourceName: "alpha", Available: true}
	deferred := Proxy{IP: "192.0.2.4", Port: "1080", Protocol: "socks5", SourceName: "gamma"}
	resource := Proxy{IP: "198.51.100.5", Port: "443", Protocol: "proxyip", Country: "HK", Continent: "AS", SourceName: "resource-feed"}
	available.Country, available.Continent, available.SourceName = "US", "NA", "alpha"
	unavailable.SourceName = "beta"

	pool := NewProxyPool()
	pool.Prime([]Proxy{available, unavailable, policy}, nil)
	inventory := []Proxy{resource, deferred, policy, failed, unavailable, available}
	refresh := pool.candidates.begin(inventory, nil, nil, 0)
	pool.candidates.complete(refresh, []Proxy{available, failed, policy}, []Proxy{available}, map[string]bool{policy.Key(): true})
	server := NewStatusServer(pool, &ConfigStore{})

	page, raw := getCandidatePage(t, server.handler(), "/api/candidates/page?page_size=100")
	if page.CandidateTotal != 6 || page.FilteredTotal != 6 || page.Phase != "complete" {
		t.Fatalf("catalog page totals/phase = %#v", page)
	}
	if strings.Contains(raw, "catalog-user") || strings.Contains(raw, "do-not-leak") {
		t.Fatalf("candidate response leaked upstream credentials: %s", raw)
	}
	byKey := make(map[string]CandidateView)
	for _, candidate := range page.Candidates {
		byKey[candidate.Key] = candidate
	}
	assertCandidateStatus(t, byKey, available.Key(), "known_available")
	assertCandidateStatus(t, byKey, unavailable.Key(), "known_unavailable")
	assertCandidateStatus(t, byKey, failed.Key(), "checked_failed")
	assertCandidateStatus(t, byKey, policy.Key(), "policy_filtered")
	assertCandidateStatus(t, byKey, deferred.Key(), "deferred")
	assertCandidateStatus(t, byKey, resource.Key(), "resource")
	if !byKey[failed.Key()].HasAuth {
		t.Fatal("authenticated candidate did not expose has_auth=true")
	}
	if byKey[resource.Key()].Routable {
		t.Fatal("proxyip resource was marked routable")
	}
	if got := byKey[failed.Key()].SourceNames; len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("multi-source attribution = %v", got)
	}
	if byKey[failed.Key()].SourceCountry != "JP" || byKey[failed.Key()].Country != "JP" {
		t.Fatalf("source geography missing: %#v", byKey[failed.Key()])
	}
	if byKey[failed.Key()].LastChecked == "" || byKey[deferred.Key()].LastChecked != "" {
		t.Fatalf("last_checked semantics failed: failed=%q deferred=%q", byKey[failed.Key()].LastChecked, byKey[deferred.Key()].LastChecked)
	}

	beta, _ := getCandidatePage(t, server.handler(), "/api/candidates/page?source=beta&page_size=100")
	if beta.FilteredTotal != 2 {
		t.Fatalf("source=beta total = %d, want both primary and secondary attribution", beta.FilteredTotal)
	}
	unknown, _ := getCandidatePage(t, server.handler(), "/api/candidates/page?country=__unknown__&page_size=100")
	if unknown.FilteredTotal != 2 {
		t.Fatalf("unknown-country total = %d, want 2", unknown.FilteredTotal)
	}
	jpFailed, _ := getCandidatePage(t, server.handler(), "/api/candidates/page?country=JP&status=failed&page_size=100")
	if jpFailed.FilteredTotal != 1 || jpFailed.Candidates[0].Key != failed.Key() {
		t.Fatalf("JP failed filter = %#v", jpFailed)
	}
	protocolVariant, _ := getCandidatePage(t, server.handler(), "/api/candidates/page?search=192.0.2.1&page_size=100")
	if protocolVariant.FilteredTotal != 2 {
		t.Fatalf("protocol-aware variants at one address = %d, want 2", protocolVariant.FilteredTotal)
	}
	clamped, _ := getCandidatePage(t, server.handler(), "/api/candidates/page?page=999&page_size=100000")
	if clamped.Page != 1 || clamped.PageSize != maxCandidatePageSize {
		t.Fatalf("page bounds = page %d size %d", clamped.Page, clamped.PageSize)
	}

	// Availability is a live overlay, not frozen at scrape completion.
	pool.SetAvailable(available.Key(), false)
	updated, _ := getCandidatePage(t, server.handler(), "/api/candidates/page?search=192.0.2.1:8080&page_size=100")
	updatedByKey := make(map[string]CandidateView)
	for _, candidate := range updated.Candidates {
		updatedByKey[candidate.Key] = candidate
	}
	assertCandidateStatus(t, updatedByKey, available.Key(), "known_unavailable")
}

func TestCandidateCatalogResetHealthOutcomesRetainsInventoryAndResources(t *testing.T) {
	failed := Proxy{IP: "192.0.2.51", Port: "8080", Protocol: "http", SourceName: "feed"}
	policy := Proxy{IP: "192.0.2.52", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	resource := Proxy{IP: "198.51.100.52", Port: "443", Protocol: "proxyip", SourceName: "resource"}
	pool := NewProxyPool()
	refresh := pool.candidates.begin([]Proxy{failed, policy, resource}, nil, nil, 0)
	pool.candidates.complete(refresh, []Proxy{failed, policy}, nil, map[string]bool{policy.Key(): true})

	if reset := pool.candidates.ResetHealthOutcomes(); reset != 2 {
		t.Fatalf("reset=%d, want 2", reset)
	}
	page := NewStatusServer(pool, &ConfigStore{}).buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page?page_size=100", nil))
	if page.CandidateTotal != 3 || page.Phase != "restored" {
		t.Fatalf("reset page = %#v", page)
	}
	byKey := make(map[string]CandidateView)
	for _, candidate := range page.Candidates {
		byKey[candidate.Key] = candidate
	}
	assertCandidateStatus(t, byKey, failed.Key(), "deferred")
	assertCandidateStatus(t, byKey, policy.Key(), "deferred")
	assertCandidateStatus(t, byKey, resource.Key(), "resource")
	if byKey[failed.Key()].LastChecked != "" || byKey[policy.Key()].LastChecked != "" {
		t.Fatalf("criterion-dependent timestamps survived reset: %#v", byKey)
	}
}

func TestCandidateCatalogHasAuthIncludesAlternateCredentials(t *testing.T) {
	px := Proxy{
		IP: "192.0.2.53", Port: "1080", Protocol: "socks5", SourceName: "feed",
		CredentialAlternates: []ProxyCredential{{Username: "alternate", Password: "secret"}},
	}
	pool := NewProxyPool()
	refresh := pool.candidates.begin([]Proxy{px}, nil, nil, 0)
	pool.candidates.complete(refresh, nil, nil, nil)
	page := NewStatusServer(pool, &ConfigStore{}).buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page", nil))
	if len(page.Candidates) != 1 || !page.Candidates[0].HasAuth {
		t.Fatalf("alternate credential auth marker = %#v", page.Candidates)
	}
}

func TestCandidateCatalogPartialSourceCycleUnionsInsteadOfDeleting(t *testing.T) {
	pool := NewProxyPool()
	oldA := Proxy{IP: "192.0.2.10", Port: "80", Protocol: "http", SourceName: "source-a", Country: "US"}
	oldB := Proxy{IP: "192.0.2.11", Port: "80", Protocol: "http", SourceName: "source-b", Country: "JP"}
	first := pool.candidates.begin([]Proxy{oldA, oldB}, nil, nil, 0)
	pool.candidates.complete(first, nil, nil, nil)

	newB := Proxy{IP: "192.0.2.12", Port: "80", Protocol: "http", SourceName: "source-b", Country: "DE"}
	partial := pool.candidates.begin([]Proxy{oldB, newB}, nil, map[string]bool{legacySourceKey("source-a"): true}, 1) // source-a failed
	pool.candidates.complete(partial, nil, nil, nil)

	page, _ := getCandidatePage(t, NewStatusServer(pool, &ConfigStore{}).handler(), "/api/candidates/page?page_size=100")
	if page.Phase != "partial" || page.SourceErrors != 1 {
		t.Fatalf("partial phase = %q errors=%d", page.Phase, page.SourceErrors)
	}
	if page.CandidateTotal != 3 {
		t.Fatalf("partial union total = %d, want old A + old B + new B", page.CandidateTotal)
	}
	keys := make(map[string]bool)
	for _, candidate := range page.Candidates {
		keys[candidate.Key] = true
	}
	for _, want := range []string{oldA.Key(), oldB.Key(), newB.Key()} {
		if !keys[want] {
			t.Errorf("partial union lost %q", want)
		}
	}
}

func TestCandidateCatalogMergesSourcesOnPartialOverlap(t *testing.T) {
	pool := NewProxyPool()
	pxA := Proxy{IP: "192.0.2.20", Port: "1080", Protocol: "socks5", SourceName: "source-a", SourceNames: []string{"source-a"}}
	first := pool.candidates.begin([]Proxy{pxA}, nil, nil, 0)
	pool.candidates.complete(first, nil, nil, nil)
	pxB := pxA
	pxB.SourceName, pxB.SourceNames = "source-b", []string{"source-b"}
	second := pool.candidates.begin([]Proxy{pxB}, nil, map[string]bool{legacySourceKey("source-a"): true}, 1)
	pool.candidates.complete(second, nil, nil, nil)
	page, _ := getCandidatePage(t, NewStatusServer(pool, &ConfigStore{}).handler(), "/api/candidates/page?page_size=100")
	if got := page.Candidates[0].SourceNames; len(got) != 2 || got[0] != "source-a" || got[1] != "source-b" {
		t.Fatalf("merged partial sources = %v", got)
	}
}

func TestCandidateCatalogPartialRefreshIsSourceAuthoritative(t *testing.T) {
	pool := NewProxyPool()
	labels := map[string]string{"source-a-id": "source-a", "source-b-id": "source-b"}
	oldA := candidateFromSource("192.0.2.60", "source-a-id", "source-a")
	newA := candidateFromSource("192.0.2.61", "source-a-id", "source-a")
	oldB := candidateFromSource("192.0.2.62", "source-b-id", "source-b")
	first := pool.candidates.begin([]Proxy{oldA, oldB}, labels, nil, 0)
	pool.candidates.complete(first, nil, nil, nil)

	// A succeeded and now advertises newA instead of oldA; B failed.
	second := pool.candidates.begin([]Proxy{newA}, labels, map[string]bool{"source-b-id": true}, 1)
	pool.candidates.complete(second, nil, nil, nil)
	page, _ := getCandidatePage(t, NewStatusServer(pool, &ConfigStore{}).handler(), "/api/candidates/page?page_size=100")
	keys := make(map[string]bool)
	for _, candidate := range page.Candidates {
		keys[candidate.Key] = true
	}
	if keys[oldA.Key()] || !keys[newA.Key()] || !keys[oldB.Key()] || page.CandidateTotal != 2 {
		t.Fatalf("source-authoritative partial keys = %v total=%d", keys, page.CandidateTotal)
	}
	if got := candidateFacetTotal(page.Sources, "source-a"); got != 1 {
		t.Errorf("source-a facet = %d, want 1", got)
	}
	if got := candidateFacetTotal(page.Sources, "source-b"); got != 1 {
		t.Errorf("source-b facet = %d, want 1", got)
	}
}

func TestCandidateCatalogPartialRefreshRetainsOnlyFailedAttribution(t *testing.T) {
	pool := NewProxyPool()
	labels := map[string]string{"source-a-id": "same-name", "source-b-id": "same-name"}
	shared := candidateFromSource("192.0.2.70", "source-a-id", "same-name")
	shared.SourceNames = []string{"source-a-id", "source-b-id"}
	first := pool.candidates.begin([]Proxy{shared}, labels, nil, 0)
	pool.candidates.complete(first, nil, nil, nil)

	// A succeeded but removed the key; B (with the same display name) failed.
	second := pool.candidates.begin(nil, labels, map[string]bool{"source-b-id": true}, 1)
	pool.candidates.complete(second, nil, nil, nil)
	snapshot := pool.candidates.snapshot.Load()
	if snapshot == nil || len(snapshot.records) != 1 {
		t.Fatalf("partial duplicate-name snapshot = %#v", snapshot)
	}
	record := snapshot.records[0]
	if record.sourceCount != 1 {
		t.Fatalf("retained source count = %d, want only failed B", record.sourceCount)
	}
	ref := snapshot.sourceRefs[record.sourceOffset]
	if got := snapshot.sourceKeys[ref]; got != "source-b-id" {
		t.Fatalf("retained stable source = %q, want source-b-id", got)
	}
	page, _ := getCandidatePage(t, NewStatusServer(pool, &ConfigStore{}).handler(), "/api/candidates/page?source=same-name")
	if page.FilteredTotal != 1 || len(page.Sources) != 1 || page.Sources[0].Total != 1 {
		t.Fatalf("duplicate display-name API facets = %#v", page)
	}
}

func TestCandidateCatalogFullRefreshReplacesInventoryAndKeepsIntersectionHistory(t *testing.T) {
	pool := NewProxyPool()
	labels := map[string]string{"source-a-id": "source-a"}
	removed := candidateFromSource("192.0.2.80", "source-a-id", "source-a")
	shared := candidateFromSource("192.0.2.81", "source-a-id", "source-a")
	added := candidateFromSource("192.0.2.82", "source-a-id", "source-a")
	first := pool.candidates.begin([]Proxy{removed, shared}, labels, nil, 0)
	pool.candidates.complete(first, []Proxy{shared}, nil, nil)
	before := pool.candidates.snapshot.Load()
	sharedIndex := before.find(shared.Protocol, shared.Addr())
	wantCheckedAt := before.records[sharedIndex].checkedUnix
	if before.records[sharedIndex].status != candidateCheckedFailed || wantCheckedAt == 0 {
		t.Fatalf("initial shared outcome = %#v", before.records[sharedIndex])
	}

	second := pool.candidates.begin([]Proxy{shared, added}, labels, nil, 0)
	after := pool.candidates.snapshot.Load()
	if len(after.records) != 2 || after.find(removed.Protocol, removed.Addr()) >= 0 || after.find(added.Protocol, added.Addr()) < 0 {
		t.Fatalf("full replacement inventory = %#v", after.records)
	}
	sharedIndex = after.find(shared.Protocol, shared.Addr())
	if got := after.records[sharedIndex]; got.status != candidateCheckedFailed || got.checkedUnix != wantCheckedAt {
		t.Fatalf("intersection history = %#v, want failed at %d", got, wantCheckedAt)
	}
	pool.candidates.complete(second, nil, nil, nil)
}

func TestCandidateCatalogMetadataAndAuthFallbackOnlyForFailedAttribution(t *testing.T) {
	labels := map[string]string{"old-id": "old", "new-id": "new"}
	old := candidateFromSource("192.0.2.90", "old-id", "old")
	old.Country, old.City, old.Continent = "JP", "Tokyo", "AS"
	old.Username, old.Password = "user", "secret"
	current := candidateFromSource("192.0.2.90", "new-id", "new")

	partialPool := NewProxyPool()
	first := partialPool.candidates.begin([]Proxy{old}, labels, nil, 0)
	partialPool.candidates.complete(first, nil, nil, nil)
	partialPool.candidates.begin([]Proxy{current}, labels, map[string]bool{"old-id": true}, 1)
	partialPage, raw := getCandidatePage(t, NewStatusServer(partialPool, &ConfigStore{}).handler(), "/api/candidates/page")
	if !partialPage.Candidates[0].HasAuth || partialPage.Candidates[0].Country != "JP" || strings.Contains(raw, "secret") {
		t.Fatalf("failed-attribution fallback = %#v raw=%s", partialPage.Candidates[0], raw)
	}

	fullPool := NewProxyPool()
	fullFirst := fullPool.candidates.begin([]Proxy{old}, labels, nil, 0)
	fullPool.candidates.complete(fullFirst, nil, nil, nil)
	fullPool.candidates.begin([]Proxy{current}, labels, nil, 0)
	fullPage, _ := getCandidatePage(t, NewStatusServer(fullPool, &ConfigStore{}).handler(), "/api/candidates/page")
	if fullPage.Candidates[0].HasAuth || fullPage.Candidates[0].Country != "Unknown" {
		t.Fatalf("successful-source metadata was not authoritative: %#v", fullPage.Candidates[0])
	}
}

func TestCandidateCatalogHandlerMethodAndLoadingState(t *testing.T) {
	pool := NewProxyPool()
	handler := NewStatusServer(pool, &ConfigStore{}).handler()
	loading, _ := getCandidatePage(t, handler, "/api/candidates/page")
	if loading.Phase != "loading" || loading.CandidateTotal != 0 || loading.Candidates == nil {
		t.Fatalf("empty loading page = %#v", loading)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodPost, "/api/candidates/page", nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST candidate page = %d Allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}
}

func TestCandidateSnapshotIDChangesForSameGenerationCompletionAndPoolOverlay(t *testing.T) {
	px := Proxy{IP: "192.0.2.90", Port: "8080", Protocol: "http", Available: true}
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	refresh := pool.candidates.begin([]Proxy{px}, nil, nil, 0)
	server := NewStatusServer(pool, &ConfigStore{})

	checking := server.buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page", nil))
	if checking.Phase != "checking" || checking.SnapshotID == "" {
		t.Fatalf("checking snapshot = %#v", checking)
	}
	pool.candidates.complete(refresh, []Proxy{px}, []Proxy{px}, nil)
	completed := server.buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page", nil))
	if completed.Phase != "complete" || completed.SnapshotID == checking.SnapshotID {
		t.Fatalf("completion reused snapshot token: checking=%q completed=%q", checking.SnapshotID, completed.SnapshotID)
	}

	pool.SetAvailable(px.Key(), false)
	overlaid := server.buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page", nil))
	if overlaid.SnapshotID == completed.SnapshotID || len(overlaid.Candidates) != 1 || overlaid.Candidates[0].Status != candidateKnownUnavailable.String() {
		t.Fatalf("pool overlay reused token/content: completed=%q overlaid=%#v", completed.SnapshotID, overlaid)
	}

	stale := httptest.NewRecorder()
	path := "/api/candidates/page?snapshot_id=" + url.QueryEscape(completed.SnapshotID)
	server.handler().ServeHTTP(stale, localTestRequest(http.MethodGet, path, nil))
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"snapshot_changed"`) {
		t.Fatalf("stale same-generation token = %d %s", stale.Code, stale.Body.String())
	}
}

func TestSnapshotIDBootNoncePreventsCrossRestartReuse(t *testing.T) {
	poolA := formatPoolSnapshotIDWithBoot("boot-a", 7)
	poolB := formatPoolSnapshotIDWithBoot("boot-b", 7)
	if poolA == poolB || poolA != formatPoolSnapshotIDWithBoot("boot-a", 7) {
		t.Fatalf("pool boot snapshot IDs are not stable/process-unique: %q %q", poolA, poolB)
	}
	candidateA := formatCandidateSnapshotIDWithBoot("boot-a", 3, 2, 7)
	candidateB := formatCandidateSnapshotIDWithBoot("boot-b", 3, 2, 7)
	if candidateA == candidateB || candidateA != formatCandidateSnapshotIDWithBoot("boot-a", 3, 2, 7) {
		t.Fatalf("candidate boot snapshot IDs are not stable/process-unique: %q %q", candidateA, candidateB)
	}
	views := []V1ProxyView{{ProxyURL: "http://192.0.2.1:80", Key: "http://192.0.2.1:80", Protocol: "http"}}
	v1A := formatV1ProxySnapshotIDWithBoot("boot-a", views)
	v1B := formatV1ProxySnapshotIDWithBoot("boot-b", views)
	if v1A == v1B || v1A != formatV1ProxySnapshotIDWithBoot("boot-a", views) {
		t.Fatalf("v1 boot snapshot IDs are not stable/process-unique: %q %q", v1A, v1B)
	}
}

func TestCandidateCountryContractAcceptsOnlyASCIIISO2(t *testing.T) {
	pool := NewProxyPool()
	inventory := []Proxy{
		{IP: "192.0.2.100", Port: "80", Protocol: "http", Country: "jp", City: "Tokyo", SourceName: "feed"},
		{IP: "192.0.2.101", Port: "80", Protocol: "http", Country: "Japan", City: "Osaka", SourceName: "feed"},
		{IP: "192.0.2.102", Port: "80", Protocol: "http", Country: "??", SourceName: "feed"},
		{IP: "192.0.2.103", Port: "80", Protocol: "http", Country: "u1", SourceName: "feed"},
	}
	refresh := pool.candidates.begin(inventory, nil, nil, 0)
	pool.candidates.complete(refresh, nil, nil, nil)
	handler := NewStatusServer(pool, &ConfigStore{}).handler()
	page, _ := getCandidatePage(t, handler, "/api/candidates/page?page_size=100")
	if page.CountryUnknownTotal != 3 || len(page.Countries) != 1 || page.Countries[0].Country != "JP" || page.Countries[0].Total != 1 {
		t.Fatalf("strict country facets = %#v unknown=%d", page.Countries, page.CountryUnknownTotal)
	}
	unknown, _ := getCandidatePage(t, handler, "/api/candidates/page?country=__unknown__&page_size=100")
	if unknown.FilteredTotal != 3 {
		t.Fatalf("unknown strict-country filter = %d", unknown.FilteredTotal)
	}
	invalidRecorder := httptest.NewRecorder()
	handler.ServeHTTP(invalidRecorder, localTestRequest(http.MethodGet, "/api/candidates/page?country=Japan&page_size=100", nil))
	if invalidRecorder.Code != http.StatusBadRequest || !strings.Contains(invalidRecorder.Body.String(), `"code":"invalid_country"`) {
		t.Fatalf("invalid country query = %d %s, want structured 400", invalidRecorder.Code, invalidRecorder.Body.String())
	}
	city, _ := getCandidatePage(t, handler, "/api/candidates/page?search=Osaka&page_size=100")
	if city.FilteredTotal != 1 || city.Candidates[0].Country != "Unknown" {
		t.Fatalf("city search/source-country separation = %#v", city)
	}
}

func TestCandidateRecordStaysCompact(t *testing.T) {
	if size := unsafe.Sizeof(candidateRecord{}); size > 64 {
		t.Fatalf("candidateRecord is %d bytes; compact catalog would exceed its memory budget", size)
	}
}

func TestProxyIPResourcesNeverEnterHealthInventory(t *testing.T) {
	forwarding := Proxy{IP: "192.0.2.40", Port: "1080", Protocol: "socks5"}
	resource := Proxy{IP: "198.51.100.40", Port: "443", Protocol: "proxyip"}
	health, resources := splitHealthInventory([]Proxy{resource, forwarding})
	if resources != 1 || len(health) != 1 || health[0].Key() != forwarding.Key() {
		t.Fatalf("health inventory = %#v resources=%d", health, resources)
	}
}

func TestStatusProxyIPTotalComesFromCatalogWithoutEnteringRoutablePool(t *testing.T) {
	pool := NewProxyPool()
	resource := Proxy{IP: "198.51.100.41", Port: "443", Protocol: "proxyip", SourceName: "resource-feed"}
	refresh := pool.candidates.begin([]Proxy{resource}, nil, nil, 0)
	pool.candidates.complete(refresh, nil, nil, nil)
	summary := NewStatusServer(pool, &ConfigStore{}).buildSummaryWithProxies(false)
	if summary.ProxyIPTotal != 1 || summary.Total != 0 || len(summary.Proxies) != 0 {
		t.Fatalf("status compatibility counts = proxyip %d total %d proxies=%v", summary.ProxyIPTotal, summary.Total, summary.Proxies)
	}
}

func TestNodesPageSupportsUnknownCountryFilter(t *testing.T) {
	pool := NewProxyPool()
	unknown := Proxy{IP: "192.0.2.50", Port: "8080", Protocol: "http", Available: true}
	known := Proxy{IP: "192.0.2.51", Port: "8080", Protocol: "http", Country: "JP", Available: true}
	pool.Prime([]Proxy{unknown, known}, nil)
	server := NewStatusServer(pool, &ConfigStore{})
	recorder := httptest.NewRecorder()
	server.handler().ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/nodes/page?country=__unknown__", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unknown node country response = %d: %s", recorder.Code, recorder.Body.String())
	}
	var page NodePageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.FilteredTotal != 1 || len(page.Nodes) != 1 || page.Nodes[0].Key != unknown.Key() || page.CountryUnknownTotal != 1 {
		t.Fatalf("unknown node country page = %#v", page)
	}
}

func BenchmarkCandidateCatalogPage100K(b *testing.B) {
	candidates := benchmarkCandidates(100_000)
	labels := benchmarkSourceLabels()
	pool := NewProxyPool()
	refresh := pool.candidates.begin(candidates, labels, nil, 0)
	pool.candidates.complete(refresh, nil, nil, nil)
	server := NewStatusServer(pool, &ConfigStore{})
	request := localTestRequest(http.MethodGet, "/api/candidates/page?page=10&page_size=50&protocol=http", nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page := server.buildCandidatePage(request)
		if len(page.Candidates) != 50 {
			b.Fatal(len(page.Candidates))
		}
	}
}

func BenchmarkCandidateCatalogBuild100K(b *testing.B) {
	candidates := benchmarkCandidates(100_000)
	labels := benchmarkSourceLabels()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		catalog := &CandidateCatalog{}
		refresh := catalog.begin(candidates, labels, nil, 0)
		catalog.complete(refresh, nil, nil, nil)
		if snapshot := catalog.snapshot.Load(); snapshot == nil || len(snapshot.records) != len(candidates) {
			b.Fatal("incomplete snapshot")
		}
	}
}

func BenchmarkCandidateCatalogPartialMerge100K(b *testing.B) {
	candidates := benchmarkCandidates(100_000)
	labels := benchmarkSourceLabels()
	catalog := &CandidateCatalog{}
	first := catalog.begin(candidates, labels, nil, 0)
	catalog.complete(first, nil, nil, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		refresh := catalog.begin(candidates, labels, map[string]bool{"source-0": true}, 1)
		catalog.complete(refresh, nil, nil, nil)
		if snapshot := catalog.snapshot.Load(); snapshot == nil || len(snapshot.records) != len(candidates) {
			b.Fatal("incomplete merged snapshot")
		}
	}
}

func BenchmarkCandidateCatalogFullRefresh481K(b *testing.B) {
	benchmarkCandidateCatalogRefresh(b, 481_000, false)
}

func BenchmarkCandidateCatalogPartialRefresh481K(b *testing.B) {
	benchmarkCandidateCatalogRefresh(b, 481_000, true)
}

func BenchmarkCandidateRefreshPipeline481K(b *testing.B) {
	all := benchmarkCandidates(481_000)
	labels := benchmarkSourceLabels()
	catalog := &CandidateCatalog{}
	base := dedupeCandidates(all)
	first := catalog.begin(base, labels, nil, 0)
	catalog.complete(first, nil, nil, nil)
	base = nil
	runtime.GC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		deduped := dedupeCandidates(all)
		refresh := catalog.begin(deduped, labels, map[string]bool{"source-0": true}, 1)
		catalog.complete(refresh, nil, nil, nil)
		restoreCandidateSourceLabels(deduped, labels)
		health, _ := splitHealthInventory(deduped)
		selected := newCandidateSampler("").selectCandidates(health, nil, 1500)
		if len(selected) != 1500 {
			b.Fatalf("sampled %d candidates", len(selected))
		}
		if snapshot := catalog.snapshot.Load(); snapshot == nil || len(snapshot.records) != len(deduped) {
			b.Fatal("incomplete pipeline snapshot")
		}
	}
}

func benchmarkCandidateCatalogRefresh(b *testing.B, total int, partial bool) {
	candidates := benchmarkCandidates(total)
	labels := benchmarkSourceLabels()
	catalog := &CandidateCatalog{}
	first := catalog.begin(candidates, labels, nil, 0)
	catalog.complete(first, nil, nil, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		failed, errors := map[string]bool(nil), 0
		if partial {
			failed, errors = map[string]bool{"source-0": true}, 1
		}
		refresh := catalog.begin(candidates, labels, failed, errors)
		catalog.complete(refresh, nil, nil, nil)
		if snapshot := catalog.snapshot.Load(); snapshot == nil || len(snapshot.records) != total {
			b.Fatal("incomplete refreshed snapshot")
		}
	}
}

func benchmarkCandidates(total int) []Proxy {
	candidates := make([]Proxy, total)
	for i := range candidates {
		candidates[i] = Proxy{
			IP: fmt.Sprintf("10.%d.%d.%d", (i>>16)&255, (i>>8)&255, i&255), Port: fmt.Sprintf("%d", 1000+i%60000),
			Protocol: []string{"http", "socks5", "https"}[i%3], SourceName: fmt.Sprintf("source-%d", i%8),
			Country: []string{"US", "JP", "DE", ""}[i%4], Continent: []string{"NA", "AS", "EU", ""}[i%4],
		}
	}
	return candidates
}

func benchmarkSourceLabels() map[string]string {
	labels := make(map[string]string, 8)
	for i := 0; i < 8; i++ {
		labels[fmt.Sprintf("source-%d", i)] = fmt.Sprintf("Source %d", i)
	}
	return labels
}

func assertCandidateStatus(t *testing.T, candidates map[string]CandidateView, key, status string) {
	t.Helper()
	candidate, ok := candidates[key]
	if !ok {
		t.Fatalf("candidate %q missing", key)
	}
	if candidate.Status != status {
		t.Fatalf("candidate %q status = %q, want %q", key, candidate.Status, status)
	}
}

func candidateFromSource(ip, id, name string) Proxy {
	return Proxy{
		IP: ip, Port: "8080", Protocol: "http", SourceName: id, SourceNames: []string{id},
	}
}

func candidateFacetTotal(facets []CandidateFacet, value string) int {
	for _, facet := range facets {
		if facet.Value == value {
			return facet.Total
		}
	}
	return 0
}

func getCandidatePage(t *testing.T, handler http.Handler, path string) (CandidatePageResponse, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, recorder.Code, recorder.Body.String())
	}
	var page CandidatePageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode GET %s: %v; body=%s", path, err, recorder.Body.String())
	}
	return page, recorder.Body.String()
}
