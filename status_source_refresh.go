package main

import (
	"fmt"
	"net/http"
)

func (s *StatusServer) handleSourceRefresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	source, ok := s.store.SourceByID(in.ID)
	if !ok {
		writeErrCode(w, http.StatusNotFound, "source_not_found", fmt.Errorf("source not found: %s", in.ID))
		return
	}
	operation, accepted := s.coordinator.requestSourceRefresh(source, "manual")
	writeJSONStatus(w, http.StatusAccepted, map[string]interface{}{"operation": operation, "accepted": accepted})
}

func (s *StatusServer) handleSourceAutoRefresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID              string `json:"id"`
		Enabled         bool   `json:"enabled"`
		IntervalSeconds int    `json:"interval_seconds"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.SetSourceAutoRefresh(in.ID, in.Enabled, in.IntervalSeconds); err != nil {
		if _, ok := s.store.SourceByID(in.ID); !ok {
			writeErrCode(w, http.StatusNotFound, "source_not_found", err)
			return
		}
		writeConfigStoreError(w, err)
		return
	}
	source, _ := s.store.SourceByID(in.ID)
	writeJSON(w, map[string]interface{}{"source": safeManagementSource(source)})
}
