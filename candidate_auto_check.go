package main

import (
	"context"
	"log"
	"time"
)

// autoCheckIdleDelay paces the automatic rotation worker whenever a batch
// cannot run: the check is disabled, the shared manual candidate slot is
// busy, a batch failed or was superseded, or no pending candidates remain.
const autoCheckIdleDelay = 15 * time.Second

// runAutoCandidateCheckWorker continuously walks the pending candidate
// catalog in max-candidates-sized rotation batches, independently of the
// source-refresh schedule. The pause between batches is dashboard-tunable;
// zero runs batches back-to-back. The worker only ever leases pending
// records — failed candidates stay manual-only — and every batch goes
// through the shared single-batch path, so the health-cycle lock serializes
// publication with every other health workflow.
func runAutoCandidateCheckWorker(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) {
	for {
		if !sleepContext(ctx, store.AutoCheckInterval()) {
			return
		}
		ran, checked, status := runOneAutoCandidateCheck(ctx, cfg, store, pool, coordinator)
		if !ran || checked == 0 || status != "complete" {
			if !sleepContext(ctx, autoCheckIdleDelay) {
				return
			}
		}
	}
}

// runOneAutoCandidateCheck executes a single automatic rotation batch and
// reports whether a batch ran, how many candidates it checked, and the
// outcome status. It yields (ran=false) whenever the automatic check is
// disabled, the shared manual candidate slot is busy, or the application is
// shutting down, so an administrator operation always keeps priority.
func runOneAutoCandidateCheck(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) (bool, int, string) {
	if ctx.Err() != nil || cfg == nil || store == nil || pool == nil {
		return false, 0, "cancelled"
	}
	if !store.AutoCandidateCheckEnabled() || coordinator.candidateCheckWorkerBusy() || coordinator.shuttingDown.Load() {
		return false, 0, "skipped"
	}
	// The startup cache restore is read-only until the first refresh
	// republishes it; batches wait instead of persisting into it.
	if pool.candidates.snapshotPhaseRestored() {
		return false, 0, "skipped"
	}
	var checked, alive, failed, filtered int
	observe := func(outcome candidateCheckOutcome) {
		checked++
		switch outcome.Kind {
		case candidateCheckAlive:
			alive++
		case candidateCheckUnreachable:
			failed++
		case candidateCheckPolicyFiltered:
			filtered++
		}
	}
	request := candidateCheckRequest{kind: candidateCheckOperationCandidateBatch, limit: store.MaxCandidates(cfg.MaxCandidates)}
	status, err := runSingleCandidateBatch(ctx, cfg, store, pool, coordinator, "", request, observe)
	if status == "complete" {
		if checked > 0 {
			log.Printf("[auto-check] checked %d: alive %d failed %d policy_filtered %d", checked, alive, failed, filtered)
		}
		return true, checked, status
	}
	if err != nil {
		log.Printf("[auto-check] batch %s: %v", status, err)
	}
	return true, checked, status
}

// sleepContext waits for d, returning false when ctx is cancelled first. A
// non-positive duration returns immediately without consulting ctx, letting
// zero-interval rotation run batches back-to-back.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
