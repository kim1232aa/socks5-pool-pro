package main

import (
	"errors"
	"net/http"
)

// listenerManagerAPI is the narrow interface the status API layer consumes.
// Production code wires a *ListenerManager via SetListenerManager; tests may
// substitute a fake. It mirrors the public Manager method contract so the API
// layer stays decoupled from the concrete runtime type.
type listenerManagerAPI interface {
	Bindings() []ListenerRuntimeView
	Add(ListenerBinding) (ListenerBinding, error)
	Update(ListenerBinding) (ListenerBinding, error)
	Delete(id string) error
}

// listenerManagerUnavailableCode is the stable, machine-readable error code
// returned when no ListenerManager has been wired into the StatusServer. Kept
// as a constant so the API contract is independent of the concrete type.
const listenerManagerUnavailableCode = "listener_manager_unavailable"

// SetListenerManager wires the persistent multi-listener manager. It is safe
// to call before Start. Passing nil detaches the manager; every listener API
// call then returns 503 listener_manager_unavailable.
func (s *StatusServer) SetListenerManager(m *ListenerManager) {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	if m == nil {
		s.listenerManager = nil
		return
	}
	s.listenerManager = m
}

func (s *StatusServer) currentListenerManager() listenerManagerAPI {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	return s.listenerManager
}

// requireListenerManager resolves the wired manager or writes a 503 with the
// stable listener_manager_unavailable code and returns false. Centralizing the
// guard keeps every listener endpoint consistent under the nil case.
func (s *StatusServer) requireListenerManager(w http.ResponseWriter) (listenerManagerAPI, bool) {
	m := s.currentListenerManager()
	if m == nil {
		writeErrCode(w, http.StatusServiceUnavailable, listenerManagerUnavailableCode,
			errListenerManagerUnavailable)
		return nil, false
	}
	return m, true
}

var errListenerManagerUnavailable = errors.New("listener manager is not available")

// ---- handlers: listeners ----

// handleListeners serves the GET list and POST create entry point. GET returns
// {"listeners":[...runtime views...]}; POST accepts a ListenerBinding body and
// returns the created runtime view. Method dispatch mirrors handleGroups.
func (s *StatusServer) handleListeners(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		m, ok := s.requireListenerManager(w)
		if !ok {
			return
		}
		writeJSON(w, listenerListResponse{Listeners: m.Bindings()})
	case http.MethodPost:
		m, ok := s.requireListenerManager(w)
		if !ok {
			return
		}
		var in ListenerBinding
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := m.Add(in)
		if err != nil {
			writeConfigStoreError(w, err)
			return
		}
		writeJSON(w, created)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
	}
}

// handleListenerUpdate applies a full-binding update. It accepts a ListenerBinding
// body and returns the updated runtime view. requirePost already enforces POST.
func (s *StatusServer) handleListenerUpdate(w http.ResponseWriter, r *http.Request) {
	m, ok := s.requireListenerManager(w)
	if !ok {
		return
	}
	var in ListenerBinding
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	updated, err := m.Update(in)
	if err != nil {
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, updated)
}

// handleListenerDelete removes a binding. The body is {"id":"..."}; success
// returns {"status":"ok"} mirroring the group/source delete endpoints.
func (s *StatusServer) handleListenerDelete(w http.ResponseWriter, r *http.Request) {
	m, ok := s.requireListenerManager(w)
	if !ok {
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := m.Delete(in.ID); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// listenerListResponse wraps the GET /api/listeners body so clients receive a
// named object rather than a bare array (consistent with other list endpoints).
type listenerListResponse struct {
	Listeners []ListenerRuntimeView `json:"listeners"`
}
