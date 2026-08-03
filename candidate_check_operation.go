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
		Kind: kind, Status: "queued", RequestedAt: now.Format(time.RFC3339Nano),
	}
	if c.shuttingDown.Load() {
		operation.Status = "cancelled"
		operation.CompletedAt = now.Format(time.RFC3339Nano)
		operation.Error = "application is shutting down"
		c.candidateCheckLast = cloneCandidateCheckOperation(operation)
		return *operation, &candidateCheckShutdownError{Operation: *operation}
	}

	c.candidateCheckPending = operation
	c.candidateCheckRequest = &candidateCheckRequest{kind: kind, limit: limit, keys: append([]string(nil), keys...)}
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
			coordinator.sourceLifecycleMu.RUnlock()
			finishErr = fmt.Errorf("failed candidate keys are no longer available: %v", missing)
			return
		}
	} else {
		known, _ := pool.candidateKnownSnapshot()
		leases = pool.candidates.LeasePending(request.limit, known)
	}
	coordinator.sourceLifecycleMu.RUnlock()
	defer pool.candidates.ReleaseLeases(leases)
	coordinator.setCandidateCheckTotal(operation.ID, len(leases))

	if len(leases) == 0 {
		status = "complete"
		return
	}

	healthContext, finishHealthWork, current := pool.BeginHealthWork(healthGeneration)
	if !current {
		status, finishErr = "superseded", fmt.Errorf("health criterion changed before candidate check")
		return
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
		coordinator.recordCandidateCheckOutcome(operation.ID, outcome)
	})

	if err := parent.Err(); err != nil {
		status, finishErr = "cancelled", err
		return
	}
	if workContext.Err() != nil || pool.HealthGeneration() != healthGeneration {
		status, finishErr = "superseded", fmt.Errorf("health criterion changed during candidate check")
		return
	}

	coordinator.sourceLifecycleMu.Lock()
	defer coordinator.sourceLifecycleMu.Unlock()
	if !sameSourceRevisions(sourceSnapshot, store.Sources()) {
		status, finishErr = "superseded", fmt.Errorf("source configuration changed during candidate check")
		return
	}
	alive, unreachable, policyFiltered := splitCandidateCheckOutcomes(outcomes, leases)
	if !pool.UpdateWithEnabledSourcesAndPolicy(alive, unreachable, policyFiltered, store.Sources(), healthGeneration) {
		status, finishErr = "superseded", fmt.Errorf("health criterion changed during candidate publication")
		return
	}
	if err := pool.candidates.CommitLeaseOutcomes(leases, outcomes); err != nil {
		finishErr = err
		return
	}
	leases = nil
	if err := pool.FlushCache(); err != nil {
		finishErr = fmt.Errorf("persist candidate check pool results: %w", err)
		return
	}
	status = "complete"
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
