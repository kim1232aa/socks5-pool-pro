package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func inventoryTestProxy(protocol, ip, port string, available bool) Proxy {
	return Proxy{Protocol: protocol, IP: ip, Port: port, Available: available}
}
func inventoryLocalRequest(method, target string, body *bytes.Buffer) *http.Request {
	if body == nil {
		body = &bytes.Buffer{}
	}
	request := httptest.NewRequest(method, target, body)
	request.Host = "localhost"
	request.RemoteAddr = "127.0.0.1:12345"
	return request
}

func TestProxyPoolRemoveKeysPartialHitRemovesRoutingState(t *testing.T) {
	remove := inventoryTestProxy("socks5", "8.8.8.80", "1080", true)
	keep := inventoryTestProxy("http", "8.8.4.4", "8080", true)
	pool := NewProxyPool()
	pool.Prime([]Proxy{remove, keep}, nil)
	pool.RecordResult(remove.Key(), true, 10)
	pool.groupState["sticky"] = &groupCursor{stickyKey: remove.Key(), lastPicked: remove.Key(), pinned: true}

	removed, notFound, err := pool.RemoveKeys([]string{remove.Key(), "http://1.1.1.1:9999"})
	if err != nil || len(removed) != 1 || removed[0] != remove.Key() || len(notFound) != 1 || notFound[0] != "http://1.1.1.1:9999" {
		t.Fatalf("RemoveKeys result removed=%v notFound=%v err=%v", removed, notFound, err)
	}
	if pool.Size() != 1 || pool.All()[0].Key() != keep.Key() {
		t.Fatalf("pool after removal = %#v", pool.All())
	}
	if _, ok := pool.stats[remove.Key()]; ok {
		t.Fatal("removed node statistics survived")
	}
	if _, ok := pool.groupState["sticky"]; ok {
		t.Fatal("removed node survived in group cursor")
	}
	groups := []Group{{Name: "removed-only", Strategy: StrategyRoundRobin, Nodes: []string{remove.Key()}}}
	if _, ok, direct := pool.Pick("removed-only", groups); ok || direct {
		t.Fatalf("removed node remained routable: ok=%v direct=%v", ok, direct)
	}
}
func TestProxyPoolRemoveKeysPersistsAcrossCacheLoad(t *testing.T) {
	dir := t.TempDir()
	cache := newPoolCache(dir)
	remove := inventoryTestProxy("socks5", "8.8.8.84", "1080", true)
	keep := inventoryTestProxy("http", "8.8.4.6", "8080", true)
	pool := NewProxyPool()
	pool.Prime([]Proxy{remove, keep}, nil)
	pool.SetCache(cache)
	pool.RecordResult(remove.Key(), true, 12)

	removed, notFound, err := pool.RemoveKeys([]string{remove.Key()})
	if err != nil || len(removed) != 1 || len(notFound) != 0 {
		t.Fatalf("RemoveKeys result removed=%v notFound=%v err=%v", removed, notFound, err)
	}
	forwarding, _, stats := cache.load()
	if len(forwarding) != 1 || forwarding[0].Key() != keep.Key() {
		t.Fatalf("persisted forwarding nodes = %#v", forwarding)
	}
	if _, exists := stats[remove.Key()]; exists {
		t.Fatal("removed node statistics survived persisted cache")
	}
}

func TestCandidateCatalogRemoveKeysPersistsAcrossCacheLoad(t *testing.T) {
	dir := t.TempDir()
	catalog := &CandidateCatalog{}
	catalog.SetDiskCache(newCandidateCatalogCache(dir))
	remove := inventoryTestProxy("socks5", "8.8.8.81", "1080", false)
	keep := inventoryTestProxy("http", "8.8.4.5", "8080", false)
	refresh := catalog.begin([]Proxy{remove, keep}, nil, nil, 0)
	catalog.complete(refresh, nil, nil, nil)

	removed, notFound, persistErr := catalog.RemoveKeys([]string{remove.Key(), "https://1.1.1.1:443"})
	if len(removed) != 1 || removed[0] != remove.Key() || len(notFound) != 1 || notFound[0] != "https://1.1.1.1:443" {
		t.Fatalf("RemoveKeys result removed=%v notFound=%v", removed, notFound)
	}
	if persistErr != nil {
		t.Fatalf("RemoveKeys persistence error: %v", persistErr)
	}
	if _, ok := catalog.FindByKey(remove.Key()); ok {
		t.Fatal("removed candidate still found in live catalog")
	}
	if found, ok := catalog.FindByKey(keep.Key()); !ok || found.Key() != keep.Key() {
		t.Fatalf("kept candidate lookup = %#v, %v", found, ok)
	}

	restored := &CandidateCatalog{}
	restored.SetDiskCache(newCandidateCatalogCache(dir))
	loaded, err := restored.LoadDiskCache()
	if err != nil || !loaded {
		t.Fatalf("LoadDiskCache = %v, %v", loaded, err)
	}
	if _, ok := restored.FindByKey(remove.Key()); ok {
		t.Fatal("removed candidate was resurrected from disk cache")
	}
	if _, ok := restored.FindByKey(keep.Key()); !ok {
		t.Fatal("kept candidate was lost from disk cache")
	}
}

func TestCandidateCatalogRemoveConcurrentWithCompleteDoesNotRevive(t *testing.T) {
	for attempt := range 100 {
		catalog := &CandidateCatalog{}
		remove := inventoryTestProxy("socks5", "8.8.8.85", "1080", false)
		keep := inventoryTestProxy("http", "8.8.4.7", "8080", false)
		refresh := catalog.begin([]Proxy{remove, keep}, nil, nil, 0)
		start := make(chan struct{})
		done := make(chan struct{})
		go func() {
			<-start
			catalog.complete(refresh, []Proxy{remove, keep}, nil, nil)
			close(done)
		}()
		close(start)
		removed, _, err := catalog.RemoveKeys([]string{remove.Key()})
		<-done
		if err != nil || len(removed) != 1 {
			t.Fatalf("attempt %d removal = %v err=%v", attempt, removed, err)
		}
		if _, ok := catalog.FindByKey(remove.Key()); ok {
			t.Fatalf("attempt %d removed candidate revived", attempt)
		}
		snapshot := catalog.snapshot.Load()
		snapshot.mu.RLock()
		phase, revision := snapshot.phase, snapshot.revision
		snapshot.mu.RUnlock()
		if revision < 2 || phase != "checking" && phase != "complete" {
			t.Fatalf("attempt %d phase/revision regressed to %q/%d", attempt, phase, revision)
		}
	}
}

func TestCandidateDeleteHandlerReportsNonDurableCacheFailure(t *testing.T) {
	catalog := &CandidateCatalog{}
	candidate := inventoryTestProxy("http", "8.8.8.86", "8080", false)
	refresh := catalog.begin([]Proxy{candidate}, nil, nil, 0)
	catalog.complete(refresh, nil, nil, nil)
	badParent := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(badParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog.SetDiskCache(&candidateCatalogCache{path: badParent + "/catalog.gz"})
	pool := NewProxyPool()
	pool.candidates = catalog
	server := NewStatusServer(pool, &ConfigStore{})
	recorder := httptest.NewRecorder()
	body := `{"keys":["` + candidate.Key() + `"]}`
	server.handleCandidatesDelete(recorder, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body)))
	if recorder.Code != http.StatusInternalServerError || !bytes.Contains(recorder.Body.Bytes(), []byte("candidate_delete_not_durable")) {
		t.Fatalf("non-durable response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
func TestNodeDeleteHandlerReportsNonDurableCacheFailure(t *testing.T) {
	node := inventoryTestProxy("socks5", "8.8.8.89", "1080", true)
	pool := NewProxyPool()
	pool.Prime([]Proxy{node}, nil)
	badParent := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(badParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	pool.SetCache(newPoolCache(t.TempDir()))
	pool.cache.path = badParent + "/pool_cache.json"
	server := NewStatusServer(pool, &ConfigStore{})
	recorder := httptest.NewRecorder()
	body := `{"keys":["` + node.Key() + `"]}`
	server.handleNodesDelete(recorder, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body)))
	if recorder.Code != http.StatusInternalServerError || !bytes.Contains(recorder.Body.Bytes(), []byte("node_delete_not_durable")) {
		t.Fatalf("non-durable response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
func TestInventoryDeleteRoutesArePostOnlyAndRemoveFromPages(t *testing.T) {
	node := inventoryTestProxy("socks5", "8.8.8.87", "1080", true)
	candidate := inventoryTestProxy("http", "8.8.8.88", "8080", false)
	pool := NewProxyPool()
	pool.Prime([]Proxy{node}, nil)
	refresh := pool.candidates.begin([]Proxy{candidate}, nil, nil, 0)
	pool.candidates.complete(refresh, nil, nil, nil)
	handler := NewStatusServer(pool, &ConfigStore{}).handler()

	getDelete := httptest.NewRecorder()
	handler.ServeHTTP(getDelete, inventoryLocalRequest(http.MethodGet, "/api/nodes/delete", nil))
	if getDelete.Code != http.StatusMethodNotAllowed || getDelete.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET delete status=%d Allow=%q", getDelete.Code, getDelete.Header().Get("Allow"))
	}
	for route, key := range map[string]string{"/api/nodes/delete": node.Key(), "/api/candidates/delete": candidate.Key()} {
		recorder := httptest.NewRecorder()
		body := `{"keys":["` + key + `"]}`
		handler.ServeHTTP(recorder, inventoryLocalRequest(http.MethodPost, route, bytes.NewBufferString(body)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("POST %s status=%d body=%s", route, recorder.Code, recorder.Body.String())
		}
	}
	if pool.Size() != 0 {
		t.Fatalf("deleted node remains in pool page source: %#v", pool.All())
	}
	page := NewStatusServer(pool, &ConfigStore{}).buildCandidatePage(inventoryLocalRequest(http.MethodGet, "/api/candidates/page", nil))
	if page.CandidateTotal != 0 || len(page.Candidates) != 0 {
		t.Fatalf("deleted candidate remains paginated: %#v", page)
	}
}

func TestInventoryDeleteHandlersValidateAndReportPartialHits(t *testing.T) {
	node := inventoryTestProxy("socks5", "8.8.8.82", "1080", true)
	candidate := inventoryTestProxy("http", "8.8.8.83", "8080", false)
	pool := NewProxyPool()
	pool.Prime([]Proxy{node}, nil)
	refresh := pool.candidates.begin([]Proxy{candidate}, nil, nil, 0)
	pool.candidates.complete(refresh, nil, nil, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	assertDelete := func(handler http.HandlerFunc, body string, want inventoryDeleteResponse) {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("delete status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var got inventoryDeleteResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !equalInventoryKeys(got.Removed, want.Removed) || !equalInventoryKeys(got.NotFound, want.NotFound) {
			t.Fatalf("delete response=%#v want=%#v", got, want)
		}
	}
	assertDelete(server.handleNodesDelete, `{"keys":["`+node.Key()+`","`+node.Key()+`","http://1.1.1.1:1"]}`, inventoryDeleteResponse{Removed: []string{node.Key()}, NotFound: []string{"http://1.1.1.1:1"}})
	assertDelete(server.handleCandidatesDelete, `{"keys":["`+candidate.Key()+`","https://1.1.1.1:443"]}`, inventoryDeleteResponse{Removed: []string{candidate.Key()}, NotFound: []string{"https://1.1.1.1:443"}})

	for _, body := range []string{`{"keys":[]}`, `{"keys":[""]}`} {
		recorder := httptest.NewRecorder()
		server.handleNodesDelete(recorder, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body)))
		if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("invalid body %s status=%d type=%q response=%s", body, recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
		}
	}
}

func equalInventoryKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
