package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	recheckProbeExitIP               = probeExitIPContext
	recheckCheckCredentialCandidates = checkCredentialCandidates
)

// ScrapeInfo separates source inventory from the tested/usable pool. Keeping
// these counters distinct prevents a 250k-entry feed from being presented as
// 250k usable proxies, while still making it obvious that candidates were not
// deleted merely because the bounded checker has not reached them yet.
type ScrapeInfo struct {
	Raw         int `json:"raw"`
	Candidates  int `json:"candidates"`
	Checked     int `json:"checked"`
	FreshAlive  int `json:"fresh_alive"`
	SourceTotal int `json:"source_total"`
	SourceError int `json:"source_errors"`
}

// RefreshOperation makes the asynchronous /api/refresh action observable.
// A second request coalesces with the already-queued job, while a request made
// during a running job creates one bounded follow-up job in the coordinator.
type RefreshOperation struct {
	ID           string `json:"id"`
	Status       string `json:"status"`            // queued, running, complete, partial, skipped
	Trigger      string `json:"trigger,omitempty"` // startup, scheduled, manual
	RequestedAt  string `json:"requested_at"`
	StartedAt    string `json:"started_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
	SourceErrors int    `json:"source_errors,omitempty"`
	Error        string `json:"error,omitempty"`
}

type SourceRefreshOperation struct {
	ID             string `json:"id"`
	SourceID       string `json:"source_id"`
	SourceName     string `json:"source_name"`
	Status         string `json:"status"`
	Trigger        string `json:"trigger"`
	RequestedAt    string `json:"requested_at"`
	StartedAt      string `json:"started_at,omitempty"`
	CompletedAt    string `json:"completed_at,omitempty"`
	Error          string `json:"error,omitempty"`
	sourceRevision sourceRefreshRevision
}

type sourceRefreshRevision Source

func newSourceRefreshRevision(source Source) sourceRefreshRevision {
	return sourceRefreshRevision(source)
}

func (revision sourceRefreshRevision) matches(source Source, trigger string) bool {
	if !source.Enabled || (trigger == "scheduled" && !source.AutoRefreshEnabled) {
		return false
	}
	return revision == sourceRefreshRevision(source)
}

type RefreshOperationStatus struct {
	State   string            `json:"state"` // idle, queued, running
	Active  *RefreshOperation `json:"active,omitempty"`
	Pending *RefreshOperation `json:"pending,omitempty"`
	Last    *RefreshOperation `json:"last,omitempty"`
}

type HealthRecheckOperation struct {
	ID             string `json:"id"`
	Status         string `json:"status"` // queued, running, complete, superseded
	Generation     uint64 `json:"generation"`
	CheckURL       string `json:"check_url"`
	RequestedAt    string `json:"requested_at"`
	StartedAt      string `json:"started_at,omitempty"`
	CompletedAt    string `json:"completed_at,omitempty"`
	Total          int    `json:"total"`
	Completed      int    `json:"completed"`
	Reachable      int    `json:"reachable"`
	Failed         int    `json:"failed"`
	PolicyFiltered int    `json:"policy_filtered"`
}

type HealthRecheckOperationStatus struct {
	State   string                  `json:"state"`
	Active  *HealthRecheckOperation `json:"active,omitempty"`
	Pending *HealthRecheckOperation `json:"pending,omitempty"`
	Last    *HealthRecheckOperation `json:"last,omitempty"`
}

type refreshRunResult struct {
	Status       string
	SourceErrors int
	Error        string
}

type scrapeStatusSnapshot struct {
	Last time.Time
	Next time.Time
	Info ScrapeInfo
}

type backgroundLifecycle struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newBackgroundLifecycle(parent context.Context) *backgroundLifecycle {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &backgroundLifecycle{ctx: ctx, cancel: cancel}
}

func (l *backgroundLifecycle) Context() context.Context { return l.ctx }

func (l *backgroundLifecycle) Go(worker func(context.Context)) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		worker(l.ctx)
	}()
}

func (l *backgroundLifecycle) Cancel() { l.cancel() }

func (l *backgroundLifecycle) Join(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func shutdownApplication(ctx context.Context, coordinator *RefreshCoordinator, lifecycle *backgroundLifecycle, stopAdmission func(context.Context) error, flush func() error) error {
	coordinator.shutdown()
	admissionErr := stopAdmission(ctx)
	lifecycle.Cancel()
	if err := lifecycle.Join(ctx); err != nil {
		return errors.Join(admissionErr, fmt.Errorf("background workers did not stop before shutdown deadline: %w", err))
	}
	return errors.Join(admissionErr, flush())
}

func waitForShutdown(signalDone <-chan struct{}, serverErrors <-chan error, listenerFatalEvents <-chan ListenerFatalEvent) error {
	select {
	case <-signalDone:
		return nil
	case err := <-serverErrors:
		return err
	case event := <-listenerFatalEvents:
		return fmt.Errorf("auxiliary listener %s (%s) stopped unexpectedly: %w", event.ID, event.Addr, event.Err)
	}
}

func stopAndDrainTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func main() {
	cfg := ParseConfig()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("[main] invalid configuration: %v", err)
	}

	store, err := NewConfigStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("[main] failed to load config: %v", err)
	}

	log.Printf("socks5-pool starting...")
	log.Printf("  listen:   %s", cfg.ListenAddr)
	log.Printf("  status:   %s", cfg.StatusAddr)
	log.Printf("  data-dir: %s", cfg.DataDir)
	log.Printf("  scrape:   every %s", cfg.ScrapeInterval)

	// Establish the root application context before any refresh, recheck,
	// rotation, or baseline request starts.
	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	background := newBackgroundLifecycle(context.Background())

	// Establish the policy baseline before deciding whether a persisted pool was
	// validated under the same require-ip-change criterion. When the policy is
	// disabled its fingerprint is baseline-independent, so avoid delaying both
	// listeners on an unnecessary external request. The dashboard override
	// (when set) wins over the CLI flag here and everywhere below.
	if store.RequireIPChange(cfg.RequireIPChange) {
		_, _ = RefreshBaselineExitWithChangeContext(signalContext, store.CheckTimeout(cfg.CheckTimeout))
	}

	coordinator := newRefreshCoordinator()
	pool := NewProxyPool()
	pool.SetHealthCriterion(store.CheckURL())
	pool.SetRequireIPChangePolicy(store.RequireIPChange(cfg.RequireIPChange))
	candidateCache := newCandidateCatalogCache(cfg.DataDir)
	pool.candidates.SetDiskCache(candidateCache)
	if loaded, loadErr := pool.candidates.LoadDiskCache(); loadErr != nil {
		log.Printf("[candidate-cache] load failed, continuing with an empty catalog: %v", loadErr)
	} else if loaded {
		reset := pool.candidates.ResetHealthOutcomes()
		snapshot := pool.candidates.snapshot.Load()
		snapshot.mu.RLock()
		log.Printf("[candidate-cache] restored %d candidates and reset %d criterion-dependent outcomes (generation=%d revision=%d phase=%s)", len(snapshot.records), reset, snapshot.generation, snapshot.revision, snapshot.phase)
		snapshot.mu.RUnlock()
	}

	// Seed from the on-disk cache so the pool is usable immediately, then
	// enable write-back so every refresh keeps the cache fresh.
	cache := newPoolCache(cfg.DataDir)
	cacheCriterionChanged := false
	if fwd, info, stats, cachedCheckURL, cachedHealthPolicy, cachedRecheckPending := cache.loadWithHealthState(); len(fwd) > 0 || len(info) > 0 {
		pool.Prime(fwd, info)
		pool.restoreStats(stats)
		// A legacy cache has no criterion metadata. Treat it as stale rather
		// than advertising nodes that may only have passed the former HTTP
		// default (or another user target) as healthy under today's URL.
		if strings.TrimSpace(cachedCheckURL) != store.CheckURL() || cachedHealthPolicy != pool.HealthPolicyFingerprint() {
			pool.InvalidateHealth(store.CheckURL())
			cacheCriterionChanged = true
		} else if cachedRecheckPending {
			pool.RestoreHealthRecheckPending()
			cacheCriterionChanged = true
		}
	}
	pool.SetCache(cache)
	// Config may have changed while the process was offline. Retire cached
	// nodes whose full provenance is no longer enabled before either listener
	// accepts traffic, while retaining the inventory for later recovery.
	if retired := pool.ApplyEnabledSources(store.Sources()); retired > 0 {
		log.Printf("[pool] retired %d cached node(s) from disabled or deleted sources", retired)
		cacheCriterionChanged = true
	}
	if cacheCriterionChanged {
		if err := pool.FlushCache(); err != nil {
			log.Printf("[cache] startup flush failed: %v", err)
		}
		if _, queued := coordinator.triggerFullRecheck(pool); !queued {
			log.Printf("[main] full recheck already pending or active; not re-queued")
		}
	}

	// Background: initial scrape + check, then periodic scrape + manual refresh,
	// all serialized through one lifecycle-owned worker.
	background.Go(func(ctx context.Context) {
		run := func(trigger string, manual bool) {
			var operationID string
			if manual {
				log.Printf("[main] manual refresh triggered")
				operationID = coordinator.beginRefreshOperation()
			} else {
				operationID = coordinator.beginBackgroundRefreshOperation(trigger)
			}
			if operationID == "" {
				return
			}
			result := refreshPoolContext(ctx, cfg, store, pool, coordinator)
			coordinator.finishRefreshOperation(operationID, result)
		}

		run("startup", false)
		if ctx.Err() != nil {
			return
		}
		coordinator.markSourcesRefreshed(store.Sources(), time.Now())
		scanInterval := cfg.ScrapeInterval
		if scanInterval > time.Minute {
			scanInterval = time.Minute
		}
		timer := time.NewTimer(scanInterval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				coordinator.queueDueSourceRefreshes(store, cfg.ScrapeInterval, time.Now())
				timer.Reset(scanInterval)
			case <-coordinator.refreshChan:
				run("manual", true)
			case sourceID := <-coordinator.sourceRefreshChan:
				operation, ok := coordinator.beginSourceRefresh(sourceID)
				if !ok {
					continue
				}
				result := refreshSourceContext(ctx, cfg, store, pool, coordinator, sourceID, operation.Trigger, operation.sourceRevision)
				coordinator.finishSourceRefresh(sourceID, result)
				if result.Status == "complete" {
					coordinator.recordSourceRefresh(time.Now(), store.SourceRefreshInterval(cfg.ScrapeInterval))
				}
			}
		}
	})

	// Background: random rotation of the default (ANY) group every 3-6 minutes.
	background.Go(func(ctx context.Context) {
		for {
			delay := 3*time.Minute + time.Duration(rand.Intn(4))*time.Minute
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if ctx.Err() != nil {
				return
			}
			if pool.Size() == 0 {
				log.Printf("[main] pool empty, triggering immediate refresh")
				coordinator.triggerRefresh()
			} else if pool.Size() > 1 {
				pool.RotateSticky(GroupAny)
			}
		}
	})

	// Background: keep a bounded rotating slice fresh without coupling it to
	// the independently configured exhaustive full-pool schedule.
	background.Go(func(ctx context.Context) {
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			case <-coordinator.recheckChan:
				stopAndDrainTimer(timer)
			}
			if ctx.Err() != nil {
				return
			}
			baselineCurrent, baselineChanged := refreshBaselineBeforeRecheck(ctx, cfg, store, pool, coordinator)
			if !baselineCurrent {
				return
			}
			if !baselineChanged {
				reCheckAliveContext(ctx, cfg, store, pool, coordinator)
			}
			if ctx.Err() != nil {
				return
			}
			timer.Reset(5 * time.Minute)
		}
	})

	// Background: exhaustively re-check every retained forwarding node on its
	// own persisted schedule. Manual/criterion-change triggers wake the same
	// worker, and each completed cycle reads the latest interval before reset.
	background.Go(func(ctx context.Context) {
		interval := store.FullRecheckInterval(cfg.FullRecheckInterval)
		coordinator.scheduleFullRecheck(time.Now().Add(interval))
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			timerWake := false
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				timerWake = true
			case <-coordinator.fullRecheckChan:
				stopAndDrainTimer(timer)
			}
			if ctx.Err() != nil {
				return
			}
			// Channel tokens only wake the worker. Pending operation state is the
			// authority; a consumed/coalesced token must not synthesize a cycle.
			if !timerWake && !coordinator.hasPendingHealthRecheck() {
				interval = store.FullRecheckInterval(cfg.FullRecheckInterval)
				coordinator.scheduleFullRecheck(time.Now().Add(interval))
				timer.Reset(interval)
				continue
			}
			baselineCurrent, _ := refreshBaselineBeforeRecheck(ctx, cfg, store, pool, coordinator)
			if !baselineCurrent {
				return
			}
			_ = runFullRecheckCycleWithWake(ctx, cfg, store, pool, coordinator, timerWake)
			if ctx.Err() != nil {
				return
			}
			interval = store.FullRecheckInterval(cfg.FullRecheckInterval)
			coordinator.scheduleFullRecheck(time.Now().Add(interval))
			timer.Reset(interval)
		}
	})

	status := NewStatusServerWithAdminCredentialsAndCoordinator(pool, store, coordinator, cfg.AdminUser, cfg.AdminPass)
	status.SetCheckDefaults(cfg.CheckTimeout, cfg.MaxConcurrent, cfg.MaxCandidates, cfg.RequireIPChange)
	status.SetScheduleDefaults(cfg.ScrapeInterval, cfg.FullRecheckInterval)
	listenerManager := NewListenerManager(
		cfg.ListenAddr, pool, store, cfg.SOCKSUser, cfg.SOCKSPass, cfg.MaxClientConnections,
	)
	status.SetListenerManager(listenerManager)
	trustedManagementProxies := make([]string, 0, len(cfg.TrustedManagementProxies))
	for _, ip := range cfg.TrustedManagementProxies {
		trustedManagementProxies = append(trustedManagementProxies, ip.String())
	}
	if err := status.SetTrustedManagementProxies(trustedManagementProxies); err != nil {
		log.Fatalf("[main] invalid trusted management proxy: %v", err)
	}
	server := NewServerWithSharedAdmissionAndPolicy(
		cfg.ListenAddr, pool, store, cfg.SOCKSUser, cfg.SOCKSPass, listenerManager.slots, nil,
	)

	if err := listenerManager.Start(); err != nil {
		log.Fatalf("[main] start additional listeners: %v", err)
	}

	// Treat both listeners as one service. A bind/runtime failure in either is
	// fatal, while SIGINT/SIGTERM closes admission, drains handlers to a bounded
	// deadline, and flushes the latest pool state before the process exits.
	serverErrors := make(chan error, 2)
	go func() {
		log.Printf("[status] dashboard at http://%s", cfg.StatusAddr)
		serverErrors <- status.Start(cfg.StatusAddr)
	}()
	go func() { serverErrors <- server.Start() }()

	exitErr := waitForShutdown(signalContext.Done(), serverErrors, listenerManager.FatalEvents())
	if exitErr == nil {
		log.Printf("[main] shutdown requested")
	} else {
		log.Printf("[main] listener failed: %v", exitErr)
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	shutdownErr := shutdownApplication(shutdownContext, coordinator, background, func(ctx context.Context) error {
		return errors.Join(status.Shutdown(ctx), listenerManager.Shutdown(ctx), server.Shutdown(ctx))
	}, pool.FlushCache)
	if shutdownErr != nil {
		log.Printf("[main] shutdown incomplete: %v", shutdownErr)
	}
	if exitErr != nil {
		log.Fatalf("[main] stopped after listener failure: %v", exitErr)
	}
}

func refreshBaselineBeforeRecheck(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) (current, changed bool) {
	if !store.RequireIPChange(cfg.RequireIPChange) {
		return ctx.Err() == nil, false
	}
	_, baselineChanged := RefreshBaselineExitWithChangeContext(ctx, store.CheckTimeout(cfg.CheckTimeout))
	if ctx.Err() != nil {
		return false, false
	}
	if !baselineChanged || !pool.SetRequireIPChangePolicy(true) {
		return true, false
	}
	pool.InvalidateHealth(store.CheckURL())
	pool.candidates.ResetHealthOutcomes()
	if err := pool.FlushCache(); err != nil {
		log.Printf("[cache] baseline change flush failed: %v", err)
	}
	_, _ = coordinator.triggerFullRecheck(pool)
	return true, true
}

// reCheckAlive re-probes a bounded, rotating slice of known nodes against the
// configured CheckURL and records the outcome, so quality scores stay current
// without an unbounded retained pool turning a five-minute background pass
// into a multi-hour job. No scraping or geo lookups happen here.
func reCheckAlive(cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) {
	reCheckAliveContext(context.Background(), cfg, store, pool, coordinator)
}

func reCheckAliveContext(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) {
	_, _ = reCheckNodesContext(ctx, cfg, store, pool, coordinator, pool.RecheckCandidates(store.MaxCandidates(cfg.MaxCandidates)), pool.Size(), "recheck", "")
}

func reCheckAllAlive(cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) {
	_ = reCheckAllAliveContext(context.Background(), cfg, store, pool, coordinator)
}

func reCheckAllAliveContext(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) bool {
	return reCheckAllAliveContextWithWake(ctx, cfg, store, pool, coordinator, true)
}

func reCheckAllAliveContextWithWake(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator, timerWake bool) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false
	}
	coordinator.healthCycleMu.Lock()
	defer coordinator.healthCycleMu.Unlock()

	_, _ = currentHealthCriterion(pool, store)
	nodes := pool.FullRecheckCandidates()
	operation, claimed := coordinator.claimHealthRecheckOperation(pool, len(nodes), timerWake)
	if !claimed {
		return false
	}
	_, completed := reCheckNodesLockedContext(ctx, cfg, store, pool, coordinator, nodes, len(nodes), "full-recheck", operation.ID, true, operation.Generation, operation.CheckURL)
	if completed {
		completed = pool.CompleteHealthRecheck(operation.Generation)
		if completed {
			if err := pool.FlushCache(); err != nil {
				log.Printf("[cache] health recheck flush failed: %v", err)
				pool.RestoreHealthRecheckPending()
				completed = false
			}
		}
	}
	coordinator.finishHealthRecheckOperation(operation.ID, completed)
	return completed
}

func recordCompletedFullRecheck(coordinator *RefreshCoordinator, store *ConfigStore, fallback time.Duration, completedAt time.Time) {
	coordinator.recordFullRecheck(completedAt, store.FullRecheckInterval(fallback))
}

func runFullRecheckCycle(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) bool {
	return runFullRecheckCycleWithWake(ctx, cfg, store, pool, coordinator, true)
}

func runFullRecheckCycleWithWake(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator, timerWake bool) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	if !reCheckAllAliveContextWithWake(ctx, cfg, store, pool, coordinator, timerWake) {
		return false
	}
	recordCompletedFullRecheck(coordinator, store, cfg.FullRecheckInterval, time.Now())
	return true
}

func reCheckNodes(cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator, nodes []Proxy, knownTotal int, logLabel, operationID string) (uint64, bool) {
	return reCheckNodesContext(context.Background(), cfg, store, pool, coordinator, nodes, knownTotal, logLabel, operationID)
}

func reCheckNodesContext(parent context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator, nodes []Proxy, knownTotal int, logLabel, operationID string) (uint64, bool) {
	return reCheckNodesWithModeContext(parent, cfg, store, pool, coordinator, nodes, knownTotal, logLabel, operationID, false)
}

func reCheckNodesWithModeContext(parent context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator, nodes []Proxy, knownTotal int, logLabel, operationID string, exhaustive bool) (uint64, bool) {
	if parent == nil {
		parent = context.Background()
	}
	coordinator.healthCycleMu.Lock()
	defer coordinator.healthCycleMu.Unlock()
	healthGeneration, testURL := currentHealthCriterion(pool, store)
	return reCheckNodesLockedContext(parent, cfg, store, pool, coordinator, nodes, knownTotal, logLabel, operationID, exhaustive, healthGeneration, testURL)
}

// reCheckNodesLockedContext runs with coordinator.healthCycleMu held. Exhaustive
// results stay detached until every worker has returned and the cycle remains
// current; bounded checks retain their per-node publication behavior.
func reCheckNodesLockedContext(parent context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator, nodes []Proxy, knownTotal int, logLabel, operationID string, exhaustive bool, healthGeneration uint64, testURL string) (uint64, bool) {
	if len(nodes) == 0 {
		return healthGeneration, parent.Err() == nil
	}
	healthContext, finishHealthWork, current := pool.BeginHealthWork(healthGeneration)
	if !current {
		return healthGeneration, false
	}
	defer finishHealthWork()
	workContext, cancelWork := context.WithCancel(healthContext)
	stopParentCancel := context.AfterFunc(parent, cancelWork)
	defer func() {
		stopParentCancel()
		cancelWork()
	}()
	var wg sync.WaitGroup
	// Read the effective check options once per cycle: dashboard overrides win
	// over the CLI flags, and a mid-cycle change applies to the next cycle.
	maxConcurrent := store.MaxConcurrent(cfg.MaxConcurrent)
	checkTimeout := store.CheckTimeout(cfg.CheckTimeout)
	requireIPChange := store.RequireIPChange(cfg.RequireIPChange)
	sem := make(chan struct{}, maxConcurrent)
	baseline := BaselineExitIP()
	var outcomeMu sync.Mutex
	checked := make([]Proxy, 0, len(nodes))
	reachableKeys := make(map[string]bool, len(nodes))
	policyFiltered := make(map[string]bool)
	fullObservations := make([]fullHealthObservation, 0, len(nodes))
checkLoop:
	for _, px := range nodes {
		if px.Protocol == "proxyip" {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-workContext.Done():
			break checkLoop
		}
		wg.Add(1)
		go func(px Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			nodeContext, cancelNode := context.WithTimeout(workContext, checkTimeout)
			defer cancelNode()
			verified, reachable, latency := recheckCheckCredentialCandidates(nodeContext, px, testURL, checkTimeout)
			if workContext.Err() != nil {
				return
			}
			policyAllowed := true
			exitIP := ""
			ipChangeKnown := false
			ipChanged := false
			if reachable && requireIPChange {
				exitIP = recheckProbeExitIP(nodeContext, verified, checkTimeout)
				if workContext.Err() != nil {
					return
				}
				policy := evaluateIPChangePolicy(exitIP, baseline, requireIPChange)
				ipChangeKnown = policy.IPChangeKnown
				ipChanged = policy.IPChanged
				policyAllowed = policy.PolicyAllowed
			}

			if exhaustive {
				outcomeMu.Lock()
				checked = append(checked, px)
				fullObservations = append(fullObservations, fullHealthObservation{
					Key: px.Key(), Verified: verified, CredentialsVerified: reachable,
					Reachable: reachable, PolicyAllowed: policyAllowed, LatencyMs: latency.Milliseconds(),
					ExitIP: exitIP, IPChanged: ipChanged, IPChangeKnown: ipChangeKnown,
				})
				if reachable {
					reachableKeys[px.Key()] = true
				}
				if reachable && !policyAllowed {
					policyFiltered[px.Key()] = true
				}
				outcomeMu.Unlock()
				coordinator.recordHealthRecheckOutcome(operationID, reachable, reachable && !policyAllowed)
				return
			}

			if reachable {
				pool.UpdateVerifiedCredentialsAtGeneration(px.Key(), verified, healthGeneration)
			}
			if !pool.ObserveHealthOutcomeAtGeneration(px.Key(), reachable, policyAllowed, latency.Milliseconds(), healthGeneration) {
				return
			}
			if exitIP != "" {
				pool.UpdateGeoAtGeneration(px.Key(), exitIP, "", "", "", ipChanged, ipChangeKnown, healthGeneration)
			}
			outcomeMu.Lock()
			checked = append(checked, px)
			if reachable {
				reachableKeys[px.Key()] = true
			}
			if reachable && !policyAllowed {
				policyFiltered[px.Key()] = true
			}
			outcomeMu.Unlock()
			coordinator.recordHealthRecheckOutcome(operationID, reachable, reachable && !policyAllowed)
		}(px)
	}
	wg.Wait()
	if workContext.Err() != nil {
		return healthGeneration, false
	}

	removedNodes := 0
	if exhaustive {
		result, err := pool.commitFullHealthRecheckContext(workContext, fullObservations, healthGeneration)
		if err != nil {
			log.Printf("[cache] full health recheck commit failed: %v", err)
			return healthGeneration, false
		}
		if !result.Applied {
			return healthGeneration, false
		}
		removedNodes = len(result.RemovedKeys)
	}
	pool.candidates.ApplyHealthOutcomes(checked, reachableKeys, policyFiltered)
	if exhaustive {
		log.Printf("[%s] re-probed %d/%d retained nodes against %s; removed %d after three full-check failures", logLabel, len(nodes), knownTotal, safeSourceURL(testURL), removedNodes)
	} else {
		log.Printf("[%s] re-probed %d/%d known nodes against %s", logLabel, len(nodes), knownTotal, safeSourceURL(testURL))
	}
	return healthGeneration, true
}

// refreshPool fetches every enabled source concurrently, dedups the
// combined candidate list, health-checks it, and installs the result as
// the pool's new live proxy list.
func refreshPool(cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) refreshRunResult {
	return refreshPoolContext(context.Background(), cfg, store, pool, coordinator)
}

func refreshPoolContext(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) refreshRunResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return refreshRunResult{Status: "cancelled", Error: err.Error()}
	}
	coordinator.sourceLifecycleMu.RLock()
	sources := store.EnabledSources()
	coordinator.sourceLifecycleMu.RUnlock()
	if len(sources) == 0 {
		log.Printf("[main] no enabled sources, skipping refresh")
		return refreshRunResult{Status: "skipped", Error: "no enabled sources"}
	}
	sourceRevisions := make(map[string]sourceRefreshRevision, len(sources))
	for _, src := range sources {
		sourceRevisions[src.ID] = newSourceRefreshRevision(src)
	}

	var (
		mu            sync.Mutex
		all           []Proxy
		sourceErrors  int
		failedSources = make(map[string]bool)
		wg            sync.WaitGroup
	)
	// A fixed worker set starts every configured source eventually without
	// creating dozens of goroutines that can expire in a separate queue before
	// they ever get a network slot.
	jobs := make(chan Source)
	workerCount := min(len(sources), maxConcurrentSourceFetches)
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for src := range jobs {
				proxies, err := store.LoadSourceContext(ctx, src)
				if err != nil {
					log.Printf("[error] scrape %s failed: %v", src.Name, err)
					mu.Lock()
					sourceErrors++
					failedSources[src.ID] = true
					mu.Unlock()
					continue
				}
				for i := range proxies {
					// During dedupe/catalog construction SourceName carries the stable
					// ConfigStore ID. It is translated back to the display name before
					// candidates reach the checker/pool, avoiding extra attribution
					// fields and per-entry slices on a ~500k-item transient inventory.
					proxies[i].SourceName = src.ID
				}
				mu.Lock()
				if len(proxies) > maxCandidateCacheRecords-len(all) {
					sourceErrors++
					failedSources[src.ID] = true
					mu.Unlock()
					log.Printf("[error] scrape %s exceeded combined candidate budget %d; preserving its previous catalog", src.Name, maxCandidateCacheRecords)
					continue
				}
				all = append(all, proxies...)
				mu.Unlock()
			}
		}()
	}
sendJobs:
	for _, src := range sources {
		select {
		case jobs <- src:
		case <-ctx.Done():
			break sendJobs
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return refreshRunResult{Status: "cancelled", SourceErrors: sourceErrors, Error: err.Error()}
	}

	// Publish only source revisions that are still current. Source mutations take
	// the same lock, so no disable/delete/revision change can interleave between
	// this validation and the candidate catalog's first publication.
	coordinator.sourceLifecycleMu.Lock()
	validSources, sourceLabels := currentFullRefreshSources(store.Sources(), sourceRevisions)
	all = filterCandidatesBySources(all, validSources)
	for sourceID := range failedSources {
		if !validSources[sourceID] {
			delete(failedSources, sourceID)
			sourceErrors--
		}
	}
	rawCount := len(all)
	deduped := dedupeCandidates(all)
	all = nil
	candidateTotal := len(deduped)
	catalogRefresh := pool.candidates.begin(deduped, sourceLabels, failedSources, sourceErrors)
	coordinator.sourceLifecycleMu.Unlock()
	captureCandidateSourceIDs(deduped)
	restoreCandidateSourceLabels(deduped, sourceLabels)

	// ProxyIP entries are external reverse-proxy/jump resources for Cloudflare
	// Worker-style deployments rather than generic SOCKS/HTTP upstreams. They
	// remain fully browseable in the candidate catalog, but must not consume
	// scarce forwarding health-check slots or enter the routable pool.
	healthInventory, resourceCount := splitHealthInventory(deduped)
	deduped = nil
	// Automatic refreshes share the same durable admission rule as periodic
	// rechecks. Filter before sampling so a cooled or terminal known node cannot
	// consume a scarce slot that should discover an unseen candidate.
	healthInventory = pool.FilterAutoRecheckCandidates(healthInventory)

	// Some sources (e.g. large community-aggregated lists) return well
	// over 100k raw entries. Checking all of them every cycle would make
	// a refresh take hours and hammer auxiliary lookup services. Cap the
	// checked set, but retain a small cursor state so repeated cycles walk
	// deterministically through the entire source inventory instead of
	// retrying the same failing prefix forever.
	candidates := healthInventory
	maxCandidates := store.MaxCandidates(cfg.MaxCandidates)
	if len(candidates) > maxCandidates {
		known := make(map[string]bool, pool.Size())
		for _, px := range pool.All() {
			known[px.Key()] = true
		}
		candidates = newCandidateSampler(cfg.DataDir).selectCandidates(healthInventory, known, maxCandidates)
		log.Printf("[main] %d candidates exceed max-candidates=%d, selecting an unseen-first source/protocol-balanced rotating subset (rest deferred)",
			len(healthInventory), maxCandidates)
	}
	healthInventory = nil

	// unreachable (from CheckProxies) is addresses that were actually dialed
	// and genuinely failed to connect - as opposed to ones that connected
	// fine but got excluded from alive for a policy reason (transparent
	// proxy). Only genuine connectivity failures should flip a
	// previously-known-good node to Available=false.
	coordinator.healthCycleMu.Lock()
	defer coordinator.healthCycleMu.Unlock()
	healthGeneration, testURL := currentHealthCriterion(pool, store)
	healthContext, finishHealthWork, current := pool.BeginHealthWork(healthGeneration)
	if !current {
		pool.candidates.complete(catalogRefresh, nil, nil, nil)
		return refreshRunResult{Status: "skipped", SourceErrors: sourceErrors, Error: "health criterion changed before checking; exhaustive recheck queued"}
	}
	workContext, cancelWork := context.WithCancel(ctx)
	stopHealthCancel := context.AfterFunc(healthContext, cancelWork)
	alive, unreachable, policyFiltered := checkProxiesDetailedContext(workContext, candidates, store.CheckTimeout(cfg.CheckTimeout), store.MaxConcurrent(cfg.MaxConcurrent), store.RequireIPChange(cfg.RequireIPChange), testURL)
	stopHealthCancel()
	cancelWork()
	finishHealthWork()
	if err := ctx.Err(); err != nil {
		pool.candidates.complete(catalogRefresh, nil, nil, nil)
		return refreshRunResult{Status: "cancelled", SourceErrors: sourceErrors, Error: err.Error()}
	}
	if coordinator.fullRefreshBeforeFinalValidation != nil {
		coordinator.fullRefreshBeforeFinalValidation()
	}
	coordinator.sourceLifecycleMu.Lock()
	validSources, _ = currentFullRefreshSources(store.Sources(), sourceRevisions)
	for sourceID := range failedSources {
		if !validSources[sourceID] {
			delete(failedSources, sourceID)
			sourceErrors--
		}
	}
	candidates = filterRefreshResultsBySources(candidates, validSources)
	alive = filterRefreshResultsBySources(alive, validSources)
	unreachable = filterRefreshOutcomeKeys(unreachable, candidates)
	policyFiltered = filterRefreshOutcomeKeys(policyFiltered, candidates)
	reconcileFullRefreshCatalog(pool.candidates, catalogRefresh, validSources, sourceErrors)
	applied := pool.UpdateWithEnabledSourcesAndPolicy(alive, unreachable, policyFiltered, store.Sources(), healthGeneration)
	if !applied {
		pool.candidates.complete(catalogRefresh, nil, nil, nil)
		coordinator.sourceLifecycleMu.Unlock()
		log.Printf("[main] discarded health results because the check criterion changed during refresh")
		return refreshRunResult{Status: "skipped", SourceErrors: sourceErrors, Error: "health criterion changed during refresh; exhaustive recheck queued"}
	}
	pool.candidates.complete(catalogRefresh, candidates, alive, policyFiltered)
	coordinator.sourceLifecycleMu.Unlock()

	coordinator.recordScrape(ScrapeInfo{
		Raw: rawCount, Candidates: candidateTotal, Checked: len(candidates),
		FreshAlive: len(alive), SourceTotal: len(sources), SourceError: sourceErrors,
	}, store.SourceRefreshInterval(cfg.ScrapeInterval))
	// Persist the new pool membership immediately rather than relying on the
	// 500ms debounce timer. A process kill between refresh completion and the
	// debounced write would otherwise lose the freshly discovered nodes.
	if err := pool.FlushCache(); err != nil {
		log.Printf("[cache] pool refresh flush failed: %v", err)
	}
	log.Printf("[main] pool refreshed: %d fresh alive / %d checked against %s; %d known total (from %d sources, %d errors, %d raw, %d protocol-aware candidates, %d non-routable resources)",
		len(alive), len(candidates), safeSourceURL(testURL), pool.Size(), len(sources), sourceErrors, rawCount, candidateTotal, resourceCount)
	status := "complete"
	if sourceErrors > 0 {
		status = "partial"
	}
	return refreshRunResult{Status: status, SourceErrors: sourceErrors}
}

// refreshSource fetches exactly one configured source. Other source
// attributions are retained as last-good inventory by the catalog merge.
func refreshSource(cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator, sourceID, trigger string, revision sourceRefreshRevision) refreshRunResult {
	return refreshSourceContext(context.Background(), cfg, store, pool, coordinator, sourceID, trigger, revision)
}

func refreshSourceContext(ctx context.Context, cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator, sourceID, trigger string, revision sourceRefreshRevision) refreshRunResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return refreshRunResult{Status: "cancelled", Error: err.Error()}
	}
	coordinator.sourceLifecycleMu.RLock()
	source, ok := store.SourceByID(sourceID)
	if !ok {
		coordinator.sourceLifecycleMu.RUnlock()
		return refreshRunResult{Status: "skipped", Error: "source no longer exists"}
	}
	if !revision.matches(source, trigger) {
		coordinator.sourceLifecycleMu.RUnlock()
		return refreshRunResult{Status: "skipped", Error: "source is disabled or changed before refresh"}
	}
	coordinator.sourceLifecycleMu.RUnlock()

	proxies, err := store.LoadSourceContext(ctx, source)
	if err != nil {
		if ctx.Err() != nil {
			return refreshRunResult{Status: "cancelled", Error: ctx.Err().Error()}
		}
		log.Printf("[error] source refresh %s failed: %v", source.Name, err)
		return refreshRunResult{Status: "failed", SourceErrors: 1, Error: err.Error()}
	}
	for i := range proxies {
		proxies[i].SourceName = source.ID
	}
	deduped := dedupeCandidates(proxies)
	if len(deduped) > maxCandidateCacheRecords {
		return refreshRunResult{Status: "failed", SourceErrors: 1, Error: "source exceeded candidate budget"}
	}
	labels := make(map[string]string)
	retained := make(map[string]bool)
	for _, configured := range store.Sources() {
		labels[configured.ID] = configured.Name
		if configured.ID != source.ID {
			retained[configured.ID] = true
		}
	}

	// Keep deduped untouched until it is published to CandidateCatalog: it holds
	// stable Source.ID values and the full source inventory. Build only a bounded
	// health-check work set, because the scheduled eligibility filter used to
	// compact the catalog's backing slice in place.
	excluded := map[string]bool(nil)
	if trigger == "scheduled" {
		excluded = pool.autoRecheckExcludedKeys()
	}
	includeHealthCandidate := func(px Proxy) bool {
		return isForwardingProtocol(px.Protocol) && !excluded[px.Key()]
	}
	healthCandidateTotal := 0
	for _, px := range deduped {
		if includeHealthCandidate(px) {
			healthCandidateTotal++
		}
	}
	maxCandidates := store.MaxCandidates(cfg.MaxCandidates)
	candidates := make([]Proxy, 0, min(healthCandidateTotal, maxCandidates))
	if healthCandidateTotal <= maxCandidates {
		for _, px := range deduped {
			if includeHealthCandidate(px) {
				candidates = append(candidates, px)
			}
		}
	} else {
		known := make(map[string]bool, pool.Size())
		for _, px := range pool.All() {
			known[px.Key()] = true
		}
		candidates = newCandidateSampler(cfg.DataDir).selectCandidatesWhere(deduped, known, maxCandidates, includeHealthCandidate)
	}
	// Only health-check candidates need user-facing source labels and copied
	// provenance. Cloning this bounded work set prevents label conversion from
	// mutating the stable-ID catalog input above.
	candidates = cloneProxySlice(candidates)
	captureCandidateSourceIDs(candidates)
	restoreCandidateSourceLabels(candidates, labels)
	coordinator.healthCycleMu.Lock()
	defer coordinator.healthCycleMu.Unlock()
	healthGeneration, testURL := currentHealthCriterion(pool, store)
	healthContext, finishHealthWork, current := pool.BeginHealthWork(healthGeneration)
	if !current {
		return refreshRunResult{Status: "skipped", Error: "health criterion changed before checking"}
	}
	workContext, cancelWork := context.WithCancel(ctx)
	stopHealthCancel := context.AfterFunc(healthContext, cancelWork)
	alive, unreachable, policyFiltered := checkProxiesDetailedContext(workContext, candidates, store.CheckTimeout(cfg.CheckTimeout), store.MaxConcurrent(cfg.MaxConcurrent), store.RequireIPChange(cfg.RequireIPChange), testURL)
	stopHealthCancel()
	cancelWork()
	finishHealthWork()
	if err := ctx.Err(); err != nil {
		return refreshRunResult{Status: "cancelled", Error: err.Error()}
	}
	if coordinator.sourceRefreshBeforeFinalValidation != nil {
		coordinator.sourceRefreshBeforeFinalValidation()
	}

	coordinator.sourceLifecycleMu.Lock()
	defer coordinator.sourceLifecycleMu.Unlock()
	currentSource, exists := store.SourceByID(sourceID)
	if !exists || !revision.matches(currentSource, trigger) {
		return refreshRunResult{Status: "skipped", Error: "source was disabled, deleted, or changed during refresh"}
	}
	catalogRefresh := pool.candidates.begin(deduped, labels, retained, 0)
	if !pool.UpdateWithEnabledSourcesAndPolicy(alive, unreachable, policyFiltered, store.Sources(), healthGeneration) {
		pool.candidates.complete(catalogRefresh, nil, nil, nil)
		return refreshRunResult{Status: "skipped", Error: "health criterion changed during refresh"}
	}
	pool.candidates.complete(catalogRefresh, candidates, alive, policyFiltered)
	if err := pool.FlushCache(); err != nil {
		log.Printf("[cache] source refresh flush failed: %v", err)
	}
	return refreshRunResult{Status: "complete"}
}

// applyRefreshHealthResults closes the source-toggle race at the one point a
// completed network check can make nodes routable. Tests exercise this helper
// directly with a deliberately stale result captured before a source change.
func applyRefreshHealthResults(pool *ProxyPool, store *ConfigStore, coordinator *RefreshCoordinator, alive []Proxy, unreachable, policyFiltered map[string]bool, healthGeneration uint64) bool {
	coordinator.sourceLifecycleMu.Lock()
	defer coordinator.sourceLifecycleMu.Unlock()
	if !pool.UpdateWithEnabledSourcesAndPolicy(alive, unreachable, policyFiltered, store.Sources(), healthGeneration) {
		return false
	}
	return true
}

func currentHealthCriterion(pool *ProxyPool, store *ConfigStore) (uint64, string) {
	generation, checkURL := pool.HealthCriterion()
	if checkURL != "" {
		return generation, checkURL
	}
	checkURL = store.CheckURL()
	pool.SetHealthCriterion(checkURL)
	return pool.HealthCriterion()
}

func currentFullRefreshSources(sources []Source, revisions map[string]sourceRefreshRevision) (map[string]bool, map[string]string) {
	valid := make(map[string]bool, len(sources))
	labels := make(map[string]string, len(sources))
	for _, source := range sources {
		revision, fetched := revisions[source.ID]
		if !fetched {
			continue
		}
		source.Name = safeLogLabel(source.Name)
		if !revision.matches(source, "manual") {
			continue
		}
		valid[source.ID] = true
		labels[source.ID] = source.Name
	}
	return valid, labels
}

func filterCandidatesBySources(candidates []Proxy, validSources map[string]bool) []Proxy {
	write := 0
	for _, candidate := range candidates {
		if !validSources[candidate.SourceName] {
			continue
		}
		candidates[write] = candidate
		write++
	}
	for i := write; i < len(candidates); i++ {
		candidates[i] = Proxy{}
	}
	return candidates[:write:write]
}

func filterRefreshResultsBySources(results []Proxy, validSources map[string]bool) []Proxy {
	write := 0
	for _, result := range results {
		ids := result.SourceIDs[:0]
		for _, sourceID := range result.SourceIDs {
			if validSources[sourceID] {
				ids = append(ids, sourceID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		result.SourceIDs = ids
		results[write] = result
		write++
	}
	for i := write; i < len(results); i++ {
		results[i] = Proxy{}
	}
	return results[:write:write]
}

func filterRefreshOutcomeKeys(outcomes map[string]bool, candidates []Proxy) map[string]bool {
	if len(outcomes) == 0 {
		return outcomes
	}
	validKeys := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		validKeys[candidate.Key()] = true
	}
	for key := range outcomes {
		if !validKeys[key] {
			delete(outcomes, key)
		}
	}
	return outcomes
}

// reconcileFullRefreshCatalog removes attributions whose source revision became
// stale after begin published the checking snapshot. The lifecycle lock held by
// the caller makes this withdrawal atomic with the final pool update/complete.
func reconcileFullRefreshCatalog(catalog *CandidateCatalog, refresh candidateRefresh, validSources map[string]bool, sourceErrors int) {
	catalog.publicationMu.Lock()
	defer catalog.publicationMu.Unlock()
	current := catalog.snapshot.Load()
	if current == nil {
		return
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.generation != refresh.generation {
		return
	}
	staleSource := false
	for _, sourceID := range current.sourceKeys {
		if !validSources[sourceID] {
			staleSource = true
			break
		}
	}
	if !staleSource {
		current.sourceErrors = sourceErrors
		return
	}
	builder := newCandidateSnapshotBuilder(len(current.records))
	for _, record := range current.records {
		builder.appendFilteredRecord(current, record, validSources)
	}
	builder.finalizeCredentialAlternates()
	next := builder.snapshot
	next.generation = current.generation
	next.revision = current.revision + 1
	next.phase = current.phase
	next.sourceErrors = sourceErrors
	next.seenAt = current.seenAt
	next.refreshAttempt = current.refreshAttempt
	next.completedAt = current.completedAt
	rebuildCandidateSourceFacets(next)
	catalog.snapshot.Store(next)
}

func splitHealthInventory(candidates []Proxy) (health []Proxy, resources int) {
	write := 0
	for _, px := range candidates {
		if px.Protocol == "proxyip" {
			resources++
			continue
		}
		candidates[write] = px
		write++
	}
	for i := write; i < len(candidates); i++ {
		candidates[i] = Proxy{}
	}
	return candidates[:write:write], resources
}

// dedupeCandidates keeps protocol variants distinct. A public list may label
// the same address as both SOCKS5 and HTTP; choosing one by a static protocol
// rank can discard the only protocol that actually works. Duplicates of the
// same protocol+address are merged and retain all source attribution.
func dedupeCandidates(list []Proxy) []Proxy {
	if len(list) == 0 {
		return nil
	}
	// Sort and compact in place. A map[string]Proxy used to retain another
	// full copy of a ~500k-item inventory plus one allocated Key string per
	// row, pushing real refreshes beyond a 512 MiB container. Protocol/IP/port
	// ordering is allocation-free and groups exactly the same identities as
	// Proxy.Key; the compact candidate catalog establishes its own wire-key
	// ordering while building the snapshot.
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		if a.IP != b.IP {
			return a.IP < b.IP
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.SourceName != b.SourceName {
			return a.SourceName < b.SourceName
		}
		if a.Username != b.Username {
			return a.Username < b.Username
		}
		return a.Password < b.Password
	})

	write := 0
	for _, px := range list {
		if write == 0 || !sameCandidateIdentity(list[write-1], px) {
			list[write] = px
			write++
			continue
		}
		cur := list[write-1]
		attribution := mergeCandidateSources(cur, px)
		// Pick the lexicographically-smallest source as the stable primary,
		// then fill any missing metadata from the other declaration.
		if px.SourceName != "" && (cur.SourceName == "" || px.SourceName < cur.SourceName) {
			px = mergeCandidateMetadata(px, cur)
			px = mergeCandidateCredentials(px, cur)
			px.SourceNames = attribution
			if len(attribution) > 0 {
				px.SourceName = attribution[0]
			}
			list[write-1] = px
		} else {
			cur = mergeCandidateMetadata(cur, px)
			cur = mergeCandidateCredentials(cur, px)
			cur.SourceNames = attribution
			if len(attribution) > 0 {
				cur.SourceName = attribution[0]
			}
			list[write-1] = cur
		}
	}
	for i := 0; i < write; i++ {
		px := &list[i]
		if len(px.SourceNames) > 0 {
			sort.Strings(px.SourceNames)
			px.SourceName = px.SourceNames[0]
		}
	}
	for i := write; i < len(list); i++ {
		list[i] = Proxy{}
	}
	return list[:write:write]
}

func sameCandidateIdentity(a, b Proxy) bool {
	return a.Protocol == b.Protocol && a.IP == b.IP && a.Port == b.Port
}

func mergeCandidateSources(a, b Proxy) []string {
	out := make([]string, 0, len(a.SourceNames)+len(b.SourceNames)+2)
	appendUnique := func(value string) {
		if value == "" {
			return
		}
		for _, existing := range out {
			if existing == value {
				return
			}
		}
		out = append(out, value)
	}
	appendUnique(a.SourceName)
	for _, value := range a.SourceNames {
		appendUnique(value)
	}
	appendUnique(b.SourceName)
	for _, value := range b.SourceNames {
		appendUnique(value)
	}
	sort.Strings(out)
	return out
}

func restoreCandidateSourceLabels(candidates []Proxy, labels map[string]string) {
	for i := range candidates {
		if len(candidates[i].SourceNames) == 0 {
			if name := strings.TrimSpace(labels[candidates[i].SourceName]); name != "" {
				candidates[i].SourceName = name
			}
			continue
		}
		for j, stableID := range candidates[i].SourceNames {
			name := strings.TrimSpace(labels[stableID])
			if name == "" {
				name = stableID
			}
			candidates[i].SourceNames[j] = name
		}
		sort.Strings(candidates[i].SourceNames)
		names := candidates[i].SourceNames[:0]
		for _, name := range candidates[i].SourceNames {
			if len(names) == 0 || names[len(names)-1] != name {
				names = append(names, name)
			}
		}
		candidates[i].SourceNames = names
		if len(names) > 0 {
			candidates[i].SourceName = names[0]
		}
	}
}

func captureCandidateSourceIDs(candidates []Proxy) {
	for i := range candidates {
		ids := candidates[i].SourceNames
		if len(ids) == 0 && candidates[i].SourceName != "" {
			ids = []string{candidates[i].SourceName}
		}
		candidates[i].SourceIDs = append(candidates[i].SourceIDs[:0], ids...)
	}
}

func mergeCandidateMetadata(dst, src Proxy) Proxy {
	if dst.Country == "" || dst.Country == "Unknown" {
		dst.Country = src.Country
	}
	if dst.City == "" {
		dst.City = src.City
	}
	if dst.Continent == "" {
		dst.Continent = src.Continent
	}
	return dst
}

func mergeCandidateCredentials(primary, other Proxy) Proxy {
	primaryCredential := ProxyCredential{Username: primary.Username, Password: primary.Password}
	seen := map[ProxyCredential]bool{primaryCredential: true}
	for _, credential := range primary.CredentialAlternates {
		seen[credential] = true
	}
	appendAlternative := func(credential ProxyCredential) {
		if seen[credential] || len(primary.CredentialAlternates) >= maxCredentialAlternates {
			return
		}
		seen[credential] = true
		primary.CredentialAlternates = append(primary.CredentialAlternates, credential)
	}
	appendAlternative(ProxyCredential{Username: other.Username, Password: other.Password})
	for _, credential := range other.CredentialAlternates {
		appendAlternative(credential)
	}
	sort.Slice(primary.CredentialAlternates, func(i, j int) bool {
		if primary.CredentialAlternates[i].Username != primary.CredentialAlternates[j].Username {
			return primary.CredentialAlternates[i].Username < primary.CredentialAlternates[j].Username
		}
		return primary.CredentialAlternates[i].Password < primary.CredentialAlternates[j].Password
	})
	return primary
}
