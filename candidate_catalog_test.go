package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestCandidatePageContainsOnlyPendingForwardingRecords(t *testing.T) {
	known := Proxy{IP: "192.0.2.10", Port: "1080", Protocol: "socks5", SourceName: "feed", Available: true}
	pending := Proxy{IP: "192.0.2.11", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	failed := Proxy{IP: "192.0.2.12", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	policy := Proxy{IP: "192.0.2.13", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	resource := Proxy{IP: "198.51.100.10", Port: "443", Protocol: "proxyip", SourceName: "resource-feed"}

	pool := NewProxyPool()
	pool.Prime([]Proxy{known}, nil)
	refresh := pool.candidates.begin([]Proxy{known, pending, failed, policy, resource}, nil, nil, 0)
	pool.candidates.complete(refresh, []Proxy{failed, policy}, nil, map[string]bool{policy.Key(): true})

	page := NewStatusServer(pool, &ConfigStore{}).buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page?page_size=100", nil))
	if page.CandidateTotal != 1 || page.FilteredTotal != 1 || len(page.Candidates) != 1 {
		t.Fatalf("pending candidate page = %#v", page)
	}
	if got := page.Candidates[0]; got.Key != pending.Key() || got.Status != candidateDeferred.String() || !got.Routable {
		t.Fatalf("candidate page record = %#v, want pending forwarding candidate %q", got, pending.Key())
	}
}

func TestCandidateOutcomeMovesFailureOutOfCandidates(t *testing.T) {
	unreachable := Proxy{IP: "192.0.2.20", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	policy := Proxy{IP: "192.0.2.21", Port: "8080", Protocol: "http", SourceName: "feed"}
	catalog := &CandidateCatalog{}
	refresh := catalog.begin([]Proxy{unreachable, policy}, nil, nil, 0)
	catalog.complete(refresh, []Proxy{unreachable, policy}, nil, map[string]bool{policy.Key(): true})

	snapshot := catalog.snapshot.Load()
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if len(snapshot.records) != 0 || len(snapshot.failedRecords) != 2 {
		t.Fatalf("outcome ownership = pending %d failed %d, want 0/2", len(snapshot.records), len(snapshot.failedRecords))
	}
	failures := make(map[string]CandidateFailureKind, 2)
	for _, record := range snapshot.failedRecords {
		key := snapshot.protocols[record.protocolID] + "://" + record.addr
		failures[key] = record.kind
	}
	if failures[unreachable.Key()] != candidateFailureUnreachable || failures[policy.Key()] != candidateFailurePolicyFiltered {
		t.Fatalf("failure kinds = %#v", failures)
	}
}

func TestFailedCandidatePageOmitsUnreachableIsolation(t *testing.T) {
	unreachable := Proxy{IP: "192.0.2.20", Port: "1080", Protocol: "socks5", SourceName: "Fyvri HTTP"}
	policy := Proxy{IP: "192.0.2.21", Port: "8080", Protocol: "http", SourceName: "feed"}
	pool := NewProxyPool()
	refresh := pool.candidates.begin([]Proxy{unreachable, policy}, nil, nil, 0)
	pool.candidates.complete(refresh, []Proxy{unreachable, policy}, nil, map[string]bool{policy.Key(): true})

	page := NewStatusServer(pool, &ConfigStore{}).buildFailedCandidatePage(localTestRequest(http.MethodGet, "/api/failed-candidates", nil))
	if page.FailedTotal != 1 || page.IsolatedUnreachableTotal != 1 || len(page.FailedCandidates) != 1 || page.FailedCandidates[0].Key != policy.Key() || page.FailedCandidates[0].FailureType != "policy_filtered" {
		t.Fatalf("retry page = %#v", page)
	}
	if keys := pool.candidates.FailedKeys(); len(keys) != 1 || keys[0] != policy.Key() {
		t.Fatalf("retry-all keys = %v, want only policy-filtered", keys)
	}
}

func TestCandidateOutcomeHidesAliveRecordBehindPoolOwnership(t *testing.T) {
	alive := Proxy{IP: "192.0.2.22", Port: "1080", Protocol: "socks5", SourceName: "feed", Available: true}
	pool := NewProxyPool()
	refresh := pool.candidates.begin([]Proxy{alive}, nil, nil, 0)
	pool.candidates.complete(refresh, []Proxy{alive}, []Proxy{alive}, nil)
	pool.Prime([]Proxy{alive}, nil)

	page := NewStatusServer(pool, &ConfigStore{}).buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page", nil))
	if page.CandidateTotal != 0 || page.FilteredTotal != 0 || len(page.Candidates) != 0 {
		t.Fatalf("alive pool-owned record remained pending: %#v", page)
	}
}

func TestFailedRediscoveryMergesSourcesWithoutRequeueing(t *testing.T) {
	failed := Proxy{IP: "192.0.2.23", Port: "1080", Protocol: "socks5", SourceName: "source-a", SourceNames: []string{"source-a"}}
	catalog := &CandidateCatalog{}
	first := catalog.begin([]Proxy{failed}, map[string]string{"source-a": "Source A", "source-b": "Source B"}, nil, 0)
	catalog.complete(first, []Proxy{failed}, nil, nil)
	before := catalog.snapshot.Load()
	before.mu.Lock()
	before.failedRecords[0].lastError = "dial timeout"
	checkedUnix := before.failedRecords[0].checkedUnix
	before.mu.Unlock()

	rediscovered := failed
	rediscovered.SourceName = "source-b"
	rediscovered.SourceNames = []string{"source-b"}
	catalog.begin([]Proxy{rediscovered}, map[string]string{"source-a": "Source A", "source-b": "Source B"}, nil, 0)

	snapshot := catalog.snapshot.Load()
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if len(snapshot.records) != 0 || len(snapshot.failedRecords) != 1 {
		t.Fatalf("rediscovered failure ownership = pending %d failed %d", len(snapshot.records), len(snapshot.failedRecords))
	}
	record := snapshot.failedRecords[0]
	if record.kind != candidateFailureUnreachable || record.lastError != "dial timeout" || record.checkedUnix != checkedUnix {
		t.Fatalf("rediscovery changed failure conclusion: %#v", record)
	}
	sources := make([]string, 0, record.sourceCount)
	for i := uint32(0); i < uint32(record.sourceCount); i++ {
		sources = append(sources, snapshot.sources[snapshot.sourceRefs[record.sourceOffset+i]])
	}
	sort.Strings(sources)
	if !reflect.DeepEqual(sources, []string{"Source A", "Source B"}) {
		t.Fatalf("rediscovered failure sources = %v", sources)
	}
}

func TestCandidateFailureKeysRemainProtocolAware(t *testing.T) {
	httpProxy := Proxy{IP: "192.0.2.24", Port: "8080", Protocol: "http", SourceName: "feed"}
	socksProxy := httpProxy
	socksProxy.Protocol = "socks5"
	catalog := &CandidateCatalog{}
	refresh := catalog.begin([]Proxy{httpProxy, socksProxy}, nil, nil, 0)
	catalog.complete(refresh, []Proxy{httpProxy, socksProxy}, nil, nil)

	snapshot := catalog.snapshot.Load()
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if len(snapshot.failedRecords) != 2 {
		t.Fatalf("protocol-aware failures = %d, want 2", len(snapshot.failedRecords))
	}
	if snapshot.protocols[snapshot.failedRecords[0].protocolID] == snapshot.protocols[snapshot.failedRecords[1].protocolID] {
		t.Fatalf("failure protocols collapsed: %#v", snapshot.failedRecords)
	}
}

func TestResetHealthOutcomesPreservesFailures(t *testing.T) {
	failed := Proxy{IP: "192.0.2.25", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	catalog := &CandidateCatalog{}
	refresh := catalog.begin([]Proxy{failed}, nil, nil, 0)
	catalog.complete(refresh, []Proxy{failed}, nil, nil)

	catalog.ResetHealthOutcomes()
	snapshot := catalog.snapshot.Load()
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if len(snapshot.failedRecords) != 1 || snapshot.failedRecords[0].kind != candidateFailureUnreachable {
		t.Fatalf("criterion reset changed failures: %#v", snapshot.failedRecords)
	}
}

func TestCandidateLeasePreventsDuplicateClaims(t *testing.T) {
	candidate := Proxy{IP: "192.0.2.26", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	catalog := &CandidateCatalog{}
	catalog.begin([]Proxy{candidate}, nil, nil, 0)

	first := catalog.LeasePending(1, nil)
	if len(first) != 1 || first[0].Key != candidate.Key() {
		t.Fatalf("first lease = %#v", first)
	}
	if duplicate := catalog.LeasePending(1, nil); len(duplicate) != 0 {
		t.Fatalf("duplicate lease = %#v", duplicate)
	}
	catalog.ReleaseLeases(first)
	retried := catalog.LeasePending(1, nil)
	if len(retried) != 1 || retried[0].Token == first[0].Token {
		t.Fatalf("released candidate was not leased with a fresh token: first=%#v retried=%#v", first, retried)
	}
}

func TestFailedLeaseRequiresExplicitKeys(t *testing.T) {
	failed := Proxy{IP: "192.0.2.27", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	catalog := &CandidateCatalog{}
	refresh := catalog.begin([]Proxy{failed}, nil, nil, 0)
	catalog.complete(refresh, []Proxy{failed}, nil, nil)

	if automatic := catalog.LeasePending(10, nil); len(automatic) != 0 {
		t.Fatalf("automatic pending lease claimed failures: %#v", automatic)
	}
	leases, missing := catalog.LeaseFailed([]string{failed.Key(), "socks5://192.0.2.99:1080"})
	if len(leases) != 0 || !reflect.DeepEqual(missing, []string{"socks5://192.0.2.99:1080"}) {
		t.Fatalf("partial failed lease = leases %#v missing %v", leases, missing)
	}
	leases, missing = catalog.LeaseFailed([]string{failed.Key()})
	if len(leases) != 1 || len(missing) != 0 || leases[0].Kind != "failed" {
		t.Fatalf("explicit failed lease = leases %#v missing %v", leases, missing)
	}
}

// TestReconcileFailuresKeepsMergedSourcesSorted guards the cache encoder
// invariants: when a rediscovered failure merges the current declaration's
// sources with the old failure's retained attributions, the merged source
// segment must stay strictly sorted, and the failure collection itself must
// stay sorted when rediscovered and retained failures interleave — otherwise
// the candidate cache refuses to persist the snapshot.
func TestReconcileFailuresKeepsMergedSourcesSorted(t *testing.T) {
	labels := map[string]string{"alpha-feed": "Alpha Feed", "zulu-feed": "Zulu Feed"}
	previous := buildCandidateSnapshot([]Proxy{
		{IP: "192.0.2.70", Port: "1080", Protocol: "socks5", SourceNames: []string{"zulu-feed"}},
		{IP: "192.0.2.80", Port: "1080", Protocol: "socks5", SourceNames: []string{"alpha-feed", "zulu-feed"}},
	}, labels)
	previous.failedRecords = []candidateFailureRecord{
		{candidateRecord: previous.records[0], kind: candidateFailureUnreachable, lastError: "dial timeout"},
		{candidateRecord: previous.records[1], kind: candidateFailureUnreachable, lastError: "dial timeout"},
	}
	previous.records = nil

	// Only the .80 failure is rediscovered; the .70 failure is retained. The
	// rediscovered record is appended before the retained one, which breaks
	// the collection order unless reconciliation re-sorts.
	current := buildCandidateSnapshot([]Proxy{
		{IP: "192.0.2.80", Port: "1080", Protocol: "socks5", SourceNames: []string{"zulu-feed"}},
	}, labels)

	reconciled := reconcileCandidateFailures(previous, current)
	if len(reconciled.records) != 0 || len(reconciled.failedRecords) != 2 {
		t.Fatalf("reconciled = %d pending %d failed, want 0 pending 2 failed", len(reconciled.records), len(reconciled.failedRecords))
	}
	rediscovered := reconciled.failedRecords[1]
	if !strings.HasSuffix(rediscovered.addr, ".80:1080") || rediscovered.sourceCount != 2 {
		t.Fatalf("rediscovered failure = addr %q sources %d, want .80:1080 with both attributions retained", rediscovered.addr, rediscovered.sourceCount)
	}
	// begin() assigns these after reconciliation in the live refresh path.
	reconciled.generation = 1
	reconciled.revision = 1
	reconciled.phase = "complete"
	if err := validateCandidateSnapshot(reconciled); err != nil {
		t.Fatalf("reconciled snapshot violates cache invariants: %v", err)
	}
}

func TestCancelledLeaseLeavesRecordInOriginalCollection(t *testing.T) {
	pending := Proxy{IP: "192.0.2.28", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	failed := Proxy{IP: "192.0.2.29", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	catalog := &CandidateCatalog{}
	refresh := catalog.begin([]Proxy{pending, failed}, nil, nil, 0)
	catalog.complete(refresh, []Proxy{failed}, nil, nil)

	pendingLeases := catalog.LeasePending(1, nil)
	failedLeases, missing := catalog.LeaseFailed([]string{failed.Key()})
	if len(pendingLeases) != 1 || len(failedLeases) != 1 || len(missing) != 0 {
		t.Fatalf("initial leases = pending %#v failed %#v missing %v", pendingLeases, failedLeases, missing)
	}
	catalog.ReleaseLeases(append(pendingLeases, failedLeases...))
	if len(catalog.LeasePending(1, nil)) != 1 {
		t.Fatal("cancelled pending lease did not remain pending")
	}
	if retried, missing := catalog.LeaseFailed([]string{failed.Key()}); len(retried) != 1 || len(missing) != 0 {
		t.Fatalf("cancelled failure lease did not remain failed: %#v %v", retried, missing)
	}
}

func TestStaleLeaseCannotPublishIntoReplacementSnapshot(t *testing.T) {
	candidate := Proxy{IP: "192.0.2.30", Port: "1080", Protocol: "socks5", SourceName: "feed", Username: "old", Password: "secret"}
	catalog := &CandidateCatalog{}
	catalog.begin([]Proxy{candidate}, nil, nil, 0)
	leases := catalog.LeasePending(1, nil)
	if len(leases) != 1 {
		t.Fatalf("initial leases = %#v", leases)
	}
	replacement := candidate
	replacement.Username = "new"
	replacement.Password = "changed"
	catalog.begin([]Proxy{replacement}, nil, nil, 0)
	outcomes := map[string]candidateCheckOutcome{
		candidate.Key(): {Key: candidate.Key(), Proxy: candidate, Kind: candidateCheckUnreachable, Error: "dial timeout"},
	}
	if err := catalog.CommitLeaseOutcomes(leases, outcomes); err == nil {
		t.Fatal("stale lease committed into changed declaration")
	}
	snapshot := catalog.snapshot.Load()
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if len(snapshot.records) != 1 || len(snapshot.failedRecords) != 0 || snapshot.records[0].username != "new" {
		t.Fatalf("stale lease changed replacement snapshot: pending=%#v failed=%#v", snapshot.records, snapshot.failedRecords)
	}
}

func TestCatalogBudgetCountsInventoryAndFailuresWithoutEviction(t *testing.T) {
	snapshot := buildCandidateSnapshot([]Proxy{
		{IP: "192.0.2.71", Port: "1080", Protocol: "socks5", SourceName: "feed"},
		{IP: "192.0.2.72", Port: "8080", Protocol: "http", SourceName: "feed"},
	}, nil)
	failedRecord := snapshot.records[1]
	snapshot.records = snapshot.records[:1]
	snapshot.failedRecords = []candidateFailureRecord{{candidateRecord: failedRecord, kind: candidateFailureUnreachable}}
	snapshot.generation = 1
	snapshot.revision = 1
	snapshot.phase = "complete"

	if err := validateCandidateSnapshot(snapshot); err != nil {
		t.Fatalf("inventory+failure snapshot under budget rejected: %v", err)
	}
	oversized := cloneCandidateSnapshot(snapshot)
	oversized.records = make([]candidateRecord, maxCandidateCacheRecords)
	oversized.failedRecords = []candidateFailureRecord{{kind: candidateFailureUnreachable}}
	if err := validateCandidateSnapshot(oversized); err == nil || !strings.Contains(err.Error(), "combined record limit") {
		t.Fatalf("combined inventory+failure budget overflow error = %v", err)
	}
	if got := len(snapshot.failedRecords); got != 1 {
		t.Fatalf("budget validation evicted %d failures, want 1 retained", got)
	}
}

// TestCandidateCatalogBeginDoesNotResurrectConcurrentRemoval guards against a
// blind Store in begin(): a scrape cycle merges against a snapshot it read
// before locking, so a concurrent RemoveKeys (which shares no lock with
// begin) can delete a candidate and publish that deletion while the merge is
// still in flight. If begin() then published unconditionally, the deletion
// would be silently undone in memory and, on the following complete(), on
// disk too - specifically via the failed-source carry-forward path, which is
// the only way a previous-only record survives into a freshly built snapshot.
func TestCandidateCatalogBeginDoesNotResurrectConcurrentRemoval(t *testing.T) {
	dir := t.TempDir()
	catalog := &CandidateCatalog{}
	catalog.SetDiskCache(newCandidateCatalogCache(dir))
	remove := Proxy{IP: "8.8.8.82", Port: "1080", Protocol: "socks5", SourceName: "flaky"}
	keep := Proxy{IP: "8.8.4.52", Port: "8080", Protocol: "http", SourceName: "feed"}
	refresh := catalog.begin([]Proxy{remove, keep}, nil, nil, 0)
	catalog.complete(refresh, nil, nil, nil)

	hookCalls := 0
	catalog.beginBeforePublish = func() {
		if hookCalls != 0 {
			return
		}
		hookCalls++
		removed, notFound, err := catalog.RemoveKeys([]string{remove.Key()})
		if err != nil || len(removed) != 1 || len(notFound) != 0 {
			t.Fatalf("concurrent RemoveKeys result removed=%v notFound=%v err=%v", removed, notFound, err)
		}
	}
	// "flaky" fails to fetch this cycle, so `remove` is absent from the fresh
	// candidate list and only survives via the failed-source carry-forward in
	// mergeCandidateSnapshots - the exact path that must not resurrect a
	// deletion that landed after this cycle read its base snapshot.
	failedSources := map[string]bool{legacySourceKey("flaky"): true}
	refresh2 := catalog.begin([]Proxy{keep}, nil, failedSources, 1)
	if hookCalls != 1 {
		t.Fatalf("begin-before-publish hook calls = %d, want 1", hookCalls)
	}
	if _, ok := catalog.FindByKey(remove.Key()); ok {
		t.Fatal("begin() resurrected a candidate deleted by a concurrent RemoveKeys")
	}

	catalog.complete(refresh2, []Proxy{keep}, []Proxy{keep}, nil)
	if _, ok := catalog.FindByKey(remove.Key()); ok {
		t.Fatal("complete() after the race resurrected the deleted candidate")
	}

	restored := &CandidateCatalog{}
	restored.SetDiskCache(newCandidateCatalogCache(dir))
	loaded, err := restored.LoadDiskCache()
	if err != nil || !loaded {
		t.Fatalf("LoadDiskCache = %v, %v", loaded, err)
	}
	if _, ok := restored.FindByKey(remove.Key()); ok {
		t.Fatal("begin() resurrection was persisted to disk")
	}
	if _, ok := restored.FindByKey(keep.Key()); !ok {
		t.Fatal("unrelated candidate was lost across the retried begin()")
	}
}

func TestCandidateCatalogRemoveKeysDoesNotHoldPublicationLocksDuringPersistence(t *testing.T) {
	dir := t.TempDir()
	catalog := &CandidateCatalog{}
	catalog.SetDiskCache(newCandidateCatalogCache(dir))
	remove := Proxy{IP: "8.8.8.83", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	keep := Proxy{IP: "8.8.4.53", Port: "8080", Protocol: "http", SourceName: "feed"}
	refresh := catalog.begin([]Proxy{remove, keep}, nil, nil, 0)
	catalog.complete(refresh, nil, nil, nil)
	current := catalog.snapshot.Load()
	_, lease, leased := catalog.leaseByKey(remove.Key())
	if !leased {
		t.Fatal("failed to acquire pre-removal promotion lease")
	}

	persistLocked := make(chan struct{})
	releasePersist := make(chan struct{})
	catalog.persistLocked = func() {
		close(persistLocked)
		<-releasePersist
	}
	result := make(chan error, 1)
	go func() {
		_, _, err := catalog.RemoveKeys([]string{remove.Key()})
		result <- err
	}()
	<-persistLocked

	snapshotUnlocked := current.mu.TryLock()
	if snapshotUnlocked {
		current.mu.Unlock()
	}
	publicationUnlocked := catalog.publicationMu.TryRLock()
	if publicationUnlocked {
		catalog.publicationMu.RUnlock()
	}
	if catalog.withPromotionLease(lease, func(Proxy) bool { return true }) {
		t.Fatal("pending removal allowed promotion during disk persistence")
	}
	close(releasePersist)
	if err := <-result; err != nil {
		t.Fatalf("RemoveKeys returned %v", err)
	}
	if !snapshotUnlocked || !publicationUnlocked {
		t.Fatalf("persistence held catalog locks: snapshot unlocked=%v publication unlocked=%v", snapshotUnlocked, publicationUnlocked)
	}
}

func TestCandidateCatalogRemoveKeysRestoresCacheAfterUnpublishedStaleWrites(t *testing.T) {
	dir := t.TempDir()
	catalog := &CandidateCatalog{}
	catalog.SetDiskCache(newCandidateCatalogCache(dir))
	remove := Proxy{IP: "8.8.8.84", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	keep := Proxy{IP: "8.8.4.54", Port: "8080", Protocol: "http", SourceName: "feed"}
	refresh := catalog.begin([]Proxy{remove, keep}, nil, nil, 0)
	catalog.complete(refresh, nil, nil, nil)

	var hookCalls int
	catalog.removeAfterPersist = func() {
		hookCalls++
		catalog.publicationMu.Lock()
		live := catalog.snapshot.Load()
		live.mu.Lock()
		live.revision++
		live.mu.Unlock()
		catalog.publicationMu.Unlock()
	}
	removed, notFound, err := catalog.RemoveKeys([]string{remove.Key()})
	if err == nil || len(removed) != 0 || len(notFound) != 0 {
		t.Fatalf("contended RemoveKeys result removed=%v notFound=%v err=%v", removed, notFound, err)
	}
	if hookCalls != candidateRemovalMaxAttempts {
		t.Fatalf("post-persist conflicts = %d, want %d", hookCalls, candidateRemovalMaxAttempts)
	}
	if _, ok := catalog.FindByKey(remove.Key()); !ok {
		t.Fatal("unpublished removal changed the live catalog")
	}

	restored := &CandidateCatalog{}
	restored.SetDiskCache(newCandidateCatalogCache(dir))
	loaded, loadErr := restored.LoadDiskCache()
	if loadErr != nil || !loaded {
		t.Fatalf("LoadDiskCache = %v, %v", loaded, loadErr)
	}
	if _, ok := restored.FindByKey(remove.Key()); !ok {
		t.Fatal("restart observed an uncommitted stale deletion")
	}
}

func TestCandidateCatalogCompleteRemoveAndCacheConcurrent(t *testing.T) {
	for attempt := 0; attempt < 25; attempt++ {
		dir := t.TempDir()
		catalog := &CandidateCatalog{}
		catalog.SetDiskCache(newCandidateCatalogCache(dir))
		remove := Proxy{IP: "8.8.8.85", Port: "1080", Protocol: "socks5", SourceName: "feed"}
		keep := Proxy{IP: "8.8.4.55", Port: "8080", Protocol: "http", SourceName: "feed"}
		refresh := catalog.begin([]Proxy{remove, keep}, nil, nil, 0)
		current := catalog.snapshot.Load()

		start := make(chan struct{})
		errs := make(chan error, 2)
		var workers sync.WaitGroup
		workers.Add(3)
		go func() {
			defer workers.Done()
			<-start
			catalog.complete(refresh, []Proxy{keep}, nil, nil)
		}()
		go func() {
			defer workers.Done()
			<-start
			removed, notFound, err := catalog.RemoveKeys([]string{remove.Key()})
			if err != nil || len(removed) != 1 || len(notFound) != 0 {
				errs <- fmt.Errorf("RemoveKeys removed=%v notFound=%v err=%v", removed, notFound, err)
			}
		}()
		go func() {
			defer workers.Done()
			<-start
			if err := catalog.persistCompletedSnapshot(current); err != nil {
				errs <- fmt.Errorf("persist current snapshot: %w", err)
			}
		}()
		close(start)
		done := make(chan struct{})
		go func() {
			workers.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("complete/remove/cache concurrency deadlocked")
		}
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		if _, ok := catalog.FindByKey(remove.Key()); ok {
			t.Fatal("concurrent completion revived removed candidate")
		}

		restored := &CandidateCatalog{}
		restored.SetDiskCache(newCandidateCatalogCache(dir))
		loaded, err := restored.LoadDiskCache()
		if err != nil || !loaded {
			t.Fatalf("LoadDiskCache = %v, %v", loaded, err)
		}
		if _, ok := restored.FindByKey(remove.Key()); ok {
			t.Fatal("cache retained concurrently removed candidate")
		}
	}
}

func TestCandidateCatalogRemoveKeysDuringCheckingPersistsWithoutEndingRefresh(t *testing.T) {
	dir := t.TempDir()
	catalog := &CandidateCatalog{}
	catalog.SetDiskCache(newCandidateCatalogCache(dir))
	remove := Proxy{IP: "8.8.8.82", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	keep := Proxy{IP: "8.8.4.52", Port: "8080", Protocol: "http", SourceName: "feed"}
	refresh := catalog.begin([]Proxy{remove, keep}, nil, nil, 0)

	removed, notFound, err := catalog.RemoveKeys([]string{remove.Key()})
	if err != nil || len(removed) != 1 || removed[0] != remove.Key() || len(notFound) != 0 {
		t.Fatalf("checking RemoveKeys result removed=%v notFound=%v err=%v", removed, notFound, err)
	}
	live := catalog.snapshot.Load()
	live.mu.RLock()
	phase := live.phase
	live.mu.RUnlock()
	if phase != "checking" {
		t.Fatalf("live removal ended refresh phase: %q", phase)
	}
	if _, ok := catalog.FindByKey(remove.Key()); ok {
		t.Fatal("removed checking candidate remained live")
	}

	restored := &CandidateCatalog{}
	restored.SetDiskCache(newCandidateCatalogCache(dir))
	loaded, err := restored.LoadDiskCache()
	if err != nil || !loaded {
		t.Fatalf("checking removal LoadDiskCache = %v, %v", loaded, err)
	}
	if _, ok := restored.FindByKey(remove.Key()); ok {
		t.Fatal("checking removal was not durable")
	}
	persisted := restored.snapshot.Load()
	persisted.mu.RLock()
	persistedPhase := persisted.phase
	persisted.mu.RUnlock()
	if persistedPhase != "complete" {
		t.Fatalf("persisted checking removal phase = %q, want complete", persistedPhase)
	}

	catalog.complete(refresh, []Proxy{keep}, nil, nil)
	completed := catalog.snapshot.Load()
	completed.mu.RLock()
	completedPhase := completed.phase
	completed.mu.RUnlock()
	if completedPhase != "complete" {
		t.Fatalf("refresh completion after removal phase = %q", completedPhase)
	}
	if _, ok := catalog.FindByKey(remove.Key()); ok {
		t.Fatal("refresh completion revived removed candidate")
	}
}

func TestCandidateCatalogResetHealthOutcomesRetainsFailuresAndResources(t *testing.T) {
	failed := Proxy{IP: "192.0.2.51", Port: "8080", Protocol: "http", SourceName: "feed"}
	policy := Proxy{IP: "192.0.2.52", Port: "1080", Protocol: "socks5", SourceName: "feed"}
	resource := Proxy{IP: "198.51.100.52", Port: "443", Protocol: "proxyip", SourceName: "resource"}
	pool := NewProxyPool()
	refresh := pool.candidates.begin([]Proxy{failed, policy, resource}, nil, nil, 0)
	pool.candidates.complete(refresh, []Proxy{failed, policy}, nil, map[string]bool{policy.Key(): true})

	if reset := pool.candidates.ResetHealthOutcomes(); reset != 0 {
		t.Fatalf("reset=%d, want no failure reset", reset)
	}
	snapshot := pool.candidates.snapshot.Load()
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if len(snapshot.records) != 1 || snapshot.protocols[snapshot.records[0].protocolID] != "proxyip" || len(snapshot.failedRecords) != 2 {
		t.Fatalf("reset ownership = records %#v failures %#v", snapshot.records, snapshot.failedRecords)
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

func TestCandidateCatalogPartialMergeRetainsAlternateCredentials(t *testing.T) {
	labels := map[string]string{"old-id": "old", "new-id": "new"}
	old := candidateFromSource("192.0.2.54", "old-id", "old")
	old.CredentialAlternates = []ProxyCredential{{Username: "old-alt", Password: "secret"}}
	current := candidateFromSource("192.0.2.54", "new-id", "new")

	catalog := &CandidateCatalog{}
	first := catalog.begin([]Proxy{old}, labels, nil, 0)
	catalog.complete(first, nil, nil, nil)
	catalog.begin([]Proxy{current}, labels, map[string]bool{"old-id": true}, 1)

	got, ok := catalog.FindByKey(old.Key())
	if !ok || !reflect.DeepEqual(got.CredentialAlternates, old.CredentialAlternates) {
		t.Fatalf("partial merge alternates = %#v, want %#v", got.CredentialAlternates, old.CredentialAlternates)
	}
	if snapshot := catalog.snapshot.Load(); !snapshot.records[0].hasAuth {
		t.Fatal("partial merge lost alternate credential auth marker")
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

func TestCandidateCatalogRetainsUntouchedSourcesWithoutReportingErrors(t *testing.T) {
	pool := NewProxyPool()
	oldA := Proxy{IP: "192.0.2.13", Port: "80", Protocol: "http", SourceName: "source-a"}
	oldB := Proxy{IP: "192.0.2.14", Port: "80", Protocol: "http", SourceName: "source-b"}
	first := pool.candidates.begin([]Proxy{oldA, oldB}, nil, nil, 0)
	pool.candidates.complete(first, nil, nil, nil)

	newA := Proxy{IP: "192.0.2.15", Port: "80", Protocol: "http", SourceName: "source-a"}
	refresh := pool.candidates.begin(
		[]Proxy{newA},
		nil,
		map[string]bool{legacySourceKey("source-b"): true},
		0,
	)
	pool.candidates.complete(refresh, nil, nil, nil)

	page, _ := getCandidatePage(t, NewStatusServer(pool, &ConfigStore{}).handler(), "/api/candidates/page?page_size=100")
	if page.Phase != "complete" || page.SourceErrors != 0 {
		t.Fatalf("single-source refresh phase=%q errors=%d, want complete/0", page.Phase, page.SourceErrors)
	}
	keys := make(map[string]bool, len(page.Candidates))
	for _, candidate := range page.Candidates {
		keys[candidate.Key] = true
	}
	if keys[oldA.Key()] || !keys[newA.Key()] || !keys[oldB.Key()] {
		t.Fatalf("single-source refresh keys=%v; want new A and retained B", keys)
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
	sharedIndex := before.findFailed(shared.Protocol, shared.Addr())
	if sharedIndex < 0 {
		t.Fatalf("shared failure missing from initial snapshot: %#v", before.failedRecords)
	}
	wantCheckedAt := before.failedRecords[sharedIndex].checkedUnix
	if before.failedRecords[sharedIndex].kind != candidateFailureUnreachable || wantCheckedAt == 0 {
		t.Fatalf("initial shared outcome = %#v", before.failedRecords[sharedIndex])
	}

	second := pool.candidates.begin([]Proxy{shared, added}, labels, nil, 0)
	after := pool.candidates.snapshot.Load()
	if len(after.records) != 1 || after.find(removed.Protocol, removed.Addr()) >= 0 || after.find(added.Protocol, added.Addr()) < 0 {
		t.Fatalf("full replacement pending inventory = %#v", after.records)
	}
	sharedIndex = after.findFailed(shared.Protocol, shared.Addr())
	if sharedIndex < 0 {
		t.Fatalf("rediscovered shared failure was requeued: pending=%#v failed=%#v", after.records, after.failedRecords)
	}
	if got := after.failedRecords[sharedIndex]; got.kind != candidateFailureUnreachable || got.checkedUnix != wantCheckedAt {
		t.Fatalf("intersection failure history = %#v, want unreachable at %d", got, wantCheckedAt)
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
	if got := partialPage.Candidates[0]; !got.HasAuth || got.Country != "JP" || got.ProxyURL != old.ConsumerURL() || got.Username != old.Username || got.Password != old.Password || !strings.Contains(raw, "secret") {
		t.Fatalf("failed-attribution fallback = %#v raw=%s", got, raw)
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
	if overlaid.SnapshotID == completed.SnapshotID || overlaid.CandidateTotal != 0 || len(overlaid.Candidates) != 0 {
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

func TestCandidateRecordStaysBounded(t *testing.T) {
	if size := unsafe.Sizeof(candidateRecord{}); size > 96 {
		t.Fatalf("candidateRecord is %d bytes; credential-bearing catalog exceeded its memory budget", size)
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
