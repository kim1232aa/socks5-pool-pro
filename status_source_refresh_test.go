package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestSourceRefreshCoalescesSameSource(t *testing.T) {
	coordinator := newRefreshCoordinator()
	source := Source{ID: "source-a", Name: "A"}
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

func TestSourceRefreshHandlerNotFoundAndCoalescing(t *testing.T) {
	store := &ConfigStore{cfg: PoolConfig{Sources: []Source{{ID: "source-a", Name: "A", Enabled: true}}}}
	server := NewStatusServerWithCoordinator(NewProxyPool(), store, newRefreshCoordinator())

	missing := httptest.NewRecorder()
	server.handleSourceRefresh(missing, httptest.NewRequest(http.MethodPost, "/api/sources/refresh", strings.NewReader(`{"id":"missing"}`)))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", missing.Code)
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
