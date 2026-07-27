package main

import (
	"fmt"
	"net/http"
	"strings"
)

const maxInventoryDeleteKeys = 100

type inventoryDeleteRequest struct {
	Keys []string `json:"keys"`
}

type inventoryDeleteResponse struct {
	Removed  []string `json:"removed"`
	NotFound []string `json:"not_found"`
}

func normalizedInventoryDeleteKeys(keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("keys must contain at least one key")
	}
	if len(keys) > maxInventoryDeleteKeys {
		return nil, fmt.Errorf("keys exceeds maximum batch size %d", maxInventoryDeleteKeys)
	}
	out := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, raw := range keys {
		key := strings.TrimSpace(raw)
		if key == "" {
			return nil, fmt.Errorf("keys must not contain an empty key")
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out, nil
}

func (s *StatusServer) handleNodesDelete(w http.ResponseWriter, r *http.Request) {
	var request inventoryDeleteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_delete_request", err)
		return
	}
	keys, err := normalizedInventoryDeleteKeys(request.Keys)
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_delete_keys", err)
		return
	}
	if s.pool == nil {
		writeErrCode(w, http.StatusServiceUnavailable, "pool_unavailable", fmt.Errorf("proxy pool is unavailable"))
		return
	}
	removed, notFound, persistErr := s.pool.RemoveKeys(keys)
	if persistErr != nil {
		writeErrCode(w, http.StatusInternalServerError, "node_delete_not_durable", fmt.Errorf("删除未提交：节点缓存持久化失败: %w", persistErr))
		return
	}
	// Sync: also remove deleted nodes from the candidate catalog so the
	// inventory view does not leave stale entries behind an explicit pool
	// removal. Candidate removal is best-effort here — the forwarding pool
	// deletion above is the source of truth for durability.
	if s.pool.candidates != nil && len(removed) > 0 {
		s.pool.candidates.RemoveKeys(removed)
	}
	writeJSON(w, inventoryDeleteResponse{Removed: nonNilInventoryKeys(removed), NotFound: nonNilInventoryKeys(notFound)})
}

func (s *StatusServer) handleCandidatesDelete(w http.ResponseWriter, r *http.Request) {
	var request inventoryDeleteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_delete_request", err)
		return
	}
	keys, err := normalizedInventoryDeleteKeys(request.Keys)
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_delete_keys", err)
		return
	}
	if s.pool == nil || s.pool.candidates == nil {
		writeErrCode(w, http.StatusServiceUnavailable, "candidate_catalog_unavailable", fmt.Errorf("candidate catalog is unavailable"))
		return
	}
	removed, notFound, persistErr := s.pool.candidates.RemoveKeys(keys)
	if persistErr != nil {
		writeErrCode(w, http.StatusInternalServerError, "candidate_delete_not_durable", fmt.Errorf("删除未提交：候选缓存持久化失败: %w", persistErr))
		return
	}
	writeJSON(w, inventoryDeleteResponse{Removed: nonNilInventoryKeys(removed), NotFound: nonNilInventoryKeys(notFound)})
}

func nonNilInventoryKeys(keys []string) []string {
	if keys == nil {
		return []string{}
	}
	return keys
}
