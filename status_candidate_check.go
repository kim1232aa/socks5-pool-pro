package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type candidateBatchCheckRequest struct {
	Limit *int `json:"limit"`
}

type failedCandidateRetryRequest struct {
	Keys []string `json:"keys"`
	// All asks the server to enumerate the whole failed catalog itself and
	// walk it in one long operation. It is mutually exclusive with Keys.
	All bool `json:"all"`
}

type candidateCheckStartResponse struct {
	CandidateCheckOperation
	Accepted  bool   `json:"accepted"`
	StatusURL string `json:"status_url"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func (s *StatusServer) handleCandidateBatchCheck(w http.ResponseWriter, r *http.Request) {
	var request candidateBatchCheckRequest
	if err := decodeJSON(r, &request); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_candidate_check_request", err)
		return
	}
	_, _, maxCandidates, _ := s.effectiveCheckOptions()
	limit := maxCandidates
	if request.Limit != nil {
		limit = *request.Limit
	}
	if limit < 1 || limit > maxCandidates {
		writeErrCode(w, http.StatusBadRequest, "invalid_candidate_check_request", fmt.Errorf("limit must be between 1 and %d", maxCandidates))
		return
	}
	statusURL := "/api/candidates/batch-check/status"
	s.startCandidateCheck(w, candidateCheckOperationCandidateBatch, limit, nil, statusURL, false)
}

func (s *StatusServer) handleFailedCandidatesRetry(w http.ResponseWriter, r *http.Request) {
	var request failedCandidateRetryRequest
	if err := decodeJSON(r, &request); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_candidate_check_request", err)
		return
	}
	statusURL := "/api/failed-candidates/retry/status"
	if request.All {
		if len(request.Keys) > 0 {
			writeErrCode(w, http.StatusBadRequest, "invalid_candidate_check_request", fmt.Errorf("all must not be combined with explicit keys"))
			return
		}
		if s.pool == nil || s.pool.candidates == nil {
			writeErrCode(w, http.StatusBadRequest, "failed_candidate_not_found", fmt.Errorf("failed candidate catalog is unavailable"))
			return
		}
		keys := s.pool.candidates.FailedKeys()
		if len(keys) == 0 {
			writeErrCode(w, http.StatusBadRequest, "failed_candidate_not_found", fmt.Errorf("no failed candidates to retry"))
			return
		}
		s.startCandidateCheck(w, candidateCheckOperationFailedRetry, 0, keys, statusURL, true)
		return
	}
	keys, err := uniqueFailedCandidateRetryKeys(request.Keys)
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_candidate_check_request", err)
		return
	}
	_, _, maxCandidates, _ := s.effectiveCheckOptions()
	if len(keys) > maxCandidates {
		writeErrCode(w, http.StatusBadRequest, "invalid_candidate_check_request", fmt.Errorf("at most %d failed candidates may be retried", maxCandidates))
		return
	}
	if s.pool == nil || s.pool.candidates == nil {
		writeErrCode(w, http.StatusBadRequest, "failed_candidate_not_found", fmt.Errorf("failed candidate catalog is unavailable"))
		return
	}
	if missing := s.pool.candidates.ValidateFailedKeys(keys); len(missing) > 0 {
		writeErrCode(w, http.StatusBadRequest, "failed_candidate_not_found", fmt.Errorf("failed candidate keys not found: %v", missing))
		return
	}
	s.startCandidateCheck(w, candidateCheckOperationFailedRetry, 0, keys, statusURL, false)
}

func (s *StatusServer) startCandidateCheck(w http.ResponseWriter, kind string, limit int, keys []string, statusURL string, retryAll bool) {
	var operation CandidateCheckOperation
	var err error
	if retryAll {
		operation, err = s.coordinator.requestFailedRetryAll(keys)
	} else {
		operation, err = s.coordinator.requestCandidateCheck(kind, limit, keys)
	}
	response := candidateCheckStartResponse{
		CandidateCheckOperation: operation,
		Accepted:                err == nil,
		StatusURL:               statusURL,
	}
	if err == nil {
		w.Header().Set("Location", statusURL)
		writeJSONStatus(w, http.StatusAccepted, response)
		return
	}
	if current, busy := candidateCheckBusyOperation(err); busy {
		response.CandidateCheckOperation = current
		response.Code = "candidate_check_busy"
		response.Error = err.Error()
		response.RequestID = requestIDFromContext(w)
		writeJSONStatus(w, http.StatusConflict, response)
		return
	}
	var shutdown *candidateCheckShutdownError
	if errors.As(err, &shutdown) {
		response.CandidateCheckOperation = shutdown.Operation
		response.Code = "candidate_check_unavailable"
		response.Error = err.Error()
		response.RequestID = requestIDFromContext(w)
		writeJSONStatus(w, http.StatusServiceUnavailable, response)
		return
	}
	writeErrCode(w, http.StatusServiceUnavailable, "candidate_check_unavailable", err)
}

func (s *StatusServer) handleCandidateCheckStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.coordinator.candidateCheckOperationStatus())
}

func uniqueFailedCandidateRetryKeys(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("keys must contain at least one failed candidate")
	}
	keys := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, value := range raw {
		key := strings.TrimSpace(value)
		if key == "" {
			return nil, fmt.Errorf("failed candidate key must not be empty")
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate failed candidate key: %s", key)
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys, nil
}
