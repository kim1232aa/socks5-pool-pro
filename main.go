package main

import (
	"context"
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

var recheckProbeExitIP = probeExitIPContext

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

	// Establish the policy baseline before deciding whether a persisted pool was
	// validated under the same require-ip-change criterion. When the policy is
	// disabled its fingerprint is baseline-independent, so avoid delaying both
	// listeners on an unnecessary external request.
	if cfg.RequireIPChange {
		InitBaselineExit(cfg.CheckTimeout)
	}

	coordinator := newRefreshCoordinator()
	pool := NewProxyPool()
	pool.SetHealthCriterion(store.CheckURL())
	pool.SetRequireIPChangePolicy(cfg.RequireIPChange)
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
		pool.FlushCache()
		if _, queued := coordinator.triggerFullRecheck(pool); !queued {
			log.Printf("[main] full recheck already pending or active; not re-queued")
		}
	}

	// Background: initial scrape + check, then periodic scrape + manual
	// refresh, all serialized through one goroutine/loop. This runs in the
	// background so the dashboard and cached pool are available right away
	// instead of blocking on it - but it's still a single loop (not a
	// separate "initial" goroutine plus this one) specifically so two
	// refreshPool calls can never run concurrently: a manual refresh
	// (e.g. from saving a new check-url) landing while the startup scrape
	// is still in flight would otherwise race it, and whichever one
	// finished last would "win" with possibly-stale settings even though
	// the newer request should always be the one that takes effect.
	go func() {
		run := func(trigger string, manual bool) {
			var operationID string
			if manual {
				log.Printf("[main] manual refresh triggered")
				operationID = coordinator.beginRefreshOperation()
			} else {
				operationID = coordinator.beginBackgroundRefreshOperation(trigger)
			}
			result := refreshPool(cfg, store, pool, coordinator)
			coordinator.finishRefreshOperation(operationID, result)
		}

		run("startup", false)
		timer := time.NewTimer(cfg.ScrapeInterval)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				run("scheduled", false)
			case <-coordinator.refreshChan:
				run("manual", true)
			}
			// Completion-based scheduling: refreshPool may have set a shorter
			// next scrape deadline when all sources failed, so read it back rather
			// than unconditionally using the full interval.
			_, next := coordinator.scrapeTimes()
			if next.IsZero() || time.Until(next) <= 0 {
				next = time.Now().Add(cfg.ScrapeInterval)
			}
			timer.Reset(time.Until(next))
		}
	}()

	// Establish the shutdown signal context before launching background
	// goroutines so they can all select on it instead of using blocking sleeps.
	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// Background: random rotation of the default (ANY) group every 3-6
	// minutes. If the pool is empty, trigger an immediate refresh instead.
	go func() {
		for {
			delay := 3*time.Minute + time.Duration(rand.Intn(4))*time.Minute
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-signalContext.Done():
				timer.Stop()
				return
			}
			if pool.Size() == 0 {
				log.Printf("[main] pool empty, triggering immediate refresh")
				coordinator.triggerRefresh()
			} else if pool.Size() > 1 {
				pool.RotateSticky(GroupAny)
			}
		}
	}()

	// Background: periodically re-check known nodes' latency/health so the
	// latency/score strategies (and each node's Available flag) stay
	// current between full refreshes. Runs once shortly after startup too -
	// otherwise nodes restored from an older cache file (before the
	// Available field existed) would sit defaulted to Available=false, and
	// so hidden by the dashboard's default filter, for up to 5 minutes.
	go func() {
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		for {
			full := false
			select {
			case <-timer.C:
			case <-coordinator.recheckChan:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-coordinator.fullRecheckChan:
				full = true
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			// A full request is stronger than a bounded one. Coalesce it even if a
			// periodic/manual bounded signal won the select above first.
			if !full {
				select {
				case <-coordinator.fullRecheckChan:
					full = true
				default:
				}
			}
			if cfg.RequireIPChange {
				_, baselineChanged := RefreshBaselineExitWithChange(cfg.CheckTimeout)
				if baselineChanged && pool.SetRequireIPChangePolicy(true) {
					pool.InvalidateHealth(store.CheckURL())
					pool.candidates.ResetHealthOutcomes()
					pool.FlushCache()
					_, _ = coordinator.triggerFullRecheck(pool)
					full = true
					select {
					case <-coordinator.fullRecheckChan:
					default:
					}
				}
			}
			if full {
				reCheckAllAlive(cfg, store, pool, coordinator)
			} else {
				reCheckAlive(cfg, store, pool, coordinator)
			}
			timer.Reset(5 * time.Minute)
		}
	}()

	status := NewStatusServerWithAdminCredentialsAndCoordinator(pool, store, coordinator, cfg.AdminUser, cfg.AdminPass)
	trustedManagementProxies := make([]string, 0, len(cfg.TrustedManagementProxies))
	for _, ip := range cfg.TrustedManagementProxies {
		trustedManagementProxies = append(trustedManagementProxies, ip.String())
	}
	if err := status.SetTrustedManagementProxies(trustedManagementProxies); err != nil {
		log.Fatalf("[main] invalid trusted management proxy: %v", err)
	}
	server := NewServerWithCredentialsAndLimit(
		cfg.ListenAddr, pool, store, cfg.SOCKSUser, cfg.SOCKSPass, cfg.MaxClientConnections,
	)

	// Treat both listeners as one service. A bind/runtime failure in either is
	// fatal, while SIGINT/SIGTERM closes admission, drains handlers to a bounded
	// deadline, and flushes the latest pool state before the process exits.
	serverErrors := make(chan error, 2)
	go func() {
		log.Printf("[status] dashboard at http://%s", cfg.StatusAddr)
		serverErrors <- status.Start(cfg.StatusAddr)
	}()
	go func() { serverErrors <- server.Start() }()

	var exitErr error
	select {
	case <-signalContext.Done():
		log.Printf("[main] shutdown requested")
	case err := <-serverErrors:
		if err != nil {
			exitErr = err
			log.Printf("[main] listener failed: %v", err)
		}
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := status.Shutdown(shutdownContext); err != nil {
		log.Printf("[status] shutdown: %v", err)
	}
	if err := server.Shutdown(shutdownContext); err != nil {
		log.Printf("[server] shutdown: %v", err)
	}
	pool.FlushCache()
	if exitErr != nil {
		log.Fatalf("[main] stopped after listener failure: %v", exitErr)
	}
}

// reCheckAlive re-probes a bounded, rotating slice of known nodes against the
// configured CheckURL and records the outcome, so quality scores stay current
// without an unbounded retained pool turning a five-minute background pass
// into a multi-hour job. No scraping or geo lookups happen here.
func reCheckAlive(cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) {
	_, _ = reCheckNodes(cfg, store, pool, coordinator, pool.RecheckCandidates(cfg.MaxCandidates), pool.Size(), "recheck", "")
}

func reCheckAllAlive(cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) {
	all := pool.All()
	nodes := all[:0]
	for _, px := range all {
		if !px.SourceRetired {
			nodes = append(nodes, px)
		}
	}
	operation := coordinator.beginHealthRecheckOperation(pool, len(nodes))
	generation, completed := reCheckNodes(cfg, store, pool, coordinator, nodes, len(nodes), "full-recheck", operation.ID)
	if completed {
		completed = pool.CompleteHealthRecheck(generation)
		if completed {
			pool.FlushCache()
		}
	}
	coordinator.finishHealthRecheckOperation(operation.ID, completed)
}

func reCheckNodes(cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator, nodes []Proxy, knownTotal int, logLabel, operationID string) (uint64, bool) {
	coordinator.healthCycleMu.Lock()
	defer coordinator.healthCycleMu.Unlock()
	healthGeneration, testURL := currentHealthCriterion(pool, store)
	if len(nodes) == 0 {
		return healthGeneration, true
	}
	healthContext, finishHealthWork, current := pool.BeginHealthWork(healthGeneration)
	if !current {
		return healthGeneration, false
	}
	defer finishHealthWork()
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.MaxConcurrent)
	baseline := BaselineExitIP()
	var outcomeMu sync.Mutex
	checked := make([]Proxy, 0, len(nodes))
	reachableKeys := make(map[string]bool, len(nodes))
	policyFiltered := make(map[string]bool)
checkLoop:
	for _, px := range nodes {
		if px.Protocol == "proxyip" {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-healthContext.Done():
			break checkLoop
		}
		wg.Add(1)
		go func(px Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			nodeContext, cancelNode := context.WithTimeout(healthContext, cfg.CheckTimeout)
			defer cancelNode()
			verified, reachable, latency := checkCredentialCandidates(nodeContext, px, testURL, cfg.CheckTimeout)
			if healthContext.Err() != nil {
				return
			}
			policyAllowed := true
			exitIP := ""
			ipChangeKnown := false
			ipChanged := false
			if reachable && cfg.RequireIPChange {
				exitIP = recheckProbeExitIP(nodeContext, verified, cfg.CheckTimeout)
				if healthContext.Err() != nil {
					return
				}
				policy := evaluateIPChangePolicy(exitIP, baseline, cfg.RequireIPChange)
				ipChangeKnown = policy.IPChangeKnown
				ipChanged = policy.IPChanged
				policyAllowed = policy.PolicyAllowed
			}
			if reachable {
				pool.UpdateVerifiedCredentialsAtGeneration(px.Key(), verified, healthGeneration)
			}
			if !pool.ObserveHealthOutcomeAtGeneration(px.Key(), reachable, policyAllowed, latency.Milliseconds(), healthGeneration) {
				return
			}
			coordinator.recordHealthRecheckOutcome(operationID, reachable, reachable && !policyAllowed)
			if exitIP != "" {
				pool.UpdateGeo(px.Key(), exitIP, "", "", "", ipChanged, ipChangeKnown)
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
		}(px)
	}
	wg.Wait()
	if healthContext.Err() == nil {
		pool.candidates.ApplyHealthOutcomes(checked, reachableKeys, policyFiltered)
	}
	log.Printf("[%s] re-probed %d/%d known nodes against %s", logLabel, len(nodes), knownTotal, safeSourceURL(testURL))
	return healthGeneration, healthContext.Err() == nil
}

// refreshPool fetches every enabled source concurrently, dedups the
// combined candidate list, health-checks it, and installs the result as
// the pool's new live proxy list.
func refreshPool(cfg *Config, store *ConfigStore, pool *ProxyPool, coordinator *RefreshCoordinator) refreshRunResult {
	sources := store.EnabledSources()
	if len(sources) == 0 {
		log.Printf("[main] no enabled sources, skipping refresh")
		return refreshRunResult{Status: "skipped", Error: "no enabled sources"}
	}
	sourceLabels := make(map[string]string, len(sources))
	for _, src := range sources {
		sourceLabels[src.ID] = src.Name
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
				proxies, err := FetchSource(src)
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
	for _, src := range sources {
		jobs <- src
	}
	close(jobs)
	wg.Wait()

	rawCount := len(all)
	deduped := dedupeCandidates(all)
	all = nil
	candidateTotal := len(deduped)
	catalogRefresh := pool.candidates.begin(deduped, sourceLabels, failedSources, sourceErrors)
	captureCandidateSourceIDs(deduped)
	restoreCandidateSourceLabels(deduped, sourceLabels)

	// ProxyIP entries are external reverse-proxy/jump resources for Cloudflare
	// Worker-style deployments rather than generic SOCKS/HTTP upstreams. They
	// remain fully browseable in the candidate catalog, but must not consume
	// scarce forwarding health-check slots or enter the routable pool.
	healthInventory, resourceCount := splitHealthInventory(deduped)
	deduped = nil

	// Some sources (e.g. large community-aggregated lists) return well
	// over 100k raw entries. Checking all of them every cycle would make
	// a refresh take hours and hammer auxiliary lookup services. Cap the
	// checked set, but retain a small cursor state so repeated cycles walk
	// deterministically through the entire source inventory instead of
	// retrying the same failing prefix forever.
	candidates := healthInventory
	if len(candidates) > cfg.MaxCandidates {
		known := make(map[string]bool, pool.Size())
		for _, px := range pool.All() {
			known[px.Key()] = true
		}
		candidates = newCandidateSampler(cfg.DataDir).selectCandidates(healthInventory, known, cfg.MaxCandidates)
		log.Printf("[main] %d candidates exceed max-candidates=%d, selecting an unseen-first source/protocol-balanced rotating subset (rest deferred)",
			len(healthInventory), cfg.MaxCandidates)
	}
	// Once a bounded sample owns the selected Proxy values, release the large
	// raw-capacity backing array before network checks (which can run for many
	// seconds). When the inventory is already below the cap, candidates still
	// aliases it and correctly keeps that small backing alive.
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
	alive, unreachable, policyFiltered := checkProxiesDetailedContext(healthContext, candidates, cfg.CheckTimeout, cfg.MaxConcurrent, cfg.RequireIPChange, testURL)
	finishHealthWork()
	applied := applyRefreshHealthResults(pool, store, coordinator, alive, unreachable, policyFiltered, healthGeneration)
	if !applied {
		pool.candidates.complete(catalogRefresh, nil, nil, nil)
		log.Printf("[main] discarded health results because the check criterion changed during refresh")
		return refreshRunResult{Status: "skipped", SourceErrors: sourceErrors, Error: "health criterion changed during refresh; exhaustive recheck queued"}
	}
	// Reconcile against the current source configuration while the lifecycle
	// lock is still held. This closes the race where a refresh started before a
	// source toggle/delete and would otherwise revive its stale credentials.
	pool.candidates.complete(catalogRefresh, candidates, alive, policyFiltered)

	coordinator.recordScrape(ScrapeInfo{
		Raw: rawCount, Candidates: candidateTotal, Checked: len(candidates),
		FreshAlive: len(alive), SourceTotal: len(sources), SourceError: sourceErrors,
	}, cfg.ScrapeInterval)
	// Persist the new pool membership immediately rather than relying on the
	// 500ms debounce timer. A process kill between refresh completion and the
	// debounced write would otherwise lose the freshly discovered nodes.
	pool.FlushCache()
	log.Printf("[main] pool refreshed: %d fresh alive / %d checked against %s; %d known total (from %d sources, %d errors, %d raw, %d protocol-aware candidates, %d non-routable resources)",
		len(alive), len(candidates), safeSourceURL(testURL), pool.Size(), len(sources), sourceErrors, rawCount, candidateTotal, resourceCount)
	status := "complete"
	if sourceErrors > 0 {
		status = "partial"
	}
	return refreshRunResult{Status: status, SourceErrors: sourceErrors}
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
