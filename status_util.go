package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// apiBootNonce prevents generation counters that restart at zero from making a
// snapshot token from a previous process look valid after a restart.
func newAPIBootNonce() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
}

func formatPoolSnapshotIDWithBoot(boot string, generation uint64) string {
	return fmt.Sprintf("pool:%s:%d", boot, generation)
}

func formatPoolSnapshotID(generation uint64) string {
	return formatPoolSnapshotIDWithBoot(apiBootNonce, generation)
}

func formatCandidateSnapshotIDWithBoot(boot string, candidateGeneration, candidateRevision, overlayHash uint64) string {
	return fmt.Sprintf("candidate:%s:%d:%d:%016x", boot, candidateGeneration, candidateRevision, overlayHash)
}

func formatCandidateSnapshotID(candidateGeneration, candidateRevision, overlayHash uint64) string {
	return formatCandidateSnapshotIDWithBoot(apiBootNonce, candidateGeneration, candidateRevision, overlayHash)
}

func formatV1ProxySnapshotIDWithBoot(boot string, proxies []V1ProxyView) string {
	encoded, _ := json.Marshal(proxies)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("proxies:%s:%s", boot, hex.EncodeToString(digest[:12]))
}

func formatV1ProxyPickSnapshotIDWithBoot(boot string, proxies []V1ProxyView) string {
	encoded, _ := json.Marshal(proxies)
	hash := sha256.New()
	_, _ = hash.Write(encoded)
	// Score is intentionally absent from page rows so reliability observations
	// do not invalidate key-sorted pagination. /pick does use it, so that
	// endpoint gets a distinct score-aware token and can never return a
	// different best node under the same snapshot identity.
	for _, proxy := range proxies {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(strconv.AppendFloat(nil, proxy.score, 'g', -1, 64))
	}
	digest := hash.Sum(nil)
	return fmt.Sprintf("proxy-pick:%s:%s", boot, hex.EncodeToString(digest[:12]))
}

func formatV1ProxySnapshotID(proxies []V1ProxyView) string {
	return formatV1ProxySnapshotIDWithBoot(apiBootNonce, proxies)
}

func formatV1ProxyPickSnapshotID(proxies []V1ProxyView) string {
	return formatV1ProxyPickSnapshotIDWithBoot(apiBootNonce, proxies)
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, v interface{}) {
	writeJSONStatus(w, http.StatusOK, v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeErrCode(w, status, apiCodeForStatus(status), err)
}

func writeConfigStoreError(w http.ResponseWriter, err error) {
	var persistenceErr *ConfigPersistenceError
	if errors.As(err, &persistenceErr) {
		writeErrCode(w, http.StatusInternalServerError, "config_persistence_failed", err)
		return
	}
	writeErr(w, http.StatusBadRequest, err)
}

// apiErrorResponse retains the historical top-level error string used by the
// dashboard while adding stable, machine-readable metadata for API clients.
type apiErrorResponse struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	RequestID string `json:"request_id,omitempty"`
}

func writeErrCode(w http.ResponseWriter, status int, code string, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiErrorResponse{
		Error: err.Error(), Code: code, RequestID: requestIDFromContext(w),
	})
}

func apiCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestTimeout:
		return "request_timeout"
	case http.StatusTooManyRequests:
		return "too_many_requests"
	case http.StatusBadGateway:
		return "bad_gateway"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	case http.StatusGatewayTimeout:
		return "gateway_timeout"
	default:
		return "http_error"
	}
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeErrCode(w, http.StatusMethodNotAllowed, "method_not_allowed", fmt.Errorf("method not allowed"))
}

func (s *StatusServer) handleAPINotFound(w http.ResponseWriter, _ *http.Request) {
	writeErrCode(w, http.StatusNotFound, "route_not_found", fmt.Errorf("API route not found"))
}

const maxJSONBodyBytes = 1 << 20 // management payloads never need more than 1 MiB

func decodeJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBodyBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxJSONBodyBytes {
		return fmt.Errorf("request body exceeds %d bytes", maxJSONBodyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer io.Writer
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	return w.writer.Write(p)
}

// gzipIfAccepted keeps the default API wire format unchanged for clients that
// do not request compression, while preventing the dashboard's node list from
// repeatedly transferring hundreds of kilobytes of JSON over the network.
func gzipIfAccepted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, writer: gz}, r)
	})
}

// acceptsGzip honors q=0 and exact content-coding tokens. A substring check
// incorrectly compressed requests such as "xgzip" and "gzip;q=0".
func acceptsGzip(header string) bool {
	explicit, explicitAllowed := false, false
	wildcardAllowed := false
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		coding := strings.ToLower(strings.TrimSpace(parts[0]))
		if coding == "" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			name, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || parsed < 0 || parsed > 1 {
				quality = 0
			} else {
				quality = parsed
			}
		}
		switch coding {
		case "gzip":
			explicit = true
			if quality > 0 {
				explicitAllowed = true
			}
		case "*":
			if quality > 0 {
				wildcardAllowed = true
			}
		}
	}
	if explicit {
		return explicitAllowed
	}
	return wildcardAllowed
}

type apiRequestIDContextKey struct{}

func requestIDFromContext(w http.ResponseWriter) string {
	return w.Header().Get("X-Request-ID")
}

func newAPIRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
}

func withAPIResponseMetadata(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newAPIRequestID()
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("Cache-Control", "no-store, private")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		r = r.WithContext(context.WithValue(r.Context(), apiRequestIDContextKey{}, requestID))
		next.ServeHTTP(w, r)
	})
}

func validatedV1ProxyProtocol(raw string) (string, error) {
	protocol := strings.ToLower(strings.TrimSpace(raw))
	switch protocol {
	case "", "socks5", "http", "https":
		return protocol, nil
	default:
		return "", fmt.Errorf("protocol must be socks5, http, or https")
	}
}

func strictV1PageParams(r *http.Request) (page, pageSize int, err error) {
	page, pageSize = 1, defaultNodePageSize
	if raw := strings.TrimSpace(r.URL.Query().Get("page")); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			return 0, 0, fmt.Errorf("page must be a positive integer")
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("page_size")); raw != "" {
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > maxNodePageSize {
			return 0, 0, fmt.Errorf("page_size must be between 1 and %d", maxNodePageSize)
		}
	}
	return page, pageSize, nil
}

func filterV1ProxyViews(all []V1ProxyView, protocol string, r *http.Request) []V1ProxyView {
	query := r.URL.Query()
	countryRaw := strings.TrimSpace(query.Get("country"))
	unknownCountry := strings.EqualFold(countryRaw, "__unknown__") || nodeQueryEnabled(query.Get("country_unknown"))
	country := normalizedNodeCountry(countryRaw)
	filtered := make([]V1ProxyView, 0, len(all))
	for _, view := range all {
		if protocol != "" && view.Protocol != protocol {
			continue
		}
		if unknownCountry && view.Country != "" {
			continue
		}
		if !unknownCountry && country != "" && view.Country != country {
			continue
		}
		filtered = append(filtered, view)
	}
	return filtered
}

// compactStatusSummary deliberately omits the IP-pool URL list. The default
// /api/status response retains the registration-client contract; dashboard
// polling only needs counters and group state.
type compactStatusSummary struct {
	Total                 int         `json:"total"`
	ProxyIPTotal          int         `json:"proxyip_total"`
	LastScrape            string      `json:"last_scrape"`
	NextScrape            string      `json:"next_scrape"`
	LastScrapeAt          string      `json:"last_scrape_at,omitempty"`
	NextScrapeAt          string      `json:"next_scrape_at,omitempty"`
	Groups                []GroupView `json:"groups"`
	ActiveProxy           string      `json:"active_proxy"`
	AvailableTotal        int         `json:"available_total"`
	UnavailableTotal      int         `json:"unavailable_total"`
	HealthRecheckPending  bool        `json:"health_recheck_pending"`
	Scrape                ScrapeInfo  `json:"scrape"`
	CandidateTotal        int         `json:"candidate_total"`
	CandidatePhase        string      `json:"candidate_phase"`
	CandidateSourceErrors int         `json:"candidate_source_errors"`
	CandidateUpdatedAt    string      `json:"candidate_updated_at,omitempty"`
}

type compactCandidateSummary struct {
	Total        int
	Phase        string
	SourceErrors int
	UpdatedAt    string
}

func (s *StatusServer) compactCandidateStatus() compactCandidateSummary {
	if s.pool == nil || s.pool.candidates == nil {
		return compactCandidateSummary{Phase: "loading"}
	}
	snapshot := s.pool.candidates.snapshot.Load()
	if snapshot == nil {
		return compactCandidateSummary{Phase: "loading"}
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	return compactCandidateSummary{
		Total: len(snapshot.records), Phase: snapshot.phase,
		SourceErrors: snapshot.sourceErrors, UpdatedAt: formatCandidateTime(snapshot.seenAt),
	}
}

func validateCountryQuery(r *http.Request) error {
	query := r.URL.Query()
	raw := strings.TrimSpace(query.Get("country"))
	unknown := nodeQueryEnabled(query.Get("country_unknown"))
	if raw == "" {
		return nil
	}
	if strings.EqualFold(raw, "__unknown__") {
		return nil
	}
	if unknown {
		return fmt.Errorf("country cannot be combined with country_unknown")
	}
	if normalizedNodeCountry(raw) == "" {
		return fmt.Errorf("country must be a two-letter ASCII ISO code or __unknown__")
	}
	return nil
}

func nodePageParams(r *http.Request) (page, pageSize int) {
	page, pageSize = 1, defaultNodePageSize
	if raw := r.URL.Query().Get("page"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			page = parsed
		}
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			pageSize = parsed
		}
	}
	if pageSize > maxNodePageSize {
		pageSize = maxNodePageSize
	}
	return page, pageSize
}

func nodeQueryEnabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func normalizedNodeCountry(country string) string {
	code := strings.ToUpper(strings.TrimSpace(country))
	if len(code) != 2 {
		return ""
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return ""
		}
	}
	return code
}

func sortNodeViews(nodes []NodeView, sortBy string) {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		switch sortBy {
		case "latency":
			latency := func(node NodeView) int64 {
				if node.LatencyMs > 0 {
					return node.LatencyMs
				}
				return 1<<62 - 1
			}
			if la, lb := latency(a), latency(b); la != lb {
				return la < lb
			}
		case "speed":
			if a.SpeedKbps != b.SpeedKbps {
				return a.SpeedKbps > b.SpeedKbps
			}
		case "country":
			if a.Country != b.Country {
				return a.Country < b.Country
			}
		default: // score is the UI default, including unknown sort values.
			if a.Score != b.Score {
				return a.Score > b.Score
			}
		}
		return a.Key < b.Key
	})
}
