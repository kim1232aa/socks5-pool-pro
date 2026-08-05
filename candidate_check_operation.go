package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	candidateCheckOperationCandidateBatch = "candidate_batch"
	candidateCheckOperationFailedRetry    = "failed_retry"
)

type CandidateCheckOperation struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Status         string `json:"status"`
	RequestedAt    string `json:"requested_at"`
	StartedAt      string `json:"started_at,omitempty"`
	CompletedAt    string `json:"completed_at,omitempty"`
	Total          int    `json:"total"`
	Completed      int    `json:"completed"`
	Alive          int    `json:"alive"`
	Failed         int    `json:"failed"`
	PolicyFiltered int    `json:"policy_filtered"`
	Error          string `json:"error,omitempty"`
}

type candidateCheckRequest struct {
	kind  string
	limit int
	keys  []string
	// retryAll walks the full administrator-supplied failed key list in
	// max-candidates-sized chunks within one operation. tolerateMissing lets
	// those chunks skip keys that stopped being failed between enumeration
	// and lease instead of failing the whole operation.
	retryAll        bool
	tolerateMissing bool
}

type candidateCheckBusyError struct {
	Operation CandidateCheckOperation
}

func (e *candidateCheckBusyError) Error() string {
	return fmt.Sprintf("manual candidate operation %s is already %s", e.Operation.ID, e.Operation.Status)
}

type candidateCheckShutdownError struct {
	Operation CandidateCheckOperation
}

func (e *candidateCheckShutdownError) Error() string {
	return "application is shutting down"
}

func (c *RefreshCoordinator) requestCandidateCheck(kind string, limit int, keys []string) (CandidateCheckOperation, error) {
	return c.enqueueCandidateCheck(&candidateCheckRequest{kind: kind, limit: limit, keys: append([]string(nil), keys...)})
}

// requestFailedRetryAll queues the administrator-triggered walk over every
// failed candidate. The key list is enumerated server-side; the worker
// processes it in max-candidates-sized chunks inside this single operation.
func (c *RefreshCoordinator) requestFailedRetryAll(keys []string) (CandidateCheckOperation, error) {
	return c.enqueueCandidateCheck(&candidateCheckRequest{
		kind: candidateCheckOperationFailedRetry, keys: append([]string(nil), keys...),
		retryAll: true, tolerateMissing: true,
	})
}

// candidateCheckWorkerBusy reports whether the shared manual candidate slot
// is occupied, so the automatic rotation worker can yield instead of racing
// an administrator operation for health-check resources.
func (c *RefreshCoordinator) candidateCheckWorkerBusy() bool {
	c.candidateCheckMu.Lock()
	defer c.candidateCheckMu.Unlock()
	return c.candidateCheckPending != nil || c.candidateCheckActive != nil
}

func (c *RefreshCoordinator) enqueueCandidateCheck(request *candidateCheckRequest) (CandidateCheckOperation, error) {
	c.lifecycleMu.RLock()
	defer c.lifecycleMu.RUnlock()
	c.candidateCheckMu.Lock()
	defer c.candidateCheckMu.Unlock()

	if c.candidateCheckPending != nil {
		return *c.candidateCheckPending, &candidateCheckBusyError{Operation: *c.candidateCheckPending}
	}
	if c.candidateCheckActive != nil {
		return *c.candidateCheckActive, &candidateCheckBusyError{Operation: *c.candidateCheckActive}
	}

	c.candidateCheckSeq++
	now := time.Now().UTC()
	operation := &CandidateCheckOperation{
		ID:   fmt.Sprintf("candidate-check-%d-%d", now.UnixNano(), c.candidateCheckSeq),
		Kind: request.kind, Status: "queued", RequestedAt: now.Format(time.RFC3339Nano),
	}
	if c.shuttingDown.Load() {
		operation.Status = "cancelled"
		operation.CompletedAt = now.Format(time.RFC3339Nano)
		operation.Error = "application is shutting down"
		c.candidateCheckLast = cloneCandidateCheckOperation(operation)
		return *operation, &candidateCheckShutdownError{Operation: *operation}
	}

	c.candidateCheckPending = operation
	c.candidateCheckRequest = request
	select {
	case c.candidateCheckChan <- struct{}{}:
		return *operation, nil
	default:
		c.candidateCheckPending = nil
		c.candidateCheckRequest = nil
		operation.Status = "failed"
		operation.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
		operation.Error = "candidate check worker is unavailable"
		c.candidateCheckLast = cloneCandidateCheckOperation(operation)
		return *operation, fmt.Errorf("candidate check worker is unavailable")
	}
}

func (c *RefreshCoordinator) beginCandidateCheckOperation() (CandidateCheckOperation, candidateCheckRequest, bool) {
	c.candidateCheckMu.Lock()
	defer c.candidateCheckMu.Unlock()
	if c.shuttingDown.Load() || c.candidateCheckActive != nil || c.candidateCheckPending == nil || c.candidateCheckRequest == nil {
		return CandidateCheckOperation{}, candidateCheckRequest{}, false
	}
	operation := c.candidateCheckPending
	request := *c.candidateCheckRequest
	request.keys = append([]string(nil), request.keys...)
	c.candidateCheckPending = nil
	c.candidateCheckRequest = nil
	operation.Status = "running"
	operation.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	c.candidateCheckActive = operation
	return *operation, request, true
}

func (c *RefreshCoordinator) setCandidateCheckTotal(id string, total int) {
	c.candidateCheckMu.Lock()
	defer c.candidateCheckMu.Unlock()
	if c.candidateCheckActive != nil && c.candidateCheckActive.ID == id {
		c.candidateCheckActive.Total = total
	}
}

// dropCandidateCheckTotal shrinks a running operation's denominator when keys
// enumerated up front turn out to be unleasable. Retry-all sets Total from the
// administrator-enumerated failure list; keys deleted or re-tested before their
// chunk leases never produce an outcome, so without this the progress bar would
// stall short of 100% on an operation that actually finished.
func (c *RefreshCoordinator) dropCandidateCheckTotal(id string, dropped int) {
	if dropped <= 0 {
		return
	}
	c.candidateCheckMu.Lock()
	defer c.candidateCheckMu.Unlock()
	if c.candidateCheckActive == nil || c.candidateCheckActive.ID != id {
		return
	}
	c.candidateCheckActive.Total -= dropped
	if c.candidateCheckActive.Total < c.candidateCheckActive.Completed {
		c.candidateCheckActive.Total = c.candidateCheckActive.Completed
	}
}

func (c *RefreshCoordinator) recordCandidateCheckOutcome(id string, outcome candidateCheckOutcome) {
	if outcome.Kind == candidateCheckNoResult {
		return
	}
	c.candidateCheckMu.Lock()
	defer c.candidateCheckMu.Unlock()
	if c.candidateCheckActive == nil || c.candidateCheckActive.ID != id {
		return
	}
	c.candidateCheckActive.Completed++
	switch outcome.Kind {
	case candidateCheckAlive:
		c.candidateCheckActive.Alive++
	case candidateCheckUnreachable:
		c.candidateCheckActive.Failed++
	case candidateCheckPolicyFiltered:
		c.candidateCheckActive.PolicyFiltered++
	}
}

func (c *RefreshCoordinator) finishCandidateCheckOperation(id, status string, err error) {
	c.candidateCheckMu.Lock()
	defer c.candidateCheckMu.Unlock()
	if c.candidateCheckActive == nil || c.candidateCheckActive.ID != id {
		return
	}
	if status == "" {
		status = "complete"
	}
	c.candidateCheckActive.Status = status
	c.candidateCheckActive.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err != nil {
		c.candidateCheckActive.Error = sanitizeCandidateFailureError(err.Error())
	}
	c.candidateCheckLast = cloneCandidateCheckOperation(c.candidateCheckActive)
	c.candidateCheckActive = nil
	c.candidateCheckCancel = nil
}

// cancelCandidateCheck aborts a queued or running candidate operation. A
// queued task is discarded immediately. A running task is interrupted via
// the cancellation context the worker registered; the cycle exits as
// "cancelled" and all leases are released. Returns ok=false when there is
// nothing to cancel (idle).
func (c *RefreshCoordinator) cancelCandidateCheck() (CandidateCheckOperation, bool) {
	c.candidateCheckMu.Lock()
	defer c.candidateCheckMu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if c.candidateCheckPending != nil {
		op := c.candidateCheckPending
		op.Status = "cancelled"
		op.CompletedAt = now
		c.candidateCheckPending = nil
		c.candidateCheckRequest = nil
		c.candidateCheckLast = cloneCandidateCheckOperation(op)
		// Drain the channel so the worker does not wake to an empty pending slot.
		select {
		case <-c.candidateCheckChan:
		default:
		}
		return *op, true
	}
	if c.candidateCheckActive != nil {
		op := c.candidateCheckActive
		if c.candidateCheckCancel != nil {
			c.candidateCheckCancel()
			c.candidateCheckCancel = nil
		}
		return *op, true
	}
	return CandidateCheckOperation{Status: "idle"}, false
}

// registerCandidateCheckCancel stores the cancel function so that an
// administrator can interrupt the running operation before it completes.
// The cancel is cleared automatically when finishCandidateCheckOperation runs.
func (c *RefreshCoordinator) registerCandidateCheckCancel(id string, cancel func()) {
	c.candidateCheckMu.Lock()
	defer c.candidateCheckMu.Unlock()
	if c.candidateCheckActive != nil && c.candidateCheckActive.ID == id {
		c.candidateCheckCancel = cancel
	}
}

func (c *RefreshCoordinator) candidateCheckOperationStatus() CandidateCheckOperation {
	c.candidateCheckMu.RLock()
	defer c.candidateCheckMu.RUnlock()
	if c.candidateCheckActive != nil {
		return *c.candidateCheckActive
	}
	if c.candidateCheckPending != nil {
		return *c.candidateCheckPending
	}
	if c.candidateCheckLast != nil {
		return *c.candidateCheckLast
	}
	return CandidateCheckOperation{Status: "idle"}
}

func cloneCandidateCheckOperation(operation *CandidateCheckOperation) *CandidateCheckOperation {
	if operation == nil {
		return nil
	}
	copy := *operation
	return &copy
}

func runCandidateCheckWorker(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-coordinator.candidateCheckChan:
			runCandidateCheckCycle(ctx, cfg, store, pool, coordinator)
		}
	}
}

func runCandidateCheckCycle(parent context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) {
	operation, request, claimed := coordinator.beginCandidateCheckOperation()
	if !claimed {
		return
	}
	status := "failed"
	var finishErr error
	defer func() { coordinator.finishCandidateCheckOperation(operation.ID, status, finishErr) }()

	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		status, finishErr = "cancelled", err
		return
	}
	if cfg == nil || store == nil || pool == nil {
		finishErr = fmt.Errorf("candidate check runtime is not initialized")
		return
	}

	// Long retry-all walks can run for an hour; an administrator must be able
	// to abort without restarting the process. The cancel is registered on the
	// active operation and cleared when it finishes.
	taskContext, cancelTask := context.WithCancel(parent)
	defer cancelTask()
	coordinator.registerCandidateCheckCancel(operation.ID, cancelTask)

	if !request.retryAll {
		status, finishErr = runSingleCandidateBatch(taskContext, cfg, store, pool, coordinator, operation.ID, request, nil)
		return
	}

	// Retry-all walks the full administrator-enumerated failure list in
	// max-candidates-sized chunks inside this one operation. Each chunk is an
	// independent lease/check/commit unit so a criterion or source change
	// between chunks stops the walk cleanly instead of corrupting a batch.
	coordinator.setCandidateCheckTotal(operation.ID, len(request.keys))
	chunkSize := store.MaxCandidates(cfg.MaxCandidates)
	if chunkSize < 1 {
		chunkSize = 1
	}
	chunk := candidateCheckRequest{
		kind: candidateCheckOperationFailedRetry, retryAll: true, tolerateMissing: true,
	}
	for start := 0; start < len(request.keys); start += chunkSize {
		if err := taskContext.Err(); err != nil {
			status, finishErr = "cancelled", err
			return
		}
		end := start + chunkSize
		if end > len(request.keys) {
			end = len(request.keys)
		}
		chunk.keys = request.keys[start:end]
		status, finishErr = runSingleCandidateBatch(taskContext, cfg, store, pool, coordinator, operation.ID, chunk, nil)
		if status != "complete" {
			if status == "failed" && finishErr == nil {
				finishErr = fmt.Errorf("failed candidate retry-all stopped")
			}
			return
		}
	}
	status = "complete"
}

// runSingleCandidateBatch leases, health-checks and publishes one bounded
// candidate batch. It is shared by the manual candidate/failed-retry tasks,
// each retry-all chunk, and the automatic rotation worker. The returned
// status follows the operation vocabulary: complete, failed, cancelled or
// superseded. observe, when non-nil, receives every outcome in addition to
// the manual-operation progress tracking.
func runSingleCandidateBatch(parent context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator, operationID string, request candidateCheckRequest, observe func(candidateCheckOutcome)) (string, error) {
	if pool.candidates.snapshotPhaseRestored() {
		return "superseded", fmt.Errorf("candidate catalog is still the startup cache restore; waiting for the first refresh")
	}
	coordinator.healthCycleMu.Lock()
	defer coordinator.healthCycleMu.Unlock()

	coordinator.sourceLifecycleMu.RLock()
	sourceSnapshot := store.Sources()
	healthGeneration, checkURL, requireIPChange := pool.HealthCriterionAndPolicy()
	if checkURL == "" {
		pool.SetHealthCriterion(store.CheckURL())
		healthGeneration, checkURL, requireIPChange = pool.HealthCriterionAndPolicy()
	}
	var leases []CandidateLease
	if request.kind == candidateCheckOperationFailedRetry {
		var missing []string
		leases, missing = pool.candidates.LeaseFailed(request.keys)
		if len(missing) > 0 {
			if !request.tolerateMissing {
				coordinator.sourceLifecycleMu.RUnlock()
				return "failed", fmt.Errorf("failed candidate keys are no longer available: %v", missing)
			}
			// Retry-all enumerates the catalog up front; keys can be
			// re-tested or deleted before their chunk leases. Drop them and
			// lease the remainder instead of failing the whole walk.
			drop := make(map[string]bool, len(missing))
			for _, key := range missing {
				drop[key] = true
			}
			remaining := make([]string, 0, len(request.keys)-len(missing))
			for _, key := range request.keys {
				if !drop[key] {
					remaining = append(remaining, key)
				}
			}
			leases, missing = pool.candidates.LeaseFailed(remaining)
			if len(missing) > 0 {
				coordinator.sourceLifecycleMu.RUnlock()
				return "failed", fmt.Errorf("failed candidate keys are no longer available: %v", missing)
			}
			// Retry-all already counted these keys in Total up front. They will
			// never report an outcome, so take them back out of the denominator.
			if request.retryAll {
				coordinator.dropCandidateCheckTotal(operationID, len(request.keys)-len(leases))
			}
		}
	} else {
		known, _ := pool.candidateKnownSnapshot()
		leases = pool.candidates.LeasePending(request.limit, known)
	}
	coordinator.sourceLifecycleMu.RUnlock()
	defer pool.candidates.ReleaseLeases(leases)
	// Retry-all sets its own total up front; per-chunk resets would shrink
	// the progress bar back to one chunk.
	if !request.retryAll {
		coordinator.setCandidateCheckTotal(operationID, len(leases))
	}

	if len(leases) == 0 {
		return "complete", nil
	}

	healthContext, finishHealthWork, current := pool.BeginHealthWork(healthGeneration)
	if !current {
		return "superseded", fmt.Errorf("health criterion changed before candidate check")
	}
	defer finishHealthWork()
	workContext, cancelWork := context.WithCancel(healthContext)
	stopParentCancel := context.AfterFunc(parent, cancelWork)
	defer func() {
		stopParentCancel()
		cancelWork()
	}()

	outcomes := checkCandidateBatchContext(workContext, candidateLeasesToProxies(leases), candidateCheckOptions{
		Timeout: store.CheckTimeout(cfg.CheckTimeout), MaxConcurrent: store.MaxConcurrent(cfg.MaxConcurrent),
		RequireIPChange: requireIPChange, TestURL: checkURL, BaselineIP: BaselineExitIP(),
	}, func(outcome candidateCheckOutcome) {
		coordinator.recordCandidateCheckOutcome(operationID, outcome)
		if observe != nil {
			observe(outcome)
		}
	})

	if err := parent.Err(); err != nil {
		return "cancelled", err
	}
	if workContext.Err() != nil || pool.HealthGeneration() != healthGeneration {
		return "superseded", fmt.Errorf("health criterion changed during candidate check")
	}

	coordinator.sourceLifecycleMu.Lock()
	defer coordinator.sourceLifecycleMu.Unlock()
	if !sameSourceRevisions(sourceSnapshot, store.Sources()) {
		return "superseded", fmt.Errorf("source configuration changed during candidate check")
	}
	alive, unreachable, policyFiltered := splitCandidateCheckOutcomes(outcomes, leases)
	if !pool.UpdateWithEnabledSourcesAndPolicy(alive, unreachable, policyFiltered, store.Sources(), healthGeneration) {
		return "superseded", fmt.Errorf("health criterion changed during candidate publication")
	}
	if err := pool.candidates.CommitLeaseOutcomes(leases, outcomes); err != nil {
		return "failed", err
	}
	leases = nil
	if err := pool.FlushCache(); err != nil {
		return "failed", fmt.Errorf("persist candidate check pool results: %w", err)
	}
	return "complete", nil
}

func sameSourceRevisions(before, after []Source) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		if before[i] != after[i] {
			return false
		}
	}
	return true
}

func candidateCheckBusyOperation(err error) (CandidateCheckOperation, bool) {
	var busy *candidateCheckBusyError
	if errors.As(err, &busy) {
		return busy.Operation, true
	}
	return CandidateCheckOperation{}, false
}
