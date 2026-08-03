package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRequestSourceRefreshCoalescesSameSource(t *testing.T) {
	coordinator := newRefreshCoordinator()
	source := Source{ID: "source-a", Name: "A", Enabled: true}
	first, accepted := coordinator.requestSourceRefresh(source, "manual")
	if !accepted {
		t.Fatal("first source refresh was not accepted")
	}
	second, accepted := coordinator.requestSourceRefresh(source, "manual")
	if accepted || second.ID != first.ID {
		t.Fatalf("duplicate refresh = (%+v, %v), want same operation and accepted=false", second, accepted)
	}
	if got := <-coordinator.sourceRefreshChan; got != source.ID {
		t.Fatalf("queued source = %q, want %q", got, source.ID)
	}
}

func TestRequestSourceRefreshDistinguishesSourceEnabledFromAutoRefreshEnabled(t *testing.T) {
	coordinator := newRefreshCoordinator()
	manualOnly := Source{ID: "manual-only", Name: "Manual only", Enabled: true, AutoRefreshEnabled: false}
	if _, accepted := coordinator.requestSourceRefresh(manualOnly, "manual"); !accepted {
		t.Fatal("manual refresh was rejected because automatic refresh is disabled")
	}
	if got := <-coordinator.sourceRefreshChan; got != manualOnly.ID {
		t.Fatalf("queued source = %q, want %q", got, manualOnly.ID)
	}
	if _, ok := coordinator.beginSourceRefresh(manualOnly.ID); !ok {
		t.Fatal("manual refresh did not become active")
	}
	coordinator.finishSourceRefresh(manualOnly.ID, refreshRunResult{Status: "complete"})

	if operation, accepted := coordinator.requestSourceRefresh(manualOnly, "scheduled"); accepted || operation.Status != "rejected" {
		t.Fatalf("scheduled refresh = (%+v, %v), want rejected", operation, accepted)
	}
	disabled := Source{ID: "disabled", Name: "Disabled", Enabled: false, AutoRefreshEnabled: true}
	if operation, accepted := coordinator.requestSourceRefresh(disabled, "manual"); accepted || operation.Status != "rejected" {
		t.Fatalf("disabled manual refresh = (%+v, %v), want rejected", operation, accepted)
	}
	select {
	case sourceID := <-coordinator.sourceRefreshChan:
		t.Fatalf("rejected source %q was queued", sourceID)
	default:
	}
}

func TestQueueDueSourceRefreshesSkipsDisabledAutoUpdate(t *testing.T) {
	store := &ConfigStore{cfg: PoolConfig{Sources: []Source{
		{ID: "due", Name: "Due", Enabled: true, AutoRefreshEnabled: true},
		{ID: "disabled-auto", Name: "Disabled auto", Enabled: true, AutoRefreshEnabled: false},
		{ID: "disabled-source", Name: "Disabled source", Enabled: false, AutoRefreshEnabled: true},
	}}}
	coordinator := newRefreshCoordinator()
	now := time.Now()
	coordinator.markSourcesRefreshed(store.Sources(), now.Add(-2*time.Hour))
	if got := coordinator.queueDueSourceRefreshes(store, time.Hour, now); got != 1 {
		t.Fatalf("queued %d due sources, want 1", got)
	}
	if got := <-coordinator.sourceRefreshChan; got != "due" {
		t.Fatalf("queued source %q, want due", got)
	}
}

func TestIndependentSourceAndFullRecheckScheduleTimes(t *testing.T) {
	coordinator := newRefreshCoordinator()
	sourceCompleted := time.Date(2026, time.August, 2, 10, 0, 0, 0, time.UTC)
	fullCompleted := sourceCompleted.Add(5 * time.Minute)

	coordinator.recordSourceRefresh(sourceCompleted, 20*time.Minute)
	coordinator.recordFullRecheck(fullCompleted, 30*time.Minute)

	sourceLast, sourceNext := coordinator.scrapeTimes()
	fullLast, fullNext := coordinator.fullRecheckTimes()
	if !sourceLast.Equal(sourceCompleted) || !sourceNext.Equal(sourceCompleted.Add(20*time.Minute)) {
		t.Fatalf("source schedule = (%s, %s)", sourceLast, sourceNext)
	}
	if !fullLast.Equal(fullCompleted) || !fullNext.Equal(fullCompleted.Add(30*time.Minute)) {
		t.Fatalf("full recheck schedule = (%s, %s)", fullLast, fullNext)
	}
}

func TestCompletedFullRecheckUsesCurrentRuntimeInterval(t *testing.T) {
	store := &ConfigStore{cfg: PoolConfig{FullRecheckIntervalSeconds: 60}}
	coordinator := newRefreshCoordinator()
	first := time.Date(2026, time.August, 2, 11, 0, 0, 0, time.UTC)
	recordCompletedFullRecheck(coordinator, store, 30*time.Minute, first)
	_, next := coordinator.fullRecheckTimes()
	if want := first.Add(time.Minute); !next.Equal(want) {
		t.Fatalf("first next full recheck = %s, want %s", next, want)
	}

	store.mu.Lock()
	store.cfg.FullRecheckIntervalSeconds = 120
	store.mu.Unlock()
	second := first.Add(10 * time.Minute)
	recordCompletedFullRecheck(coordinator, store, 30*time.Minute, second)
	_, next = coordinator.fullRecheckTimes()
	if want := second.Add(2 * time.Minute); !next.Equal(want) {
		t.Fatalf("updated next full recheck = %s, want %s", next, want)
	}
}

func TestQueueDueSourceRefreshesUsesRuntimeGlobalOverride(t *testing.T) {
	store := &ConfigStore{cfg: PoolConfig{
		Sources:                      []Source{{ID: "source-a", Name: "A", Enabled: true, AutoRefreshEnabled: true}},
		SourceRefreshIntervalSeconds: 3600,
	}}
	coordinator := newRefreshCoordinator()
	now := time.Now()
	coordinator.markSourcesRefreshed(store.Sources(), now.Add(-30*time.Minute))
	if got := coordinator.queueDueSourceRefreshes(store, 10*time.Minute, now); got != 0 {
		t.Fatalf("queued %d sources under one-hour override, want 0", got)
	}

	store.mu.Lock()
	store.cfg.SourceRefreshIntervalSeconds = 60
	store.mu.Unlock()
	if got := coordinator.queueDueSourceRefreshes(store, 10*time.Minute, now); got != 1 {
		t.Fatalf("queued %d sources after runtime override, want 1", got)
	}
}

func TestSignalWakeWithoutPendingDoesNotSynthesizeFullRecheck(t *testing.T) {
	coordinator := newRefreshCoordinator()
	pool := NewProxyPool()
	pool.SetHealthCriterion(defaultCheckURL)

	if operation, claimed := coordinator.claimHealthRecheckOperation(pool, 3, false); claimed || operation.ID != "" {
		t.Fatalf("signal wake without pending operation claimed %#v", operation)
	}
	if state := coordinator.healthRecheckOperationStatus(); state.State != "idle" || state.Active != nil {
		t.Fatalf("phantom signal created active operation: %#v", state)
	}

	operation, claimed := coordinator.claimHealthRecheckOperation(pool, 3, true)
	if !claimed || operation.ID == "" || operation.Status != "running" {
		t.Fatalf("timer wake did not synthesize scheduled operation: %#v claimed=%v", operation, claimed)
	}
}

func TestNextSourceRefreshTimeUsesEarliestPerSourceDeadline(t *testing.T) {
	coordinator := newRefreshCoordinator()
	origin := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	sources := []Source{
		{ID: "global", Name: "Global", Enabled: true, AutoRefreshEnabled: true},
		{ID: "fast", Name: "Fast", Enabled: true, AutoRefreshEnabled: true, RefreshIntervalSeconds: 90},
		{ID: "manual", Name: "Manual", Enabled: true, AutoRefreshEnabled: false, RefreshIntervalSeconds: 60},
		{ID: "disabled", Name: "Disabled", Enabled: false, AutoRefreshEnabled: true, RefreshIntervalSeconds: 60},
	}
	coordinator.markSourcesRefreshed([]Source{sources[0]}, origin)
	coordinator.markSourcesRefreshed([]Source{sources[1]}, origin.Add(5*time.Minute))
	coordinator.markSourcesRefreshed([]Source{sources[2], sources[3]}, origin.Add(-time.Hour))

	got := coordinator.nextSourceRefreshTime(sources, 20*time.Minute, origin.Add(5*time.Minute))
	want := origin.Add(6*time.Minute + 30*time.Second)
	if !got.Equal(want) {
		t.Fatalf("next source refresh = %s, want earliest enabled automatic deadline %s", got, want)
	}
}

func TestCancelledBackgroundFullRecheckDoesNotRecordSchedule(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	coordinator := newRefreshCoordinator()
	cfg := &Config{FullRecheckInterval: 30 * time.Minute}
	store := &ConfigStore{}
	if runFullRecheckCycle(ctx, cfg, store, NewProxyPool(), coordinator) {
		t.Fatal("cancelled full recheck reported completion")
	}
	last, next := coordinator.fullRecheckTimes()
	if !last.IsZero() || !next.IsZero() {
		t.Fatalf("cancelled full recheck recorded schedule = (%s, %s)", last, next)
	}
}

func TestSourceAutoRefreshMutationWaitsForLifecycleFinalWindow(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(Source{
		Name: "scheduled", URL: "https://example.test/list", Format: FormatPlainList, Protocol: "http",
		AutoRefreshEnabled: true, RefreshIntervalSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newRefreshCoordinator()
	mutationReady := make(chan struct{})
	coordinator.sourceAutoRefreshBeforeMutation = func() { close(mutationReady) }
	server := NewStatusServerWithCoordinator(NewProxyPool(), store, coordinator)
	coordinator.sourceLifecycleMu.Lock()
	requestDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		server.handleSourceAutoRefresh(recorder, httptest.NewRequest(http.MethodPost, "/api/sources/auto-refresh", strings.NewReader(fmt.Sprintf(`{"id":%q,"enabled":false,"interval_seconds":60}`, source.ID))))
		requestDone <- recorder
	}()
	select {
	case <-mutationReady:
	case <-time.After(time.Second):
		coordinator.sourceLifecycleMu.Unlock()
		t.Fatal("auto-refresh mutation did not reach the lifecycle lock")
	}
	select {
	case <-requestDone:
		coordinator.sourceLifecycleMu.Unlock()
		t.Fatal("auto-refresh mutation escaped the source lifecycle lock")
	default:
	}
	current, ok := store.SourceByID(source.ID)
	if !ok || !current.AutoRefreshEnabled {
		coordinator.sourceLifecycleMu.Unlock()
		t.Fatalf("source changed before final window released: %+v", current)
	}
	coordinator.sourceLifecycleMu.Unlock()
	select {
	case recorder := <-requestDone:
		if recorder.Code != http.StatusOK {
			t.Fatalf("auto-refresh status = %d: %s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("auto-refresh mutation did not complete after final window")
	}
	current, _ = store.SourceByID(source.ID)
	if current.AutoRefreshEnabled {
		t.Fatalf("auto-refresh remained enabled after mutation: %+v", current)
	}
}

func TestSourceRefreshHandlerNotFoundDisabledAndCoalescing(t *testing.T) {
	store := &ConfigStore{cfg: PoolConfig{Sources: []Source{
		{ID: "source-a", Name: "A", Enabled: true},
		{ID: "source-disabled", Name: "Disabled", Enabled: false},
	}}}
	server := NewStatusServerWithCoordinator(NewProxyPool(), store, newRefreshCoordinator())

	missing := httptest.NewRecorder()
	server.handleSourceRefresh(missing, httptest.NewRequest(http.MethodPost, "/api/sources/refresh", strings.NewReader(`{"id":"missing"}`)))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", missing.Code)
	}

	disabled := httptest.NewRecorder()
	server.handleSourceRefresh(disabled, httptest.NewRequest(http.MethodPost, "/api/sources/refresh", strings.NewReader(`{"id":"source-disabled"}`)))
	if disabled.Code != http.StatusConflict {
		t.Fatalf("disabled status = %d, want 409: %s", disabled.Code, disabled.Body.String())
	}
	var disabledResponse struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(disabled.Body.Bytes(), &disabledResponse); err != nil {
		t.Fatal(err)
	}
	if disabledResponse.Code != "source_disabled" {
		t.Fatalf("disabled code = %q, want source_disabled", disabledResponse.Code)
	}
	select {
	case sourceID := <-server.coordinator.sourceRefreshChan:
		t.Fatalf("disabled source %q was queued", sourceID)
	default:
	}

	var first, second struct {
		Accepted  bool                   `json:"accepted"`
		Operation SourceRefreshOperation `json:"operation"`
	}
	for i, destination := range []*struct {
		Accepted  bool                   `json:"accepted"`
		Operation SourceRefreshOperation `json:"operation"`
	}{&first, &second} {
		recorder := httptest.NewRecorder()
		server.handleSourceRefresh(recorder, httptest.NewRequest(http.MethodPost, "/api/sources/refresh", strings.NewReader(`{"id":"source-a"}`)))
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("request %d status = %d, want 202", i, recorder.Code)
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), destination); err != nil {
			t.Fatal(err)
		}
	}
	if !first.Accepted || second.Accepted || first.Operation.ID != second.Operation.ID {
		t.Fatalf("coalescing responses: first=%+v second=%+v", first, second)
	}
}

func TestFullRefreshInFlightDisableDoesNotPublishCandidateCatalog(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseResponse
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"ip":"1.1.1.1","port":[443]}]}`))
	}))
	defer feed.Close()

	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range store.Sources() {
		if configured.Enabled {
			if err := store.ToggleSource(configured.ID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	source, err := store.AddSource(Source{
		Name: "full-in-flight", URL: feed.URL, Format: FormatProxyIPJSON, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	coordinator := newRefreshCoordinator()
	cfg := &Config{DataDir: t.TempDir(), CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10, ScrapeInterval: time.Minute}
	result := make(chan refreshRunResult, 1)
	go func() {
		result <- refreshPool(cfg, store, pool, coordinator)
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("full refresh source fetch did not start")
	}
	coordinator.sourceLifecycleMu.Lock()
	if err := store.ToggleSource(source.ID, false); err != nil {
		coordinator.sourceLifecycleMu.Unlock()
		t.Fatal(err)
	}
	if err := store.ToggleSource(source.ID, true); err != nil {
		coordinator.sourceLifecycleMu.Unlock()
		t.Fatal(err)
	}
	coordinator.sourceLifecycleMu.Unlock()
	close(releaseResponse)

	select {
	case got := <-result:
		if got.Status != "complete" {
			t.Fatalf("refresh status = %+v, want complete with stale source omitted", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("full refresh did not finish")
	}
	if candidate, ok := pool.candidates.FindByKey("proxyip://1.1.1.1:443"); ok {
		t.Fatalf("disabled source result was published to candidate catalog: %+v", candidate)
	}
}

func TestSourceRefreshInFlightDisableDoesNotPublishResult(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseResponse
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"ip":"1.1.1.1","port":[443]}]}`))
	}))
	defer feed.Close()

	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(Source{
		Name: "in-flight", URL: feed.URL, Format: FormatProxyIPJSON, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	cfg := &Config{DataDir: t.TempDir(), CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10}
	result := make(chan refreshRunResult, 1)
	go func() {
		result <- refreshSource(cfg, store, pool, newRefreshCoordinator(), source.ID, "manual", newSourceRefreshRevision(source))
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("source fetch did not start")
	}
	if err := store.ToggleSource(source.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := store.ToggleSource(source.ID, true); err != nil {
		t.Fatal(err)
	}
	close(releaseResponse)

	select {
	case got := <-result:
		if got.Status != "skipped" {
			t.Fatalf("refresh status = %+v, want skipped", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("source refresh did not finish")
	}
	if candidate, ok := pool.candidates.FindByKey("proxyip://1.1.1.1:443"); ok {
		t.Fatalf("disabled source result was published: %+v", candidate)
	}
}

func TestFullRefreshDisableInFinalWindowWithdrawsCandidateCatalog(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"ip":"1.1.1.1","port":[443]}]}`))
	}))
	defer feed.Close()
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range store.Sources() {
		if configured.Enabled {
			if err := store.ToggleSource(configured.ID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	source, err := store.AddSource(Source{
		Name: "late-window", URL: feed.URL, Format: FormatProxyIPJSON, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	coordinator := newRefreshCoordinator()
	finalWindow := make(chan struct{})
	releaseFinal := make(chan struct{})
	coordinator.fullRefreshBeforeFinalValidation = func() {
		close(finalWindow)
		<-releaseFinal
	}
	server := NewStatusServerWithCoordinator(pool, store, coordinator)
	cfg := &Config{DataDir: t.TempDir(), CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10, ScrapeInterval: time.Minute}
	result := make(chan refreshRunResult, 1)
	go func() { result <- refreshPool(cfg, store, pool, coordinator) }()

	select {
	case <-finalWindow:
	case <-time.After(2 * time.Second):
		t.Fatal("full refresh did not reach the final validation window")
	}
	checking := server.buildProxyIPPage(localTestRequest(http.MethodGet, "/api/proxyip/page?page_size=100", nil))
	if checking.ProxyIPTotal != 1 || len(checking.ProxyIPs) != 1 {
		t.Fatalf("checking ProxyIP catalog = %+v, want one published resource", checking)
	}
	recorder := httptest.NewRecorder()
	server.handleSourceToggle(recorder, httptest.NewRequest(http.MethodPost, "/api/sources/toggle", strings.NewReader(fmt.Sprintf(`{"id":%q,"enabled":false}`, source.ID))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("disable status = %d: %s", recorder.Code, recorder.Body.String())
	}
	close(releaseFinal)
	select {
	case got := <-result:
		if got.Status != "complete" {
			t.Fatalf("refresh status = %+v, want complete with disabled source withdrawn", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("full refresh did not finish")
	}
	completed := server.buildProxyIPPage(localTestRequest(http.MethodGet, "/api/proxyip/page?page_size=100", nil))
	if completed.ProxyIPTotal != 0 || len(completed.ProxyIPs) != 0 {
		t.Fatalf("disabled source remained in ProxyIP catalog: %+v", completed)
	}
}

func TestScheduledSourceRefreshAutoRefreshDisabledInFinalWindowSkipsPublication(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"ip":"1.1.1.1","port":[443]}]}`))
	}))
	defer feed.Close()
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(Source{
		Name: "scheduled-final-window", URL: feed.URL, Format: FormatProxyIPJSON, AllowPrivate: true,
		AutoRefreshEnabled: true, RefreshIntervalSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := NewProxyPool()
	coordinator := newRefreshCoordinator()
	finalWindow := make(chan struct{})
	releaseFinal := make(chan struct{})
	coordinator.sourceRefreshBeforeFinalValidation = func() {
		close(finalWindow)
		<-releaseFinal
	}
	server := NewStatusServerWithCoordinator(pool, store, coordinator)
	cfg := &Config{DataDir: t.TempDir(), CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10}
	result := make(chan refreshRunResult, 1)
	go func() {
		result <- refreshSource(cfg, store, pool, coordinator, source.ID, "scheduled", newSourceRefreshRevision(source))
	}()

	select {
	case <-finalWindow:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled refresh did not reach the final validation window")
	}
	recorder := httptest.NewRecorder()
	server.handleSourceAutoRefresh(recorder, httptest.NewRequest(http.MethodPost, "/api/sources/auto-refresh", strings.NewReader(fmt.Sprintf(`{"id":%q,"enabled":false,"interval_seconds":60}`, source.ID))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("auto-refresh status = %d: %s", recorder.Code, recorder.Body.String())
	}
	close(releaseFinal)
	select {
	case got := <-result:
		if got.Status != "skipped" {
			t.Fatalf("scheduled refresh status = %+v, want skipped", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled refresh did not finish")
	}
	completed := server.buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page?page_size=100", nil))
	if completed.CandidateTotal != 0 || len(completed.Candidates) != 0 {
		t.Fatalf("auto-refresh-disabled source was published: %+v", completed)
	}
}

func TestScheduledSourceRefreshRetainsCandidatesAndPersistsProxyIPCatalog(t *testing.T) {
	newFeed := func(ip string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"data":[{"ip":%q,"port":[443]}]}`, ip)
		}))
	}
	feedA := newFeed("1.1.1.1")
	defer feedA.Close()
	feedB := newFeed("8.8.8.8")
	defer feedB.Close()

	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range store.Sources() {
		if configured.Enabled {
			if err := store.ToggleSource(configured.ID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	sourceA, err := store.AddSource(Source{
		Name: "source-A", URL: feedA.URL, Format: FormatProxyIPJSON, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceB, err := store.AddSource(Source{
		Name: "source-B", URL: feedB.URL, Format: FormatProxyIPJSON, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	pool := NewProxyPool()
	cacheDir := t.TempDir()
	pool.candidates.SetDiskCache(newCandidateCatalogCache(cacheDir))
	cfg := &Config{DataDir: t.TempDir(), CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10}
	coordinator := newRefreshCoordinator()
	for _, source := range []Source{sourceA, sourceB} {
		result := refreshSource(cfg, store, pool, coordinator, source.ID, "scheduled", newSourceRefreshRevision(source))
		if result.Status != "complete" {
			t.Fatalf("scheduled refresh for %q = %+v, want complete", source.Name, result)
		}
	}

	want := map[string]bool{
		"proxyip://1.1.1.1:443": true,
		"proxyip://8.8.8.8:443": true,
	}
	assertCandidateCatalogKeys(t, pool.candidates, want)
	page := NewStatusServer(pool, store).buildProxyIPPage(localTestRequest(http.MethodGet, "/api/proxyip/page?page_size=100", nil))
	facets := make(map[string]int, len(page.Sources))
	for _, facet := range page.Sources {
		facets[facet.Value] = facet.Total
	}
	if facets[sourceA.Name] != 1 || facets[sourceB.Name] != 1 {
		t.Fatalf("catalog source facets = %#v, want one record from each source", facets)
	}

	restored := &CandidateCatalog{}
	restored.SetDiskCache(newCandidateCatalogCache(cacheDir))
	loaded, err := restored.LoadDiskCache()
	if err != nil || !loaded {
		t.Fatalf("LoadDiskCache() = (%v, %v), want (true, nil)", loaded, err)
	}
	assertCandidateCatalogKeys(t, restored, want)
}

func TestScheduledSourceRefreshFilteringDoesNotCorruptCandidateCache(t *testing.T) {
	firstProxy, firstTunnels := newTestConnectProxy(t)
	secondProxy, secondTunnels := newTestConnectProxy(t)
	firstURL, err := url.Parse(firstProxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	secondURL, err := url.Parse(secondProxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	knownURL, knownTunnels := firstURL, firstTunnels
	freshURL, freshTunnels := secondURL, secondTunnels
	// dedupeCandidates sorts the same-host entries lexically by port. Make the
	// cooled entry first so an in-place filter would leave a duplicate tail.
	if knownURL.Port() > freshURL.Port() {
		knownURL, freshURL = freshURL, knownURL
		knownTunnels, freshTunnels = freshTunnels, knownTunnels
	}
	asLocalhost := func(raw *url.URL) string {
		copy := *raw
		copy.Host = "localhost:" + raw.Port()
		return copy.String()
	}
	knownProxyURL, freshProxyURL := asLocalhost(knownURL), asLocalhost(freshURL)
	feedBody, err := json.Marshal([]map[string]any{
		{"proxy": knownProxyURL, "country": "US"},
		{"proxy": freshProxyURL, "country": "US"},
	})
	if err != nil {
		t.Fatal(err)
	}
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(feedBody)
	}))
	defer feed.Close()

	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range store.Sources() {
		if configured.Enabled {
			if err := store.ToggleSource(configured.ID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	source, err := store.AddSource(Source{
		Name: "scheduled-forwarding", URL: feed.URL, Format: FormatEDTJSON, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	known := Proxy{
		IP: "localhost", Port: knownURL.Port(), Protocol: "http", Country: "US", Available: true,
		SourceName: source.Name, SourceNames: []string{source.Name}, SourceIDs: []string{source.ID},
	}
	pool := NewProxyPool()
	pool.Prime([]Proxy{known}, nil)
	pool.stats[known.Key()] = &nodeStats{LastHealthSuccessAt: time.Now().UTC()}
	cacheDir := t.TempDir()
	pool.candidates.SetDiskCache(newCandidateCatalogCache(cacheDir))
	if err := store.SetCheckURL("http://health.test/check"); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: t.TempDir(), CheckTimeout: time.Second, MaxConcurrent: 1, MaxCandidates: 10, RequireIPChange: false}
	result := refreshSource(cfg, store, pool, newRefreshCoordinator(), source.ID, "scheduled", newSourceRefreshRevision(source))
	if result.Status != "complete" {
		t.Fatalf("scheduled refresh = %+v, want complete", result)
	}
	if got := knownTunnels.Load(); got != 0 {
		t.Fatalf("cooled candidate was health-checked %d time(s), want 0", got)
	}
	if got := freshTunnels.Load(); got == 0 {
		t.Fatal("eligible candidate was not health-checked")
	}

	want := map[string]bool{
		known.Key(): true,
		(Proxy{IP: "localhost", Port: freshURL.Port(), Protocol: "http"}).Key(): true,
	}
	assertCandidateCatalogKeys(t, pool.candidates, want)
	restored := &CandidateCatalog{}
	restored.SetDiskCache(newCandidateCatalogCache(cacheDir))
	loaded, err := restored.LoadDiskCache()
	if err != nil || !loaded {
		t.Fatalf("LoadDiskCache() = (%v, %v), want (true, nil)", loaded, err)
	}
	assertCandidateCatalogKeys(t, restored, want)
}

func assertCandidateCatalogKeys(t *testing.T, catalog *CandidateCatalog, want map[string]bool) {
	t.Helper()
	snapshot := catalog.snapshot.Load()
	if snapshot == nil {
		t.Fatal("candidate catalog was not published")
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if len(snapshot.records) != len(want) {
		t.Fatalf("candidate records = %d, want %d", len(snapshot.records), len(want))
	}
	got := make(map[string]bool, len(snapshot.records))
	for _, record := range snapshot.records {
		protocol := snapshot.protocols[record.protocolID]
		if protocol == "" {
			t.Fatal("candidate catalog contains an empty protocol")
		}
		got[protocol+"://"+record.addr] = true
	}
	if len(got) != len(want) {
		t.Fatalf("candidate keys = %v, want %v", got, want)
	}
	for key := range want {
		if !got[key] {
			t.Fatalf("candidate keys = %v, missing %q", got, key)
		}
	}
}
