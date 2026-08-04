package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestAutoCandidateCheckConsumesPendingCandidates(t *testing.T) {
	pending := []Proxy{
		{IP: "198.51.101.10", Port: "8080", Protocol: "http"},
		{IP: "198.51.101.11", Port: "8080", Protocol: "http"},
		{IP: "198.51.101.12", Port: "8080", Protocol: "http"},
	}
	pool := candidateOperationTestPool(pending, nil)
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check"}}
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 2, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	installCandidateCheckSeams(t,
		func(_ context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			return px, true, nil
		},
		func(context.Context, Proxy, time.Duration) string { return "203.0.113.9" }, nil, nil,
	)

	ran, checked, status := runOneAutoCandidateCheck(context.Background(), cfg, store, pool, coordinator)
	if !ran || checked != len(pending) || status != "complete" {
		t.Fatalf("auto batch = ran=%v checked=%d status=%s, want complete with %d checked", ran, checked, status, len(pending))
	}
	if pool.Size() != len(pending) {
		t.Fatalf("pool after auto batch = %d, want %d alive nodes promoted", pool.Size(), len(pending))
	}
	known, _ := pool.candidateKnownSnapshot()
	if leases := pool.candidates.LeasePending(10, known); len(leases) != 0 {
		pool.candidates.ReleaseLeases(leases)
		t.Fatalf("pending candidates remained after auto batch: %d", len(leases))
	}
}

func TestAutoCandidateCheckYieldsWhenDisabledOrManualBusy(t *testing.T) {
	pending := Proxy{IP: "198.51.101.20", Port: "8080", Protocol: "http"}
	disabled := false
	pool := candidateOperationTestPool([]Proxy{pending}, nil)
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check", AutoCandidateCheck: &disabled}}
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	installCandidateCheckSeams(t,
		func(_ context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			return px, true, nil
		},
		func(context.Context, Proxy, time.Duration) string { return "" }, nil, nil,
	)

	if ran, checked, status := runOneAutoCandidateCheck(context.Background(), cfg, store, pool, coordinator); ran || checked != 0 || status != "skipped" {
		t.Fatalf("disabled auto check = ran=%v checked=%d status=%s, want skipped", ran, checked, status)
	}
	enabled := true
	store.cfg.AutoCandidateCheck = &enabled

	if _, err := coordinator.requestCandidateCheck(candidateCheckOperationCandidateBatch, 1, nil); err != nil {
		t.Fatal(err)
	}
	if ran, checked, status := runOneAutoCandidateCheck(context.Background(), cfg, store, pool, coordinator); ran || checked != 0 || status != "skipped" {
		t.Fatalf("auto check during busy manual slot = ran=%v checked=%d status=%s, want skipped", ran, checked, status)
	}
}

func restoredPhasePool(pending []Proxy) *ProxyPool {
	pool := NewProxyPool()
	snapshot := buildCandidateSnapshot(pending, nil)
	snapshot.generation = 1
	snapshot.revision = 1
	snapshot.phase = "restored"
	pool.candidates.snapshot.Store(snapshot)
	return pool
}

func TestAutoCandidateCheckWaitsForFirstRefreshAfterRestore(t *testing.T) {
	pool := restoredPhasePool([]Proxy{{IP: "198.51.101.40", Port: "8080", Protocol: "http"}})
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check"}}
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	installCandidateCheckSeams(t,
		func(_ context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			return px, true, nil
		},
		func(context.Context, Proxy, time.Duration) string { return "" }, nil, nil,
	)

	ran, checked, status := runOneAutoCandidateCheck(context.Background(), cfg, store, pool, coordinator)
	if ran || checked != 0 || status != "skipped" {
		t.Fatalf("auto check during restored phase = ran=%v checked=%d status=%s, want skipped", ran, checked, status)
	}
	if len(pool.candidates.pendingLeases) != 0 {
		t.Fatalf("auto check leased candidates during restored phase: %v", pool.candidates.pendingLeases)
	}
}

func TestManualCandidateBatchWaitsForRestorePhase(t *testing.T) {
	pool := restoredPhasePool([]Proxy{{IP: "198.51.101.41", Port: "8080", Protocol: "http"}})
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check"}}
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	installCandidateCheckSeams(t,
		func(_ context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			return px, true, nil
		},
		func(context.Context, Proxy, time.Duration) string { return "" }, nil, nil,
	)

	status, err := runSingleCandidateBatch(context.Background(), cfg, store, pool, coordinator, "op-restored", candidateCheckRequest{kind: candidateCheckOperationCandidateBatch, limit: 5}, nil)
	if status != "superseded" || err == nil {
		t.Fatalf("manual batch during restored phase = %q %v, want superseded with explanation", status, err)
	}
}

func TestAutoCandidateCheckLeavesFailedCandidatesAlone(t *testing.T) {
	failed := Proxy{IP: "198.51.101.30", Port: "8080", Protocol: "http"}
	pool := candidateOperationTestPool(nil, []Proxy{failed})
	store := &ConfigStore{cfg: PoolConfig{CheckURL: "https://health.example/check"}}
	cfg := &Config{CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	installCandidateCheckSeams(t,
		func(_ context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			return px, true, nil
		},
		func(context.Context, Proxy, time.Duration) string { return "" }, nil, nil,
	)

	ran, checked, status := runOneAutoCandidateCheck(context.Background(), cfg, store, pool, coordinator)
	if !ran || checked != 0 || status != "complete" {
		t.Fatalf("auto batch over failed-only catalog = ran=%v checked=%d status=%s", ran, checked, status)
	}
	page := NewStatusServer(pool, store).buildFailedCandidatePage(localTestRequest(http.MethodGet, "/api/failed-candidates", nil))
	if page.FailedTotal != 1 {
		t.Fatalf("auto check changed the failed catalog: %+v", page)
	}
	if pool.Size() != 0 {
		t.Fatalf("auto check promoted a failed candidate into the pool: %d", pool.Size())
	}
}
