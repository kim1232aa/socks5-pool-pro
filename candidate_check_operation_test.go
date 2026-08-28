package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCandidateCheckStatusTracksProgressAndOutcomeCounts(t *testing.T) {
	coordinator := newRefreshCoordinator()
	operation, err := coordinator.requestCandidateCheck(candidateCheckOperationCandidateBatch, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := coordinator.beginCandidateCheckOperation(); !ok {
		t.Fatal("candidate operation did not start")
	}
	coordinator.setCandidateCheckTotal(operation.ID, 3)
	coordinator.recordCandidateCheckOutcome(operation.ID, candidateCheckOutcome{Kind: candidateCheckAlive})
	coordinator.recordCandidateCheckOutcome(operation.ID, candidateCheckOutcome{Kind: candidateCheckUnreachable})
	coordinator.recordCandidateCheckOutcome(operation.ID, candidateCheckOutcome{Kind: candidateCheckPolicyFiltered})

	running := coordinator.candidateCheckOperationStatus()
	if running.Status != "running" || running.Total != 3 || running.Completed != 3 || running.Alive != 1 || running.Failed != 1 || running.PolicyFiltered != 1 {
		t.Fatalf("running candidate operation = %+v", running)
	}
	coordinator.finishCandidateCheckOperation(operation.ID, "complete", nil)
	finished := coordinator.candidateCheckOperationStatus()
	if finished.Status != "complete" || finished.CompletedAt == "" || finished.Completed != 3 {
		t.Fatalf("finished candidate operation = %+v", finished)
	}
}

func TestFailedRetryUsesConfiguredCandidateLimitConcurrencyAndTimeout(t *testing.T) {
	failed := make([]Proxy, 5)
	for i := range failed {
		failed[i] = Proxy{IP: fmt.Sprintf("198.51.100.%d", i+40), Port: "8080", Protocol: "http"}
	}
	pool := candidateOperationTestPool(nil, failed)
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check", MaxConcurrent: 2, CheckTimeoutSeconds: 2}}
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 100}
	coordinator := newRefreshCoordinator()

	var active, maximum, calls atomic.Int64
	installCandidateCheckSeams(t,
		func(ctx context.Context, px Proxy, testURL string, timeout time.Duration) (Proxy, bool, error) {
			if testURL != "https://health.example/check" || timeout != 2*time.Second {
				t.Errorf("check options = %q %s", testURL, timeout)
			}
			calls.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}
			select {
			case <-time.After(15 * time.Millisecond):
				return px, true, nil
			case <-ctx.Done():
				return px, false, ctx.Err()
			}
		},
		func(context.Context, Proxy, time.Duration) string { return "" },
		func(context.Context, string, time.Duration) (string, string, string) { return "", "", "" },
		func(context.Context, Proxy, time.Duration) string { return "" },
	)

	keys := []string{failed[0].Key(), failed[1].Key(), failed[2].Key()}
	operation, err := coordinator.requestCandidateCheck(candidateCheckOperationFailedRetry, 0, keys)
	if err != nil {
		t.Fatal(err)
	}
	<-coordinator.candidateCheckChan
	runCandidateCheckCycle(context.Background(), cfg, store, pool, coordinator)

	finished := coordinator.candidateCheckOperationStatus()
	if finished.ID != operation.ID || finished.Status != "complete" || finished.Total != 3 || finished.Completed != 3 || finished.Alive != 3 {
		t.Fatalf("failed retry operation = %+v", finished)
	}
	if calls.Load() != 3 || maximum.Load() != 2 {
		t.Fatalf("retry calls/concurrency = %d/%d, want 3/2", calls.Load(), maximum.Load())
	}
	page := NewStatusServer(pool, store).buildFailedCandidatePage(localTestRequest(http.MethodGet, "/api/failed-candidates", nil))
	if page.FailedTotal != 2 {
		t.Fatalf("failed total after three successful retries = %d, want 2", page.FailedTotal)
	}
}

func TestFailedRetrySuccessPromotesAndRemovesFailure(t *testing.T) {
	failed := Proxy{IP: "198.51.100.60", Port: "1080", Protocol: "socks5"}
	pool := candidateOperationTestPool(nil, []Proxy{failed})
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check"}}
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	installCandidateCheckSeams(t,
		func(_ context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			return px, true, nil
		},
		func(context.Context, Proxy, time.Duration) string { return "203.0.113.60" }, nil, nil,
	)

	if _, err := coordinator.requestCandidateCheck(candidateCheckOperationFailedRetry, 0, []string{failed.Key()}); err != nil {
		t.Fatal(err)
	}
	<-coordinator.candidateCheckChan
	runCandidateCheckCycle(context.Background(), cfg, store, pool, coordinator)

	if got, ok := pool.Find(failed.Key()); !ok || !got.Available {
		t.Fatalf("successful failed retry was not promoted: %+v, %v", got, ok)
	}
	page := NewStatusServer(pool, store).buildFailedCandidatePage(localTestRequest(http.MethodGet, "/api/failed-candidates", nil))
	if page.FailedTotal != 0 {
		t.Fatalf("successful failed retry remains failed: %+v", page)
	}
}

func TestFailedRetryFailureUpdatesAndRetainsFailure(t *testing.T) {
	failed := Proxy{IP: "198.51.100.61", Port: "8080", Protocol: "http"}
	pool := candidateOperationTestPool(nil, []Proxy{failed})
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check"}}
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	installCandidateCheckSeams(t,
		func(context.Context, Proxy, string, time.Duration) (Proxy, bool, error) {
			return Proxy{}, false, errors.New("retry connection refused\nupstream detail")
		}, nil, nil, nil,
	)

	if _, err := coordinator.requestCandidateCheck(candidateCheckOperationFailedRetry, 0, []string{failed.Key()}); err != nil {
		t.Fatal(err)
	}
	<-coordinator.candidateCheckChan
	runCandidateCheckCycle(context.Background(), cfg, store, pool, coordinator)

	operation := coordinator.candidateCheckOperationStatus()
	if operation.Status != "complete" || operation.Failed != 1 || operation.Completed != 1 {
		t.Fatalf("failed retry operation = %+v", operation)
	}
	page := NewStatusServer(pool, store).buildFailedCandidatePage(localTestRequest(http.MethodGet, "/api/failed-candidates", nil))
	if page.FailedTotal != 0 || page.IsolatedUnreachableTotal != 1 || len(page.FailedCandidates) != 0 {
		t.Fatalf("retryable failure that is now unreachable should be isolated: %+v", page)
	}
	snapshot := pool.candidates.snapshot.Load()
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if len(snapshot.failedRecords) != 1 || snapshot.failedRecords[0].lastError != "retry connection refused upstream detail" {
		t.Fatalf("isolated failure was dropped from catalog: %#v", snapshot.failedRecords)
	}
}

func TestCandidateTaskCancellationReleasesUnfinishedLeases(t *testing.T) {
	pending := []Proxy{
		{IP: "198.51.100.70", Port: "8080", Protocol: "http"},
		{IP: "198.51.100.71", Port: "8080", Protocol: "http"},
	}
	pool := candidateOperationTestPool(pending, nil)
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check", MaxConcurrent: 2}}
	cfg := &Config{CheckTimeout: time.Minute, MaxConcurrent: 2, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	started := make(chan struct{}, len(pending))
	installCandidateCheckSeams(t,
		func(ctx context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			started <- struct{}{}
			<-ctx.Done()
			return px, false, ctx.Err()
		}, nil, nil, nil,
	)

	if _, err := coordinator.requestCandidateCheck(candidateCheckOperationCandidateBatch, len(pending), nil); err != nil {
		t.Fatal(err)
	}
	<-coordinator.candidateCheckChan
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runCandidateCheckCycle(ctx, cfg, store, pool, coordinator)
		close(done)
	}()
	for range pending {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("candidate check did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled candidate operation did not stop")
	}
	if operation := coordinator.candidateCheckOperationStatus(); operation.Status != "cancelled" || operation.Failed != 0 || operation.PolicyFiltered != 0 {
		t.Fatalf("cancelled candidate operation = %+v", operation)
	}
	known, _ := pool.candidateKnownSnapshot()
	leases := pool.candidates.LeasePending(len(pending), known)
	if len(leases) != len(pending) {
		t.Fatalf("pending leases after cancellation = %d, want %d", len(leases), len(pending))
	}
	pool.candidates.ReleaseLeases(leases)
}

func TestCandidateTaskCriterionChangeSupersedesWithoutFailure(t *testing.T) {
	pending := Proxy{IP: "198.51.100.72", Port: "8080", Protocol: "http"}
	pool := candidateOperationTestPool([]Proxy{pending}, nil)
	pool.SetHealthCriterion("https://health.example/first")
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/first"}}
	cfg := &Config{CheckTimeout: time.Minute, MaxConcurrent: 1, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	started := make(chan struct{})
	var once sync.Once
	installCandidateCheckSeams(t,
		func(ctx context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return px, false, ctx.Err()
		}, nil, nil, nil,
	)

	if _, err := coordinator.requestCandidateCheck(candidateCheckOperationCandidateBatch, 1, nil); err != nil {
		t.Fatal(err)
	}
	<-coordinator.candidateCheckChan
	done := make(chan struct{})
	go func() {
		runCandidateCheckCycle(context.Background(), cfg, store, pool, coordinator)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("candidate criterion test did not start")
	}
	pool.InvalidateHealth("https://health.example/second")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("superseded candidate operation did not stop")
	}
	operation := coordinator.candidateCheckOperationStatus()
	if operation.Status != "superseded" || operation.Failed != 0 || operation.PolicyFiltered != 0 {
		t.Fatalf("superseded candidate operation = %+v", operation)
	}
	page := NewStatusServer(pool, store).buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page", nil))
	failedPage := NewStatusServer(pool, store).buildFailedCandidatePage(localTestRequest(http.MethodGet, "/api/failed-candidates", nil))
	if page.CandidateTotal != 1 || failedPage.FailedTotal != 0 {
		t.Fatalf("criterion change mutated ownership: pending=%+v failed=%+v", page, failedPage)
	}
}

func TestCoordinatorShutdownCancelsCandidateOperationAndRejectsNewOne(t *testing.T) {
	coordinator := newRefreshCoordinator()
	queued, err := coordinator.requestCandidateCheck(candidateCheckOperationCandidateBatch, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.shutdown()
	last := coordinator.candidateCheckOperationStatus()
	if last.ID != queued.ID || last.Status != "cancelled" || last.CompletedAt == "" {
		t.Fatalf("candidate status after shutdown = %+v", last)
	}
	operation, err := coordinator.requestCandidateCheck(candidateCheckOperationCandidateBatch, 1, nil)
	var shutdownErr *candidateCheckShutdownError
	if !errors.As(err, &shutdownErr) || operation.Status != "cancelled" {
		t.Fatalf("post-shutdown candidate request = (%+v, %v)", operation, err)
	}
}

func TestFailedRetryAllWalksEveryFailureInChunks(t *testing.T) {
	failed := make([]Proxy, 5)
	for i := range failed {
		failed[i] = Proxy{IP: fmt.Sprintf("198.51.100.%d", i+100), Port: "8080", Protocol: "http"}
	}
	pool := candidateOperationTestPool(nil, failed)
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check"}}
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 2, MaxCandidates: 2}
	coordinator := newRefreshCoordinator()
	var calls atomic.Int64
	installCandidateCheckSeams(t,
		func(_ context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			calls.Add(1)
			if px.IP == failed[0].IP || px.IP == failed[1].IP {
				return Proxy{}, false, errors.New("still down")
			}
			return px, true, nil
		},
		func(context.Context, Proxy, time.Duration) string { return "203.0.113.9" }, nil, nil,
	)

	keys := pool.candidates.FailedKeys()
	if len(keys) != len(failed) {
		t.Fatalf("FailedKeys = %v, want all %d failures", keys, len(failed))
	}
	operation, err := coordinator.requestFailedRetryAll(keys)
	if err != nil {
		t.Fatal(err)
	}
	<-coordinator.candidateCheckChan
	runCandidateCheckCycle(context.Background(), cfg, store, pool, coordinator)

	finished := coordinator.candidateCheckOperationStatus()
	if finished.ID != operation.ID || finished.Status != "complete" || finished.Total != 5 || finished.Completed != 5 || finished.Alive != 3 || finished.Failed != 2 {
		t.Fatalf("retry-all operation = %+v", finished)
	}
	if calls.Load() != int64(len(failed)) {
		t.Fatalf("retry-all checked %d nodes, want every failure exactly once", calls.Load())
	}
	page := NewStatusServer(pool, store).buildFailedCandidatePage(localTestRequest(http.MethodGet, "/api/failed-candidates", nil))
	if page.FailedTotal != 0 || page.IsolatedUnreachableTotal != 2 {
		t.Fatalf("still-down retry-all nodes should be isolated, not listed: %+v", page)
	}
}

func TestFailedRetryAllSkipsKeysNoLongerFailed(t *testing.T) {
	failed := []Proxy{
		{IP: "198.51.100.110", Port: "8080", Protocol: "http"},
		{IP: "198.51.100.111", Port: "8080", Protocol: "http"},
	}
	pool := candidateOperationTestPool(nil, failed)
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check"}}
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	installCandidateCheckSeams(t,
		func(_ context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			return px, true, nil
		},
		func(context.Context, Proxy, time.Duration) string { return "203.0.113.9" }, nil, nil,
	)

	keys := append(pool.candidates.FailedKeys(), "http://203.0.113.250:9999")
	if _, err := coordinator.requestFailedRetryAll(keys); err != nil {
		t.Fatal(err)
	}
	<-coordinator.candidateCheckChan
	runCandidateCheckCycle(context.Background(), cfg, store, pool, coordinator)

	finished := coordinator.candidateCheckOperationStatus()
	if finished.Status != "complete" || finished.Completed != 2 {
		t.Fatalf("retry-all with vanished key = %+v, want complete with 2 checked", finished)
	}
}

// TestCancelQueuedCandidateCheckReleasesSlot covers the administrator
// cancelling a task the worker has not claimed yet: the shared manual slot must
// be free immediately and the worker must not later run the abandoned request.
func TestCancelQueuedCandidateCheckReleasesSlot(t *testing.T) {
	coordinator := newRefreshCoordinator()
	queued, err := coordinator.requestCandidateCheck(candidateCheckOperationCandidateBatch, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, ok := coordinator.cancelCandidateCheck()
	if !ok || cancelled.ID != queued.ID || cancelled.Status != "cancelled" || cancelled.CompletedAt == "" {
		t.Fatalf("cancel queued task = (%+v, %v)", cancelled, ok)
	}
	if status := coordinator.candidateCheckOperationStatus(); status.Status != "cancelled" || status.ID != queued.ID {
		t.Fatalf("status after cancelling queued task = %+v", status)
	}
	if _, _, claimed := coordinator.beginCandidateCheckOperation(); claimed {
		t.Fatal("worker claimed a cancelled request")
	}
	if _, err := coordinator.requestCandidateCheck(candidateCheckOperationCandidateBatch, 1, nil); err != nil {
		t.Fatalf("slot not released after cancelling queued task: %v", err)
	}
}

// TestCancelRunningCandidateCheckStopsWorkAndReleasesLeases proves cancellation
// interrupts in-flight dialing, ends the operation as cancelled, and leaves the
// candidates leasable again.
func TestCancelRunningCandidateCheckStopsWorkAndReleasesLeases(t *testing.T) {
	pending := []Proxy{
		{IP: "198.51.102.10", Port: "8080", Protocol: "http"},
		{IP: "198.51.102.11", Port: "8080", Protocol: "http"},
	}
	pool := candidateOperationTestPool(pending, nil)
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check", MaxConcurrent: 2}}
	cfg := &Config{CheckTimeout: time.Minute, MaxConcurrent: 2, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	started := make(chan struct{}, len(pending))
	installCandidateCheckSeams(t,
		func(ctx context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			started <- struct{}{}
			<-ctx.Done()
			return px, false, ctx.Err()
		}, nil, nil, nil,
	)

	if _, err := coordinator.requestCandidateCheck(candidateCheckOperationCandidateBatch, len(pending), nil); err != nil {
		t.Fatal(err)
	}
	<-coordinator.candidateCheckChan
	done := make(chan struct{})
	go func() {
		runCandidateCheckCycle(context.Background(), cfg, store, pool, coordinator)
		close(done)
	}()
	for range pending {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("candidate check did not start")
		}
	}
	if _, ok := coordinator.cancelCandidateCheck(); !ok {
		t.Fatal("cancelling a running candidate task was rejected")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled candidate task did not stop")
	}
	if status := coordinator.candidateCheckOperationStatus(); status.Status != "cancelled" {
		t.Fatalf("status after cancelling running task = %+v", status)
	}
	known, _ := pool.candidateKnownSnapshot()
	leases := pool.candidates.LeasePending(len(pending), known)
	if len(leases) != len(pending) {
		t.Fatalf("pending leases after cancellation = %d, want %d", len(leases), len(pending))
	}
	pool.candidates.ReleaseLeases(leases)
}

// TestCancelWithoutRunningCandidateCheckIsRejected keeps the endpoint honest:
// cancelling nothing must not fabricate a cancelled operation.
func TestCancelWithoutRunningCandidateCheckIsRejected(t *testing.T) {
	coordinator := newRefreshCoordinator()
	if operation, ok := coordinator.cancelCandidateCheck(); ok || operation.Status != "idle" {
		t.Fatalf("cancel with no task = (%+v, %v)", operation, ok)
	}
}

func TestAutomaticWorkersCannotLeaseFailedCandidates(t *testing.T) {
	failed := Proxy{IP: "198.51.100.80", Port: "8080", Protocol: "http"}
	pool := candidateOperationTestPool(nil, []Proxy{failed})
	known, _ := pool.candidateKnownSnapshot()
	if leases := pool.candidates.LeasePending(10, known); len(leases) != 0 {
		pool.candidates.ReleaseLeases(leases)
		t.Fatalf("automatic pending lease returned failed records: %+v", leases)
	}
}

func candidateOperationTestPool(pending, failed []Proxy) *ProxyPool {
	pool := NewProxyPool()
	all := append(append([]Proxy(nil), pending...), failed...)
	refresh := pool.candidates.begin(all, nil, nil, 0)
	policy := make(map[string]bool, len(failed))
	for _, px := range failed {
		policy[px.Key()] = true
	}
	pool.candidates.complete(refresh, failed, nil, policy)
	return pool
}
