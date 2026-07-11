package main

import (
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	lastScrapeTime time.Time
	nextScrapeTime time.Time
	lastScrapeInfo ScrapeInfo
	scrapeMu       sync.RWMutex
	refreshChan    = make(chan struct{}, 1) // manual refresh trigger
	recheckChan    = make(chan struct{}, 1)
	healthCycleMu  sync.Mutex // full refresh and whole-pool recheck must not overlap
	refreshOpMu    sync.RWMutex
	refreshOpSeq   uint64
	refreshActive  *RefreshOperation
	refreshPending *RefreshOperation
	refreshLast    *RefreshOperation
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
// during a running job creates one bounded follow-up job in refreshChan.
type RefreshOperation struct {
	ID           string `json:"id"`
	Status       string `json:"status"` // queued, running, complete, partial, skipped
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

func getScrapeTimes() (last, next time.Time) {
	scrapeMu.RLock()
	defer scrapeMu.RUnlock()
	return lastScrapeTime, nextScrapeTime
}

func getScrapeInfo() ScrapeInfo {
	scrapeMu.RLock()
	defer scrapeMu.RUnlock()
	return lastScrapeInfo
}

func getScrapeStatusSnapshot() scrapeStatusSnapshot {
	scrapeMu.RLock()
	defer scrapeMu.RUnlock()
	return scrapeStatusSnapshot{Last: lastScrapeTime, Next: nextScrapeTime, Info: lastScrapeInfo}
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

	pool := NewProxyPool()

	// Seed from the on-disk cache so the pool is usable immediately, then
	// enable write-back so every refresh keeps the cache fresh.
	cache := newPoolCache(cfg.DataDir)
	if fwd, info, stats := cache.load(); len(fwd) > 0 || len(info) > 0 {
		pool.Prime(fwd, info)
		pool.restoreStats(stats)
	}
	pool.SetCache(cache)

	// Measure our own direct egress once, as the baseline for detecting
	// transparent proxies that don't actually change the exit IP.
	InitBaselineExit(cfg.CheckTimeout)

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
		refreshPool(cfg, store, pool)
		ticker := time.NewTicker(cfg.ScrapeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				refreshPool(cfg, store, pool)
			case <-refreshChan:
				log.Printf("[main] manual refresh triggered")
				operationID := beginRefreshOperation()
				result := refreshPool(cfg, store, pool)
				finishRefreshOperation(operationID, result)
				ticker.Reset(cfg.ScrapeInterval)
			}
		}
	}()

	// Background: random rotation of the default (ANY) group every 3-6
	// minutes. If the pool is empty, trigger an immediate refresh instead.
	go func() {
		for {
			delay := 3*time.Minute + time.Duration(rand.Intn(4))*time.Minute
			time.Sleep(delay)
			if pool.Size() == 0 {
				log.Printf("[main] pool empty, triggering immediate refresh")
				TriggerRefresh()
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
			select {
			case <-timer.C:
			case <-recheckChan:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			if BaselineExitIP() == "" {
				InitBaselineExit(cfg.CheckTimeout)
			}
			reCheckAlive(cfg, store, pool)
			timer.Reset(5 * time.Minute)
		}
	}()

	// Background: status dashboard
	go func() {
		status := NewStatusServerWithAdminCredentials(pool, store, cfg.AdminUser, cfg.AdminPass)
		log.Printf("[status] dashboard at http://%s", cfg.StatusAddr)
		if err := status.Start(cfg.StatusAddr); err != nil {
			log.Printf("[status] failed to start: %v", err)
		}
	}()

	// Start SOCKS5 server (blocks)
	server := NewServerWithCredentials(cfg.ListenAddr, pool, store, cfg.SOCKSUser, cfg.SOCKSPass)
	log.Fatal(server.Start())
}

// reCheckAlive re-probes a bounded, rotating slice of known nodes against the
// configured CheckURL and records the outcome, so quality scores stay current
// without an unbounded retained pool turning a five-minute background pass
// into a multi-hour job. No scraping or geo lookups happen here.
func reCheckAlive(cfg *Config, store *ConfigStore, pool *ProxyPool) {
	healthCycleMu.Lock()
	defer healthCycleMu.Unlock()
	knownTotal := pool.Size()
	nodes := pool.RecheckCandidates(cfg.MaxCandidates)
	if len(nodes) == 0 {
		return
	}
	testURL := store.CheckURL()
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.MaxConcurrent)
	for _, px := range nodes {
		if px.Protocol == "proxyip" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(px Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			ok := checkURL(px, testURL, cfg.CheckTimeout)
			latency := time.Since(start).Milliseconds()
			pool.ObserveHealthResult(px.Key(), ok, latency)
		}(px)
	}
	wg.Wait()
	log.Printf("[recheck] re-probed %d/%d known nodes against %s", len(nodes), knownTotal, safeSourceURL(testURL))
}

// refreshPool fetches every enabled source concurrently, dedups the
// combined candidate list, health-checks it, and installs the result as
// the pool's new live proxy list.
func refreshPool(cfg *Config, store *ConfigStore, pool *ProxyPool) refreshRunResult {
	healthCycleMu.Lock()
	defer healthCycleMu.Unlock()
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
	for _, src := range sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			proxies, err := FetchSource(src)
			if err != nil {
				log.Printf("[error] scrape %s failed: %v", src.Name, err)
				mu.Lock()
				sourceErrors++
				failedSources[src.ID] = true
				mu.Unlock()
				return
			}
			for i := range proxies {
				// During dedupe/catalog construction SourceName carries the stable
				// ConfigStore ID. It is translated back to the display name before
				// candidates reach the checker/pool, avoiding extra attribution
				// fields and per-entry slices on a ~500k-item transient inventory.
				proxies[i].SourceName = src.ID
			}
			mu.Lock()
			all = append(all, proxies...)
			mu.Unlock()
		}(src)
	}
	wg.Wait()

	rawCount := len(all)
	deduped := dedupeCandidates(all)
	all = nil
	candidateTotal := len(deduped)
	catalogRefresh := pool.candidates.begin(deduped, sourceLabels, failedSources, sourceErrors)
	restoreCandidateSourceLabels(deduped, sourceLabels)

	// ProxyIP entries are external reverse-proxy/jump resources for Cloudflare
	// Worker-style deployments rather than generic SOCKS/HTTP upstreams. They
	// remain fully browseable in the candidate catalog, but must not consume
	// scarce forwarding health-check slots or enter the routable pool.
	healthInventory, resourceCount := splitHealthInventory(deduped)
	deduped = nil

	// Some sources (e.g. large community-aggregated lists) return well
	// over 100k raw entries. Checking all of them every cycle would make
	// a refresh take hours and hammer ip-api.com's rate limit. Cap the
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
	testURL := store.CheckURL()
	alive, unreachable, policyFiltered := checkProxiesDetailed(candidates, cfg.CheckTimeout, cfg.MaxConcurrent, cfg.RequireIPChange, testURL)
	pool.Update(alive, unreachable)
	// A node that is reachable but violates require-ip-change must not remain
	// routable merely because it survived in the known pool from an older
	// cycle. This is a policy state change, not a connection failure, so it
	// deliberately does not increment failure statistics.
	for key := range policyFiltered {
		pool.SetAvailable(key, false)
	}
	pool.candidates.complete(catalogRefresh, candidates, alive, policyFiltered)

	scrapeMu.Lock()
	lastScrapeTime = time.Now()
	nextScrapeTime = lastScrapeTime.Add(cfg.ScrapeInterval)
	lastScrapeInfo = ScrapeInfo{
		Raw: rawCount, Candidates: candidateTotal, Checked: len(candidates),
		FreshAlive: len(alive), SourceTotal: len(sources), SourceError: sourceErrors,
	}
	scrapeMu.Unlock()

	log.Printf("[main] pool refreshed: %d fresh alive / %d checked against %s; %d known total (from %d sources, %d errors, %d raw, %d protocol-aware candidates, %d non-routable resources)",
		len(alive), len(candidates), safeSourceURL(testURL), pool.Size(), len(sources), sourceErrors, rawCount, candidateTotal, resourceCount)
	status := "complete"
	if sourceErrors > 0 {
		status = "partial"
	}
	return refreshRunResult{Status: status, SourceErrors: sourceErrors}
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
		return a.SourceName < b.SourceName
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
			px.SourceNames = attribution
			if len(attribution) > 0 {
				px.SourceName = attribution[0]
			}
			list[write-1] = px
		} else {
			cur = mergeCandidateMetadata(cur, px)
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

func mergeCandidateMetadata(dst, src Proxy) Proxy {
	if dst.Username == "" {
		dst.Username, dst.Password = src.Username, src.Password
	}
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

// sampleBalanced guarantees every source/protocol bucket a small share, then
// fills the remainder proportionally from the combined leftovers. This stops
// one very large feed from crowding all smaller sources out of a cycle.
func sampleBalanced(list []Proxy, limit int) []Proxy {
	if limit <= 0 || len(list) == 0 {
		return nil
	}
	if len(list) <= limit {
		return append([]Proxy(nil), list...)
	}
	buckets := make(map[string][]Proxy)
	for _, px := range list {
		name := px.SourceName
		if name == "" {
			name = "unknown"
		}
		key := name + "\x00" + px.Protocol
		buckets[key] = append(buckets[key], px)
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
		s := buckets[key]
		rand.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
		buckets[key] = s
	}
	sort.Strings(keys)
	quota := limit / (len(keys) * 4)
	if quota < 1 {
		quota = 1
	}
	if quota > 50 {
		quota = 50
	}

	out := make([]Proxy, 0, limit)
	leftovers := make([]Proxy, 0, len(list))
	for _, key := range keys {
		bucket := buckets[key]
		take := quota
		if take > len(bucket) {
			take = len(bucket)
		}
		if take > limit-len(out) {
			take = limit - len(out)
		}
		out = append(out, bucket[:take]...)
		leftovers = append(leftovers, bucket[take:]...)
	}
	rand.Shuffle(len(leftovers), func(i, j int) { leftovers[i], leftovers[j] = leftovers[j], leftovers[i] })
	if remaining := limit - len(out); remaining > 0 {
		out = append(out, leftovers[:remaining]...)
	}
	return out
}

// RequestRefresh queues at most one follow-up operation and returns a stable
// job ID callers can inspect through /api/refresh/status. accepted=false means
// the caller joined an operation that was already queued.
func RequestRefresh() (operation RefreshOperation, accepted bool) {
	refreshOpMu.Lock()
	if refreshPending != nil {
		operation = *refreshPending
		refreshOpMu.Unlock()
		return operation, false
	}
	refreshOpSeq++
	now := time.Now().UTC()
	job := &RefreshOperation{
		ID:     fmt.Sprintf("refresh-%d-%d", now.UnixNano(), refreshOpSeq),
		Status: "queued", RequestedAt: now.Format(time.RFC3339Nano),
	}
	refreshPending = job
	operation = *job
	refreshOpMu.Unlock()

	select {
	case refreshChan <- struct{}{}:
		return operation, true
	default:
		// Defensive fallback: a buffered signal should always correspond to the
		// pending operation above. Treat it as coalesced rather than creating an
		// unbounded queue or claiming a second job was accepted.
		return operation, false
	}
}

// TriggerRefresh preserves the historical fire-and-forget call sites.
func TriggerRefresh() {
	_, _ = RequestRefresh()
}

func beginRefreshOperation() string {
	refreshOpMu.Lock()
	defer refreshOpMu.Unlock()
	job := refreshPending
	refreshPending = nil
	if job == nil {
		refreshOpSeq++
		now := time.Now().UTC()
		job = &RefreshOperation{
			ID:     fmt.Sprintf("refresh-%d-%d", now.UnixNano(), refreshOpSeq),
			Status: "queued", RequestedAt: now.Format(time.RFC3339Nano),
		}
	}
	job.Status = "running"
	job.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	refreshActive = job
	return job.ID
}

func finishRefreshOperation(id string, result refreshRunResult) {
	refreshOpMu.Lock()
	defer refreshOpMu.Unlock()
	if refreshActive == nil || refreshActive.ID != id {
		return
	}
	refreshActive.Status = result.Status
	if refreshActive.Status == "" {
		refreshActive.Status = "complete"
	}
	refreshActive.SourceErrors = result.SourceErrors
	refreshActive.Error = result.Error
	refreshActive.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	completed := *refreshActive
	refreshLast = &completed
	refreshActive = nil
}

func getRefreshOperationStatus() RefreshOperationStatus {
	refreshOpMu.RLock()
	defer refreshOpMu.RUnlock()
	clone := func(operation *RefreshOperation) *RefreshOperation {
		if operation == nil {
			return nil
		}
		copy := *operation
		return &copy
	}
	state := "idle"
	if refreshPending != nil {
		state = "queued"
	}
	if refreshActive != nil {
		state = "running"
	}
	return RefreshOperationStatus{
		State:  state,
		Active: clone(refreshActive), Pending: clone(refreshPending), Last: clone(refreshLast),
	}
}

// TriggerRecheck asks the serialized health worker to apply the current check
// URL to every known node as soon as possible. Multiple requests collapse into
// one pending run.
func TriggerRecheck() {
	select {
	case recheckChan <- struct{}{}:
	default:
	}
}
