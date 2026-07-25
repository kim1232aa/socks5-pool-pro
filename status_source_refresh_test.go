package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	checking := server.buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page?page_size=100", nil))
	if checking.CandidateTotal != 1 || len(checking.Candidates) != 1 {
		t.Fatalf("checking catalog = %+v, want one published candidate", checking)
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
	completed := server.buildCandidatePage(localTestRequest(http.MethodGet, "/api/candidates/page?page_size=100", nil))
	if completed.CandidateTotal != 0 || len(completed.Candidates) != 0 {
		t.Fatalf("disabled source remained in candidate catalog: %+v", completed)
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
