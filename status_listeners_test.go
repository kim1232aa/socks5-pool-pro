package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeListenerManager is a test double for listenerManagerAPI. It records the
// last binding handed to Add/Update and the last id handed to Delete, and
// returns programmer-controlled outcomes. It is only used by the listener API
// tests; production code wires a *ListenerManager via SetListenerManager.
type fakeListenerManager struct {
	bindings []ListenerRuntimeView

	addErr error
	updErr error
	delErr error

	lastAdd ListenerBinding
	lastUpd ListenerBinding
	lastDel string
}

func (f *fakeListenerManager) Bindings() []ListenerRuntimeView { return f.bindings }
func (f *fakeListenerManager) Add(b ListenerBinding) (ListenerBinding, error) {
	f.lastAdd = b
	if f.addErr != nil {
		return ListenerBinding{}, f.addErr
	}
	return b, nil
}
func (f *fakeListenerManager) Update(b ListenerBinding) (ListenerBinding, error) {
	f.lastUpd = b
	if f.updErr != nil {
		return ListenerBinding{}, f.updErr
	}
	return b, nil
}
func (f *fakeListenerManager) Delete(id string) error {
	f.lastDel = id
	return f.delErr
}

// newListenerTestServer builds a StatusServer whose handler chain is used in
// tests, with an optional manager wired directly onto the field. Direct field
// assignment is possible because tests live in package main.
func newListenerTestServer(t *testing.T, manager listenerManagerAPI) http.Handler {
	t.Helper()
	s := NewStatusServer(NewProxyPool(), &ConfigStore{})
	s.listenerManager = manager
	return s.handler()
}

func doListener(t *testing.T, h http.Handler, method, target string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(raw)
	}
	req := localTestRequest(method, target, r)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestListenersNilManagerReturns503(t *testing.T) {
	h := newListenerTestServer(t, nil)

	// Every listener endpoint must report a stable 503 with the
	// listener_manager_unavailable code when no manager is wired.
	for _, tc := range []struct {
		name   string
		method string
		target string
	}{
		{"get", http.MethodGet, "/api/listeners"},
		{"post", http.MethodPost, "/api/listeners"},
		{"update", http.MethodPost, "/api/listeners/update"},
		{"delete", http.MethodPost, "/api/listeners/delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]string{"id": "x"}
			if tc.target == "/api/listeners" {
				body = map[string]string{"id": "x", "port": "10001"}
			}
			rec := doListener(t, h, tc.method, tc.target, body)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
			}
			var resp apiErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error body: %v; body=%s", err, rec.Body.String())
			}
			if resp.Code != listenerManagerUnavailableCode {
				t.Fatalf("code = %q, want %q", resp.Code, listenerManagerUnavailableCode)
			}
		})
	}
}

func TestListenersMethodRestrictions(t *testing.T) {
	mgr := &fakeListenerManager{}
	h := newListenerTestServer(t, mgr)

	// /api/listeners accepts GET/HEAD/POST; PUT/DELETE must 405.
	rec := doListener(t, h, http.MethodPut, "/api/listeners", map[string]string{"id": "x"})
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/listeners status = %d, want 405; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got == "" {
		t.Fatalf("Allow header missing on 405")
	}

	// /api/listeners/update is POST-only; GET must 405.
	rec = doListener(t, h, http.MethodGet, "/api/listeners/update", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/listeners/update status = %d, want 405", rec.Code)
	}

	// /api/listeners/delete is POST-only; GET must 405.
	rec = doListener(t, h, http.MethodGet, "/api/listeners/delete", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/listeners/delete status = %d, want 405", rec.Code)
	}
}

func TestListenersGetReturnsListResponse(t *testing.T) {
	mgr := &fakeListenerManager{
		bindings: []ListenerRuntimeView{
			{
				ListenerBinding: ListenerBinding{
					ID: "a", Name: "Japan", Port: 10001, Mode: ListenerModeGroup, Group: "JP", Enabled: true,
				},
				ListenAddr: "0.0.0.0:10001",
				Listening:  true,
			},
			{
				ListenerBinding: ListenerBinding{
					ID: "b", Name: "Fixed", Port: 10002, Mode: ListenerModeFixed, NodeKey: "node-1", Enabled: true,
				},
				ListenAddr: "0.0.0.0:10002",
				Listening:  false,
				Error:      "bind: address already in use",
			},
		},
	}
	h := newListenerTestServer(t, mgr)

	rec := doListener(t, h, http.MethodGet, "/api/listeners", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp listenerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v; body=%s", err, rec.Body.String())
	}
	if len(resp.Listeners) != 2 {
		t.Fatalf("listeners len = %d, want 2", len(resp.Listeners))
	}
	if resp.Listeners[0].ID != "a" || resp.Listeners[0].ListenAddr != "0.0.0.0:10001" || !resp.Listeners[0].Listening {
		t.Fatalf("listener[0] = %#v", resp.Listeners[0])
	}
	if resp.Listeners[1].Error == "" {
		t.Fatalf("listener[1] missing error: %#v", resp.Listeners[1])
	}
}

func TestListenersPostCreateReturnsBinding(t *testing.T) {
	mgr := &fakeListenerManager{}
	h := newListenerTestServer(t, mgr)

	in := ListenerBinding{
		ID: "new", Name: "New", Port: 10010, Mode: ListenerModeRules, Enabled: true,
	}
	rec := doListener(t, h, http.MethodPost, "/api/listeners", in)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got ListenerBinding
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode binding: %v; body=%s", err, rec.Body.String())
	}
	if got.ID != "new" || got.Mode != ListenerModeRules {
		t.Fatalf("got = %#v", got)
	}
	if mgr.lastAdd.ID != "new" {
		t.Fatalf("manager.Add not called with binding; lastAdd=%#v", mgr.lastAdd)
	}
}

func TestListenersPostCreateValidationError(t *testing.T) {
	mgr := &fakeListenerManager{addErr: errors.New("port already in use")}
	h := newListenerTestServer(t, mgr)

	rec := doListener(t, h, http.MethodPost, "/api/listeners", ListenerBinding{ID: "dup", Port: 10001, Mode: ListenerModeGroup, Group: "JP", Enabled: true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var resp apiErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error: %v; body=%s", err, rec.Body.String())
	}
	if resp.Code != "bad_request" {
		t.Fatalf("code = %q, want bad_request", resp.Code)
	}
}

func TestListenersUpdateReturnsBinding(t *testing.T) {
	mgr := &fakeListenerManager{}
	h := newListenerTestServer(t, mgr)

	in := ListenerBinding{ID: "a", Name: "Renamed", Port: 10001, Mode: ListenerModeGroup, Group: "JP", Enabled: false}
	rec := doListener(t, h, http.MethodPost, "/api/listeners/update", in)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got ListenerBinding
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode binding: %v; body=%s", err, rec.Body.String())
	}
	if got.ID != "a" || got.Enabled {
		t.Fatalf("got = %#v", got)
	}
	if mgr.lastUpd.ID != "a" {
		t.Fatalf("manager.Update not called; lastUpd=%#v", mgr.lastUpd)
	}
}

func TestListenersUpdateError(t *testing.T) {
	mgr := &fakeListenerManager{updErr: errors.New("unknown id")}
	h := newListenerTestServer(t, mgr)

	rec := doListener(t, h, http.MethodPost, "/api/listeners/update", ListenerBinding{ID: "missing"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestListenersDeleteReturnsOK(t *testing.T) {
	mgr := &fakeListenerManager{}
	h := newListenerTestServer(t, mgr)

	rec := doListener(t, h, http.MethodPost, "/api/listeners/delete", map[string]string{"id": "a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode ok: %v; body=%s", err, rec.Body.String())
	}
	if got["status"] != "ok" {
		t.Fatalf("status = %q, want ok", got["status"])
	}
	if mgr.lastDel != "a" {
		t.Fatalf("manager.Delete not called with a; lastDel=%q", mgr.lastDel)
	}
}

func TestListenersDeleteError(t *testing.T) {
	mgr := &fakeListenerManager{delErr: errors.New("unknown id")}
	h := newListenerTestServer(t, mgr)

	rec := doListener(t, h, http.MethodPost, "/api/listeners/delete", map[string]string{"id": "missing"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestListenersPostRequiresJSONBody(t *testing.T) {
	mgr := &fakeListenerManager{}
	h := newListenerTestServer(t, mgr)

	// Malformed JSON body must yield 400 bad_request, not 200.
	req := localTestRequest(http.MethodPost, "/api/listeners", bytes.NewReader([]byte("{not json")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if mgr.lastAdd.ID != "" {
		t.Fatalf("manager.Add should not be called on bad body; lastAdd=%#v", mgr.lastAdd)
	}
}

func TestSetListenerManagerAcceptsNil(t *testing.T) {
	s := NewStatusServer(NewProxyPool(), &ConfigStore{})
	// SetListenerManager must not panic on nil and must detach the manager.
	s.SetListenerManager(nil)
	if s.currentListenerManager() != nil {
		t.Fatalf("manager should be nil after SetListenerManager(nil)")
	}
}
