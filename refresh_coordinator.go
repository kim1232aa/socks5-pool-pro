package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// RefreshCoordinator owns the mutable state shared by scrape, refresh, and
// health-recheck workers. A coordinator is intentionally independent of a
// ProxyPool so tests and application instances can isolate their work queues.
type RefreshCoordinator struct {
	shuttingDown atomic.Bool
	shutdownOnce sync.Once
	lifecycleMu  sync.RWMutex

	lastScrapeTime time.Time
	nextScrapeTime time.Time
	lastScrapeInfo ScrapeInfo
	scrapeMu       sync.RWMutex

	refreshChan       chan struct{}
	recheckChan       chan struct{}
	fullRecheckChan   chan struct{}
	sourceRefreshChan chan string
	healthCycleMu     sync.Mutex
	// sourceLifecycleMu makes source changes and the final pool install one
	// ordered transaction, preventing stale refresh results from reviving nodes.
	sourceLifecycleMu sync.RWMutex

	// Deterministic test seams around source lifecycle finalization/mutation.
	fullRefreshBeforeFinalValidation   func()
	sourceRefreshBeforeFinalValidation func()
	sourceAutoRefreshBeforeMutation    func()

	refreshOpMu          sync.RWMutex
	refreshOpSeq         uint64
	refreshActive        *RefreshOperation
	refreshPending       *RefreshOperation
	refreshLast          *RefreshOperation
	sourceRefreshMu      sync.Mutex
	sourceRefreshSeq     uint64
	sourceRefreshPending map[string]*SourceRefreshOperation
	sourceRefreshActive  map[string]*SourceRefreshOperation
	sourceRefreshLast    map[string]time.Time

	healthRecheckOpMu    sync.RWMutex
	healthRecheckOpSeq   uint64
	healthRecheckActive  *HealthRecheckOperation
	healthRecheckPending *HealthRecheckOperation
	healthRecheckLast    *HealthRecheckOperation
}

func newRefreshCoordinator() *RefreshCoordinator {
	return &RefreshCoordinator{
		refreshChan:          make(chan struct{}, 1),
		recheckChan:          make(chan struct{}, 1),
		fullRecheckChan:      make(chan struct{}, 1),
		sourceRefreshChan:    make(chan string, maxConfiguredSources),
		sourceRefreshPending: make(map[string]*SourceRefreshOperation),
		sourceRefreshActive:  make(map[string]*SourceRefreshOperation),
		sourceRefreshLast:    make(map[string]time.Time),
	}
}

var defaultRefreshCoordinator = newRefreshCoordinator()

func (c *RefreshCoordinator) scrapeTimes() (last, next time.Time) {
	c.scrapeMu.RLock()
	defer c.scrapeMu.RUnlock()
	return c.lastScrapeTime, c.nextScrapeTime
}

func (c *RefreshCoordinator) scrapeInfo() ScrapeInfo {
	c.scrapeMu.RLock()
	defer c.scrapeMu.RUnlock()
	return c.lastScrapeInfo
}

func (c *RefreshCoordinator) scrapeStatusSnapshot() scrapeStatusSnapshot {
	c.scrapeMu.RLock()
	defer c.scrapeMu.RUnlock()
	return scrapeStatusSnapshot{Last: c.lastScrapeTime, Next: c.nextScrapeTime, Info: c.lastScrapeInfo}
}

func (c *RefreshCoordinator) recordScrape(info ScrapeInfo, interval time.Duration) {
	c.scrapeMu.Lock()
	defer c.scrapeMu.Unlock()
	c.lastScrapeInfo = info
	if info.SourceError < info.SourceTotal {
		c.lastScrapeTime = time.Now()
		c.nextScrapeTime = c.lastScrapeTime.Add(interval)
		return
	}
	retry := interval / 4
	if retry < time.Minute {
		retry = time.Minute
	}
	c.nextScrapeTime = time.Now().Add(retry)
}

func (c *RefreshCoordinator) requestRefresh() (operation RefreshOperation, accepted bool) {
	c.lifecycleMu.RLock()
	defer c.lifecycleMu.RUnlock()
	c.refreshOpMu.Lock()
	if c.shuttingDown.Load() {
		job := c.newRefreshOperationLocked("manual", "cancelled")
		job.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
		job.Error = "application is shutting down"
		operation = *job
		c.refreshOpMu.Unlock()
		return operation, false
	}
	if c.refreshPending != nil {
		operation = *c.refreshPending
		c.refreshOpMu.Unlock()
		return operation, false
	}
	job := c.newRefreshOperationLocked("manual", "queued")
	c.refreshPending = job
	operation = *job
	c.refreshOpMu.Unlock()
	select {
	case c.refreshChan <- struct{}{}:
		return operation, true
	default:
		return operation, false
	}
}

func (c *RefreshCoordinator) triggerRefresh() { _, _ = c.requestRefresh() }

func (c *RefreshCoordinator) beginRefreshOperation() string {
	c.refreshOpMu.Lock()
	defer c.refreshOpMu.Unlock()
	if c.shuttingDown.Load() {
		return ""
	}
	job := c.refreshPending
	c.refreshPending = nil
	if job == nil {
		job = c.newRefreshOperationLocked("manual", "queued")
	}
	job.Status = "running"
	job.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	c.refreshActive = job
	return job.ID
}

func (c *RefreshCoordinator) beginBackgroundRefreshOperation(trigger string) string {
	c.refreshOpMu.Lock()
	defer c.refreshOpMu.Unlock()
	if c.shuttingDown.Load() {
		return ""
	}
	job := c.newRefreshOperationLocked(trigger, "running")
	job.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	c.refreshActive = job
	return job.ID
}

func (c *RefreshCoordinator) newRefreshOperationLocked(trigger, status string) *RefreshOperation {
	c.refreshOpSeq++
	now := time.Now().UTC()
	return &RefreshOperation{ID: fmt.Sprintf("refresh-%d-%d", now.UnixNano(), c.refreshOpSeq), Status: status, Trigger: trigger, RequestedAt: now.Format(time.RFC3339Nano)}
}

func (c *RefreshCoordinator) finishRefreshOperation(id string, result refreshRunResult) {
	c.refreshOpMu.Lock()
	defer c.refreshOpMu.Unlock()
	if c.refreshActive == nil || c.refreshActive.ID != id {
		return
	}
	c.refreshActive.Status = result.Status
	if c.refreshActive.Status == "" {
		c.refreshActive.Status = "complete"
	}
	c.refreshActive.SourceErrors = result.SourceErrors
	c.refreshActive.Error = result.Error
	c.refreshActive.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	completed := *c.refreshActive
	c.refreshLast = &completed
	c.refreshActive = nil
}

func (c *RefreshCoordinator) refreshOperationStatus() RefreshOperationStatus {
	c.refreshOpMu.RLock()
	defer c.refreshOpMu.RUnlock()
	clone := func(operation *RefreshOperation) *RefreshOperation {
		if operation == nil {
			return nil
		}
		copy := *operation
		return &copy
	}
	state := "idle"
	if c.refreshPending != nil {
		state = "queued"
	}
	if c.refreshActive != nil {
		state = "running"
	}
	return RefreshOperationStatus{State: state, Active: clone(c.refreshActive), Pending: clone(c.refreshPending), Last: clone(c.refreshLast)}
}

func (c *RefreshCoordinator) requestSourceRefresh(source Source, trigger string) (SourceRefreshOperation, bool) {
	c.lifecycleMu.RLock()
	defer c.lifecycleMu.RUnlock()
	c.sourceRefreshMu.Lock()
	defer c.sourceRefreshMu.Unlock()
	newOperation := func(status string) *SourceRefreshOperation {
		c.sourceRefreshSeq++
		now := time.Now().UTC()
		return &SourceRefreshOperation{
			ID:             fmt.Sprintf("source-refresh-%d-%d", now.UnixNano(), c.sourceRefreshSeq),
			SourceID:       source.ID,
			SourceName:     source.Name,
			Status:         status,
			Trigger:        trigger,
			RequestedAt:    now.Format(time.RFC3339Nano),
			sourceRevision: newSourceRefreshRevision(source),
		}
	}
	if c.shuttingDown.Load() {
		operation := newOperation("cancelled")
		operation.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
		operation.Error = "application is shutting down"
		return *operation, false
	}
	if !source.Enabled {
		operation := newOperation("rejected")
		operation.Error = "source is disabled"
		return *operation, false
	}
	if trigger == "scheduled" && !source.AutoRefreshEnabled {
		operation := newOperation("rejected")
		operation.Error = "automatic source refresh is disabled"
		return *operation, false
	}
	if active := c.sourceRefreshActive[source.ID]; active != nil {
		return *active, false
	}
	if pending := c.sourceRefreshPending[source.ID]; pending != nil {
		return *pending, false
	}
	operation := newOperation("queued")
	c.sourceRefreshPending[source.ID] = operation
	select {
	case c.sourceRefreshChan <- source.ID:
		return *operation, true
	default:
		delete(c.sourceRefreshPending, source.ID)
		operation.Status = "rejected"
		operation.Error = "source refresh capacity reached"
		return *operation, false
	}
}

func (c *RefreshCoordinator) beginSourceRefresh(sourceID string) (SourceRefreshOperation, bool) {
	c.sourceRefreshMu.Lock()
	defer c.sourceRefreshMu.Unlock()
	if c.shuttingDown.Load() {
		return SourceRefreshOperation{}, false
	}
	operation := c.sourceRefreshPending[sourceID]
	if operation == nil {
		return SourceRefreshOperation{}, false
	}
	delete(c.sourceRefreshPending, sourceID)
	operation.Status = "running"
	operation.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	c.sourceRefreshActive[sourceID] = operation
	return *operation, true
}

func (c *RefreshCoordinator) finishSourceRefresh(sourceID string, result refreshRunResult) {
	c.sourceRefreshMu.Lock()
	defer c.sourceRefreshMu.Unlock()
	operation := c.sourceRefreshActive[sourceID]
	if operation == nil {
		return
	}
	operation.Status, operation.Error = result.Status, result.Error
	if operation.Status == "" {
		operation.Status = "complete"
	}
	operation.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	c.sourceRefreshLast[sourceID] = time.Now()
	delete(c.sourceRefreshActive, sourceID)
}

func (c *RefreshCoordinator) markSourcesRefreshed(sources []Source, at time.Time) {
	c.sourceRefreshMu.Lock()
	defer c.sourceRefreshMu.Unlock()
	for _, source := range sources {
		c.sourceRefreshLast[source.ID] = at
	}
}

func (c *RefreshCoordinator) queueDueSourceRefreshes(store *ConfigStore, globalInterval time.Duration, now time.Time) int {
	queued := 0
	for _, source := range store.Sources() {
		if !source.Enabled || !source.AutoRefreshEnabled {
			continue
		}
		interval := globalInterval
		if source.RefreshIntervalSeconds > 0 {
			interval = time.Duration(source.RefreshIntervalSeconds) * time.Second
		}
		c.sourceRefreshMu.Lock()
		last := c.sourceRefreshLast[source.ID]
		c.sourceRefreshMu.Unlock()
		if !last.IsZero() && now.Sub(last) < interval {
			continue
		}
		if _, accepted := c.requestSourceRefresh(source, "scheduled"); accepted {
			queued++
		}
	}
	return queued
}

func (c *RefreshCoordinator) triggerRecheck() {
	c.lifecycleMu.RLock()
	defer c.lifecycleMu.RUnlock()
	if c.shuttingDown.Load() {
		return
	}
	select {
	case c.recheckChan <- struct{}{}:
	default:
	}
}

func (c *RefreshCoordinator) triggerFullRecheck(pool *ProxyPool) (HealthRecheckOperation, bool) {
	c.lifecycleMu.RLock()
	defer c.lifecycleMu.RUnlock()
	generation, checkURL := pool.HealthCriterion()
	c.healthRecheckOpMu.Lock()
	if c.shuttingDown.Load() {
		c.healthRecheckOpSeq++
		now := time.Now().UTC()
		operation := HealthRecheckOperation{
			ID: fmt.Sprintf("health-recheck-%d-%d", now.UnixNano(), c.healthRecheckOpSeq), Status: "cancelled",
			Generation: generation, CheckURL: checkURL, RequestedAt: now.Format(time.RFC3339Nano),
			CompletedAt: now.Format(time.RFC3339Nano),
		}
		c.healthRecheckOpMu.Unlock()
		return operation, false
	}
	if c.healthRecheckPending != nil && c.healthRecheckPending.Generation == generation {
		operation := *c.healthRecheckPending
		c.healthRecheckOpMu.Unlock()
		return operation, false
	}
	if c.healthRecheckActive != nil && c.healthRecheckActive.Generation == generation {
		operation := *c.healthRecheckActive
		c.healthRecheckOpMu.Unlock()
		return operation, false
	}
	if c.healthRecheckPending != nil {
		superseded := *c.healthRecheckPending
		superseded.Status = "superseded"
		superseded.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
		c.healthRecheckLast = &superseded
	}
	c.healthRecheckOpSeq++
	now := time.Now().UTC()
	job := &HealthRecheckOperation{ID: fmt.Sprintf("health-recheck-%d-%d", now.UnixNano(), c.healthRecheckOpSeq), Status: "queued", Generation: generation, CheckURL: checkURL, RequestedAt: now.Format(time.RFC3339Nano)}
	c.healthRecheckPending = job
	operation := *job
	c.healthRecheckOpMu.Unlock()
	select {
	case c.fullRecheckChan <- struct{}{}:
	default:
	}
	return operation, true
}

func (c *RefreshCoordinator) beginHealthRecheckOperation(pool *ProxyPool, total int) HealthRecheckOperation {
	c.healthRecheckOpMu.Lock()
	defer c.healthRecheckOpMu.Unlock()
	if c.shuttingDown.Load() {
		generation, checkURL := pool.HealthCriterion()
		now := time.Now().UTC()
		return HealthRecheckOperation{Status: "cancelled", Generation: generation, CheckURL: checkURL, RequestedAt: now.Format(time.RFC3339Nano), CompletedAt: now.Format(time.RFC3339Nano)}
	}
	job := c.healthRecheckPending
	c.healthRecheckPending = nil
	if job == nil {
		generation, checkURL := pool.HealthCriterion()
		c.healthRecheckOpSeq++
		now := time.Now().UTC()
		job = &HealthRecheckOperation{ID: fmt.Sprintf("health-recheck-%d-%d", now.UnixNano(), c.healthRecheckOpSeq), Status: "queued", Generation: generation, CheckURL: checkURL, RequestedAt: now.Format(time.RFC3339Nano)}
	}
	job.Status = "running"
	job.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	job.Total = total
	c.healthRecheckActive = job
	return *job
}

func (c *RefreshCoordinator) recordHealthRecheckOutcome(id string, reachable, policyFiltered bool) {
	if id == "" {
		return
	}
	c.healthRecheckOpMu.Lock()
	defer c.healthRecheckOpMu.Unlock()
	if c.healthRecheckActive == nil || c.healthRecheckActive.ID != id {
		return
	}
	c.healthRecheckActive.Completed++
	if reachable {
		c.healthRecheckActive.Reachable++
	} else {
		c.healthRecheckActive.Failed++
	}
	if policyFiltered {
		c.healthRecheckActive.PolicyFiltered++
	}
}

func (c *RefreshCoordinator) finishHealthRecheckOperation(id string, completed bool) {
	c.healthRecheckOpMu.Lock()
	defer c.healthRecheckOpMu.Unlock()
	if c.healthRecheckActive == nil || c.healthRecheckActive.ID != id {
		return
	}
	if completed {
		c.healthRecheckActive.Status = "complete"
	} else {
		c.healthRecheckActive.Status = "superseded"
	}
	c.healthRecheckActive.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	finished := *c.healthRecheckActive
	c.healthRecheckLast = &finished
	c.healthRecheckActive = nil
}

func (c *RefreshCoordinator) shutdown() {
	c.shutdownOnce.Do(func() {
		c.lifecycleMu.Lock()
		defer c.lifecycleMu.Unlock()
		c.shuttingDown.Store(true)
		now := time.Now().UTC().Format(time.RFC3339Nano)

		c.refreshOpMu.Lock()
		operation := c.refreshActive
		if operation == nil {
			operation = c.refreshPending
		}
		if operation != nil {
			operation.Status = "cancelled"
			operation.CompletedAt = now
			operation.Error = "application is shutting down"
			finished := *operation
			c.refreshLast = &finished
		}
		c.refreshActive = nil
		c.refreshPending = nil
		c.refreshOpMu.Unlock()

		c.sourceRefreshMu.Lock()
		for sourceID, operation := range c.sourceRefreshActive {
			operation.Status = "cancelled"
			operation.CompletedAt = now
			operation.Error = "application is shutting down"
			delete(c.sourceRefreshActive, sourceID)
		}
		for sourceID, operation := range c.sourceRefreshPending {
			operation.Status = "cancelled"
			operation.CompletedAt = now
			operation.Error = "application is shutting down"
			delete(c.sourceRefreshPending, sourceID)
		}
		c.sourceRefreshMu.Unlock()

		c.healthRecheckOpMu.Lock()
		healthOperation := c.healthRecheckActive
		if healthOperation == nil {
			healthOperation = c.healthRecheckPending
		}
		if healthOperation != nil {
			healthOperation.Status = "cancelled"
			healthOperation.CompletedAt = now
			finished := *healthOperation
			c.healthRecheckLast = &finished
		}
		c.healthRecheckActive = nil
		c.healthRecheckPending = nil
		c.healthRecheckOpMu.Unlock()

		drainSignal(c.refreshChan)
		drainSignal(c.recheckChan)
		drainSignal(c.fullRecheckChan)
		for {
			select {
			case <-c.sourceRefreshChan:
			default:
				return
			}
		}
	})
}

func drainSignal(channel <-chan struct{}) {
	for {
		select {
		case <-channel:
		default:
			return
		}
	}
}

func (c *RefreshCoordinator) healthRecheckOperationStatus() HealthRecheckOperationStatus {
	c.healthRecheckOpMu.RLock()
	defer c.healthRecheckOpMu.RUnlock()
	clone := func(operation *HealthRecheckOperation) *HealthRecheckOperation {
		if operation == nil {
			return nil
		}
		copy := *operation
		return &copy
	}
	state := "idle"
	if c.healthRecheckPending != nil {
		state = "queued"
	}
	if c.healthRecheckActive != nil {
		state = "running"
	}
	return HealthRecheckOperationStatus{State: state, Active: clone(c.healthRecheckActive), Pending: clone(c.healthRecheckPending), Last: clone(c.healthRecheckLast)}
}

// Package-level helpers retain the historical API while delegating all mutable
// state to the default coordinator.
func getScrapeTimes() (last, next time.Time) { return defaultRefreshCoordinator.scrapeTimes() }
func getScrapeInfo() ScrapeInfo              { return defaultRefreshCoordinator.scrapeInfo() }
func getScrapeStatusSnapshot() scrapeStatusSnapshot {
	return defaultRefreshCoordinator.scrapeStatusSnapshot()
}
func RequestRefresh() (RefreshOperation, bool) { return defaultRefreshCoordinator.requestRefresh() }
func TriggerRefresh()                          { defaultRefreshCoordinator.triggerRefresh() }
func TriggerRecheck()                          { defaultRefreshCoordinator.triggerRecheck() }
func TriggerFullRecheck(pool *ProxyPool) (HealthRecheckOperation, bool) {
	return defaultRefreshCoordinator.triggerFullRecheck(pool)
}

// resetForTest clears all operation state and drains the trigger channels. It
// is intended for tests that need an isolated coordinator.
func (c *RefreshCoordinator) resetForTest() {
	c.refreshOpMu.Lock()
	c.refreshOpSeq = 0
	c.refreshActive = nil
	c.refreshPending = nil
	c.refreshLast = nil
	c.refreshOpMu.Unlock()
	c.healthRecheckOpMu.Lock()
	c.healthRecheckOpSeq = 0
	c.healthRecheckActive = nil
	c.healthRecheckPending = nil
	c.healthRecheckLast = nil
	c.healthRecheckOpMu.Unlock()
	for {
		drained := false
		select {
		case <-c.refreshChan:
			drained = true
		default:
		}
		select {
		case <-c.recheckChan:
			drained = true
		default:
		}
		select {
		case <-c.fullRecheckChan:
			drained = true
		default:
		}
		if !drained {
			return
		}
	}
}

// setScrapeTimesForTest installs deterministic scrape deadlines for tests that
// assert the status API formats them. The caller restores the previous values
// via t.Cleanup.
func (c *RefreshCoordinator) setScrapeTimesForTest(last, next time.Time) {
	c.scrapeMu.Lock()
	c.lastScrapeTime = last
	c.nextScrapeTime = next
	c.scrapeMu.Unlock()
}

// drainFullRecheckSignalForTest reports whether a full recheck trigger is
// pending in the channel, consuming it when present.
func (c *RefreshCoordinator) drainFullRecheckSignalForTest() bool {
	select {
	case <-c.fullRecheckChan:
		return true
	default:
		return false
	}
}

// drainRefreshSignalForTest reports whether a refresh trigger is pending.
func (c *RefreshCoordinator) drainRefreshSignalForTest() bool {
	select {
	case <-c.refreshChan:
		return true
	default:
		return false
	}
}

// drainRecheckSignalForTest reports whether a bounded recheck trigger is pending.
func (c *RefreshCoordinator) drainRecheckSignalForTest() bool {
	select {
	case <-c.recheckChan:
		return true
	default:
		return false
	}
}
