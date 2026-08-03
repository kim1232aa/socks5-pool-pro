# Background Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make baseline detection resilient and give administrators independent, visible source-refresh and full-pool-recheck schedules, with automatic recovery and removal after three completed full-recheck failures.

**Architecture:** Keep source refresh in the existing `RefreshCoordinator` source queue, add a separate persisted full-recheck interval and schedule timestamps, and make exhaustive rechecks use a distinct outcome path that may recover unavailable nodes or remove them after three failures. Baseline discovery races three direct HTTPS services under one shared context deadline.

**Tech Stack:** Go 1.23+, standard library HTTP/context/concurrency, embedded vanilla JavaScript dashboard, Docker Compose.

## Global Constraints

- Baseline endpoints are `https://api64.ipify.org/`, `https://checkip.amazonaws.com/`, and `https://ident.me/`.
- Baseline requests bypass `HTTP_PROXY`/`HTTPS_PROXY`, reject redirects, share one caller timeout, and retain the previous baseline if all fail.
- Source refresh and full-pool recheck intervals are persisted independently; zero means follow the CLI default.
- Exhaustive full rechecks include available and unavailable forwarding proxies.
- A successful exhaustive observation recovers an unavailable proxy and resets its consecutive failure count.
- Three completed exhaustive failures remove the forwarding proxy and its statistics; canceled/stale cycles do not count.
- Candidate-catalog membership is not deleted when a forwarding proxy is removed.
- Do not add dependencies.

---

### Task 1: Complete baseline endpoint racing

**Files:**
- Modify: `checker.go:19-34,323-470`
- Test: `checker_test.go:539-605`

**Interfaces:**
- Produces: `refreshBaselineExitWithURLsChangeContext(parent context.Context, endpoints []string, timeout time.Duration) (success, changed bool)`.
- Preserves: `RefreshBaselineExitWithChangeContext`, `refreshBaselineExitWithURLChangeContext`, and `newDirectHTTPClient` callers.

- [ ] **Step 1: Keep the existing failing/green race test and add all-fail retention coverage**

Add a test that installs an old baseline, serves only invalid/503 responses, calls `refreshBaselineExitWithURLsChangeContext`, and asserts `success=false`, `changed=false`, and the old baseline remains.

- [ ] **Step 2: Run focused tests**

Run:

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.23 go test -buildvcs=false -run '^TestRefreshBaselineExit' ./...
```

Expected before the final implementation: the new retention test or existing race test fails for missing/incorrect race behavior.

- [ ] **Step 3: Finish the minimal implementation**

Use one `context.WithTimeout`, one result channel buffered to `len(endpoints)`, one no-proxy client per endpoint, cancel on the first valid IP, and update `baselineExitIP` only after validation. Avoid goroutine leaks when the caller returns.

- [ ] **Step 4: Run focused tests again**

Expected: all `TestRefreshBaselineExit*` tests pass.

---

### Task 2: Persist and validate two independent schedule settings

**Files:**
- Modify: `config.go:16-30,86-106,139-170`
- Modify: `sourcestore.go:115-138` and runtime option accessors
- Test: `config_test.go`
- Test: `sourcestore_auto_refresh_test.go`
- Test: `status_check_options_test.go`

**Interfaces:**
- Add to `Config`: `FullRecheckInterval time.Duration`.
- Add CLI flag: `-full-recheck-interval`, default `30m`.
- Add to `PoolConfig`: `SourceRefreshIntervalSeconds int` and `FullRecheckIntervalSeconds int`; zero follows CLI defaults.
- Add accessors: `SourceRefreshInterval(defaultValue time.Duration) time.Duration` and `FullRecheckInterval(defaultValue time.Duration) time.Duration`.

- [ ] **Step 1: Write failing configuration tests**

Cover default values, explicit CLI parsing, persisted overrides, zero/default fallback, and validation of a bounded range from 60 seconds through 7 days.

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.23 go test -buildvcs=false -run 'Schedule|RefreshInterval|FullRecheckInterval' ./...
```

- [ ] **Step 3: Implement fields, accessors, and validation**

Follow existing `MaxConcurrent`, `CheckTimeoutSeconds`, and `MaxCandidates` override patterns. Keep old cache/config files compatible through `omitempty` and zero defaults.

- [ ] **Step 4: Run the focused tests and verify GREEN**

Expected: all new schedule configuration tests pass.

---

### Task 3: Add full-recheck schedule state and runtime timing

**Files:**
- Modify: `refresh_coordinator.go:10-100,324-420`
- Modify: `main.go:339-394`
- Test: `status_source_refresh_test.go`
- Test: `background_lifecycle_test.go`

**Interfaces:**
- Add coordinator methods `fullRecheckTimes() (last, next time.Time)` and `recordFullRecheck(completedAt time.Time, interval time.Duration)`.
- The health worker reads `store.FullRecheckInterval(cfg.FullRecheckInterval)` after each completed cycle and resets its timer to that effective value.
- Source scheduling reads `store.SourceRefreshInterval(cfg.ScrapeInterval)` instead of only the startup flag.

- [ ] **Step 1: Write failing timing tests**

Test independent source/full-recheck timestamps, runtime interval changes applying to the next timer reset, and cancellation not recording a completed full recheck.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.23 go test -buildvcs=false -run 'FullRecheck.*Time|Independent.*Schedule|Background.*Recheck' ./...
```

- [ ] **Step 3: Implement separate coordinator state and timer use**

Do not couple full-recheck timestamps to `recordScrape`. Preserve the lightweight 5-minute bounded recheck loop only if it remains separate from exhaustive cycles; exhaustive cycles must run on the configured full-recheck interval.

- [ ] **Step 4: Run focused tests and verify GREEN**

Expected: schedule timing tests pass without sleeps longer than a few milliseconds; inject deterministic times/helpers where needed.

---

### Task 4: Make exhaustive rechecks recover and remove nodes after three failures

**Files:**
- Modify: `pool.go:178-297,960-1023,1407-1446`
- Modify: `main.go:455-581`
- Test: `pool_test.go:178-226,520-555`
- Test: `main_test.go:205-248`

**Interfaces:**
- Add an exhaustive result method such as `ObserveFullHealthOutcomeAtGeneration(key string, reachable, policyAllowed bool, latencyMs int64, generation uint64) (observed, removed bool)`.
- Add an exhaustive candidate selector that returns every non-retired forwarding proxy, including `HealthFailureTerminal` entries.
- Periodic bounded automatic checks retain their current admission rule; only exhaustive full checks may recover terminal nodes and remove after the third failure.

- [ ] **Step 1: Write failing pool tests**

Cover: terminal/unavailable node included in exhaustive candidates; exhaustive success clears terminal state and resets failures; first and second exhaustive failures retain the node; third removes proxy plus stats; success between failures resets the counter; stale generation and canceled work do not remove.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.23 go test -buildvcs=false -run 'Full.*Recheck|Exhaustive.*Health|Three.*Failure' ./...
```

- [ ] **Step 3: Implement the distinct exhaustive outcome path**

Keep mutation under `ProxyPool.mu`, update `proxyIndex`, group sticky state, revisions, and persistence using existing deletion helpers. Do not remove candidate-catalog records. Do not reuse the manual-only method without distinguishing exhaustive deletion semantics.

- [ ] **Step 4: Wire `reCheckAllAliveContext` to all retained forwarding nodes**

Replace `EligibleForAutoRecheck` filtering in the exhaustive path. Apply exhaustive outcomes only after a current-generation observation. Count and log removed nodes at cycle completion.

- [ ] **Step 5: Run focused tests and verify GREEN**

Expected: recovery/removal tests pass; existing periodic-terminal tests remain unchanged and pass.

---

### Task 5: Expose schedule controls through management API

**Files:**
- Modify: `status_check_options.go:1-180`
- Test: `status_check_options_test.go:1-380`

**Interfaces:**
- Extend check/settings response with effective `source_refresh_interval_seconds`, `full_recheck_interval_seconds`, and corresponding override values.
- Extend the POST request with optional override integers; zero follows CLI defaults.
- Schedule-only changes persist without invalidating health results or forcing an immediate full recheck.

- [ ] **Step 1: Write failing API contract tests**

Test GET effective/default values, POST persistence, bounds errors, zero reset, and unchanged health generation for schedule-only changes.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.23 go test -buildvcs=false -run 'CheckOptions.*Schedule|Schedule.*Settings' ./...
```

- [ ] **Step 3: Implement request/response fields and persistence**

Follow existing check-option JSON and `writeErrCode` conventions. Return precise validation errors.

- [ ] **Step 4: Run focused tests and verify GREEN**

Expected: all management API tests pass.

---

### Task 6: Report both schedules in compact status

**Files:**
- Modify: `status_views.go:86-102,223-305`
- Modify: `status_handlers.go`
- Modify: `status_util.go`
- Test: `status_api_contract_test.go:230-270`

**Interfaces:**
- Add RFC3339 and display fields for source refresh and full recheck: `last_source_refresh_at`, `next_source_refresh_at`, `last_full_recheck_at`, `next_full_recheck_at`.
- Preserve existing `last_scrape_at`/`next_scrape_at` as compatibility aliases for source refresh.

- [ ] **Step 1: Write failing compact-status contract tests**

Install deterministic coordinator times and assert both schedule pairs in `/api/status?compact=1`.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.23 go test -buildvcs=false -run 'Status.*Schedule|RFC3339.*Scrape' ./...
```

- [ ] **Step 3: Implement fields without adding full proxy payloads**

Keep compact polling O(pool size) or better and avoid cloning proxy lists.

- [ ] **Step 4: Run focused tests and verify GREEN**

Expected: compact/full status contracts both pass.

---

### Task 7: Add dashboard schedule controls and status rendering

**Files:**
- Modify: `web/dashboard.html:64-94`
- Modify: `web/dashboard.js:1687-1745,1950-1960,2100-2220,2660-2664`
- Modify: `web/dashboard.css` only if existing form styles cannot render the two numeric inputs cleanly
- Test: existing embedded-asset/API contract tests and any dashboard static-contract test files

**Interfaces:**
- Add inputs `opt-source-refresh-interval` and `opt-full-recheck-interval`, expressed in minutes for administrators and converted to seconds at the API boundary.
- Render separate recent/next source refresh and full recheck values from compact status.

- [ ] **Step 1: Write failing static/dashboard contract tests**

Assert both input IDs, request payload fields, response population, and both timeline labels exist.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.23 go test -buildvcs=false -run 'Dashboard.*Schedule|CheckOptions.*Dashboard' ./...
```

- [ ] **Step 3: Implement controls and rendering**

Reuse the existing check-options save flow. Keep the 15-second visible-page compact poll and 30-second node-page refresh; update copy to explain exhaustive deletion after three failed full cycles.

- [ ] **Step 4: Run focused tests and verify GREEN**

Expected: dashboard contract tests pass.

---

### Task 8: Documentation, full verification, and runtime validation

**Files:**
- Modify: `README.md` schedule and health semantics sections
- Verify: all modified files

- [ ] **Step 1: Update operator documentation**

Document both defaults/overrides, dashboard controls, exhaustive recovery, three-failure removal, baseline provider race, and the distinction between forwarding-pool removal and candidate-catalog retention.

- [ ] **Step 2: Run formatting and complete tests**

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.23 sh -c 'gofmt -w *.go && go test -buildvcs=false ./...'
docker run --rm -v "$PWD:/src" -w /src golang:1.23 go build -buildvcs=false ./...
git diff --check
```

Expected: all commands exit 0.

- [ ] **Step 3: Rebuild and restart the application**

```bash
docker compose up -d --build
```

Wait for `healthy`, then verify compact status includes a non-empty baseline through the baseline endpoint and both schedule pairs. Trigger or shorten a test schedule through the management API, observe a completed full recheck, and restore the desired production interval.

- [ ] **Step 4: Inspect final working tree**

```bash
git status --short --branch
git diff --stat
git diff --check
```

Do not commit or push without the user's separate explicit confirmation.
