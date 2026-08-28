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
	"hash"
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

func formatNodeSnapshotID(generation, activeRevision uint64) string {
	return fmt.Sprintf("pool:%s:%d:%d", apiBootNonce, generation, activeRevision)
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

// v1ProxySnapshotHasher incrementally computes the v1 page snapshot digest
// using a streaming sha256 hash so per-proxy feeding performs no extra
// allocation. It replaces the historical json.Marshal of the full healthy
// view list which allocated O(pool) bytes per request. The digest is
// order-sensitive: feed proxies in pool iteration order (deterministic for a
// given pool state) to preserve the snapshot identity semantics.
type v1ProxySnapshotHasher struct {
	boot    string
	hash    hash.Hash
	scratch []byte
}

func newV1ProxySnapshotHasher(boot string) *v1ProxySnapshotHasher {
	return &v1ProxySnapshotHasher{boot: boot, hash: sha256.New(), scratch: make([]byte, 0, 256)}
}

// appendProxyURL appends the consumer URL to b.
func appendProxyURL(b []byte, px Proxy) []byte {
	scheme := px.Protocol
	if scheme == "https" {
		scheme = "http"
	}
	b = append(b, scheme...)
	b = append(b, "://"...)
	if px.Username != "" {
		b = append(b, px.Username...)
		b = append(b, ':')
		b = append(b, px.Password...)
		b = append(b, '@')
	}
	b = append(b, px.IP...)
	b = append(b, ':')
	b = append(b, px.Port...)
	return b
}

// appendKey appends the protocol-aware key (protocol://ip:port) to b.
func appendKey(b []byte, px Proxy) []byte {
	b = append(b, px.Protocol...)
	b = append(b, "://"...)
	b = append(b, px.IP...)
	b = append(b, ':')
	b = append(b, px.Port...)
	return b
}

func (h *v1ProxySnapshotHasher) feed(view V1ProxyView) {
	b := h.scratch[:0]
	b = append(b, view.ProxyURL...)
	b = append(b, 0)
	b = append(b, view.SocksURL...)
	b = append(b, 0)
	b = append(b, view.Username...)
	b = append(b, 0)
	b = append(b, view.Password...)
	b = append(b, 0)
	b = append(b, view.Key...)
	b = append(b, 0)
	b = append(b, view.Protocol...)
	b = append(b, 0)
	b = append(b, view.Country...)
	b = append(b, 0)
	b = append(b, view.City...)
	b = append(b, 0)
	b = strconv.AppendInt(b, view.Latency, 10)
	b = append(b, 0)
	b = strconv.AppendFloat(b, view.Speed, 'g', -1, 64)
	b = append(b, 0)
	h.hash.Write(b)
	h.scratch = b
}

// feedProxy digests a Proxy directly without allocating a V1ProxyView (and the
// ConsumerURL/Key strings that would otherwise be built for every healthy
// node). The digest fields mirror feed() so identity semantics are preserved.
// Score is mixed in only for /pick tokens; page tokens omit it.
func (h *v1ProxySnapshotHasher) feedProxy(px Proxy, withScore bool, score float64) {
	b := h.scratch[:0]
	b = appendProxyURL(b, px)
	b = append(b, 0)
	if px.Protocol == "socks5" {
		b = appendProxyURL(b, px)
	}
	b = append(b, 0)
	b = append(b, px.Username...)
	b = append(b, 0)
	b = append(b, px.Password...)
	b = append(b, 0)
	b = appendKey(b, px)
	b = append(b, 0)
	b = append(b, px.Protocol...)
	b = append(b, 0)
	b = append(b, normalizedNodeCountry(px.Country)...)
	b = append(b, 0)
	b = append(b, px.City...)
	b = append(b, 0)
	b = strconv.AppendInt(b, px.LatencyMs, 10)
	b = append(b, 0)
	b = strconv.AppendFloat(b, px.SpeedKbps, 'g', -1, 64)
	b = append(b, 0)
	if withScore {
		b = append(b, 0)
		b = strconv.AppendFloat(b, score, 'g', -1, 64)
		b = append(b, 0)
	}
	h.hash.Write(b)
	h.scratch = b
}

func (h *v1ProxySnapshotHasher) feedScore(score float64) {
	b := h.scratch[:0]
	b = append(b, 0)
	b = strconv.AppendFloat(b, score, 'g', -1, 64)
	b = append(b, 0)
	h.hash.Write(b)
	h.scratch = b
}

func (h *v1ProxySnapshotHasher) digest() []byte {
	return h.hash.Sum(nil)
}

func (h *v1ProxySnapshotHasher) finish() string {
	return fmt.Sprintf("proxies:%s:%s", h.boot, hex.EncodeToString(h.digest()[:12]))
}

func (h *v1ProxySnapshotHasher) finishPick() string {
	return fmt.Sprintf("proxy-pick:%s:%s", h.boot, hex.EncodeToString(h.digest()[:12]))
}

// v1ProxyDigestFields returns the canonical per-proxy digest input. It mirrors
// the JSON field set that the historical marshal-based snapshot token covered
// (ProxyURL, SocksURL, Username, Password, Key, Protocol, Country, City,
// LatencyMs, SpeedKbps), delimited by NUL bytes so field boundaries are
// unambiguous. Score is intentionally excluded from the page digest per the
// historical contract; /pick appends it separately via feedScore.
func v1ProxyDigestFields(view V1ProxyView) []byte {
	var buf []byte
	buf = append(buf, view.ProxyURL...)
	buf = append(buf, 0)
	buf = append(buf, view.SocksURL...)
	buf = append(buf, 0)
	buf = append(buf, view.Username...)
	buf = append(buf, 0)
	buf = append(buf, view.Password...)
	buf = append(buf, 0)
	buf = append(buf, view.Key...)
	buf = append(buf, 0)
	buf = append(buf, view.Protocol...)
	buf = append(buf, 0)
	buf = append(buf, view.Country...)
	buf = append(buf, 0)
	buf = append(buf, view.City...)
	buf = append(buf, 0)
	buf = strconv.AppendInt(buf, view.Latency, 10)
	buf = append(buf, 0)
	buf = strconv.AppendFloat(buf, view.Speed, 'g', -1, 64)
	buf = append(buf, 0)
	return buf
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

// compactStatusSummary deliberately omits the IP-pool URL list. The default
// /api/status response retains the registration-client contract; dashboard
// polling only needs counters and group state.
type compactStatusSummary struct {
	Total                    int         `json:"total"`
	ProxyIPTotal             int         `json:"proxyip_total"`
	LastScrape               string      `json:"last_scrape"`
	NextScrape               string      `json:"next_scrape"`
	LastScrapeAt             string      `json:"last_scrape_at,omitempty"`
	NextScrapeAt             string      `json:"next_scrape_at,omitempty"`
	LastSourceRefresh        string      `json:"last_source_refresh"`
	NextSourceRefresh        string      `json:"next_source_refresh"`
	LastSourceRefreshAt      string      `json:"last_source_refresh_at,omitempty"`
	NextSourceRefreshAt      string      `json:"next_source_refresh_at,omitempty"`
	LastFullRecheck          string      `json:"last_full_recheck"`
	NextFullRecheck          string      `json:"next_full_recheck"`
	LastFullRecheckAt        string      `json:"last_full_recheck_at,omitempty"`
	NextFullRecheckAt        string      `json:"next_full_recheck_at,omitempty"`
	Groups                   []GroupView `json:"groups"`
	ActiveProxy              string      `json:"active_proxy"`
	AvailableTotal           int         `json:"available_total"`
	UnavailableTotal         int         `json:"unavailable_total"`
	HealthRecheckPending     bool        `json:"health_recheck_pending"`
	Scrape                   ScrapeInfo  `json:"scrape"`
	CandidateTotal           int         `json:"candidate_total"`
	FailedCandidateTotal     int         `json:"failed_candidate_total"`
	IsolatedUnreachableTotal int         `json:"isolated_unreachable_total"`
	CandidatePhase           string      `json:"candidate_phase"`
	CandidateSourceErrors    int         `json:"candidate_source_errors"`
	CandidateUpdatedAt       string      `json:"candidate_updated_at,omitempty"`
}

type compactCandidateSummary struct {
	Total                    int
	FailedTotal              int
	IsolatedUnreachableTotal int
	Phase                    string
	SourceErrors             int
	UpdatedAt                string
}

func (s *StatusServer) compactCandidateStatus() compactCandidateSummary {
	if s.pool == nil || s.pool.candidates == nil {
		return compactCandidateSummary{Phase: "loading"}
	}
	snapshot := s.pool.candidates.snapshot.Load()
	if snapshot == nil {
		return compactCandidateSummary{Phase: "loading"}
	}
	known, _ := s.pool.candidateKnownSnapshot()
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	pending := 0
	for _, record := range snapshot.records {
		if candidatePendingRecord(snapshot, record, known) {
			pending++
		}
	}
	retryable, isolated := countRetryableFailedRecords(snapshot.failedRecords)
	return compactCandidateSummary{
		Total: pending, FailedTotal: retryable, IsolatedUnreachableTotal: isolated, Phase: snapshot.phase,
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

func pageWindowLimit(page, pageSize int) int {
	if page < 1 || pageSize < 1 {
		return 0
	}
	maxInt := int(^uint(0) >> 1)
	if page > maxInt/pageSize {
		return 0
	}
	limit := page * pageSize
	if limit == maxInt {
		return 0
	}
	return limit
}

func pageCountForTotal(total, pageSize int) int {
	if total < 1 || pageSize < 1 {
		return 1
	}
	pageCount := total / pageSize
	if total%pageSize != 0 {
		pageCount++
	}
	return pageCount
}

// pageCollectorWindow chooses the smaller side of the sorted result set to
// retain. Early pages keep the prefix through end; late pages keep the suffix
// from start. In particular, the first and last pages retain at most pageSize
// entries rather than growing the collector to the full filtered pool.
func pageCollectorWindow(total, start, end int) (limit int, reverse bool) {
	prefix := end
	suffix := total - start
	if suffix < prefix {
		return suffix, true
	}
	return prefix, false
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

// boundedCollector keeps at most k elements in ascending presentation order
// (items[0] is the "best"/first-in-page, items[k-1] is the "worst"/last-in-page).
// The caller supplies `less`, the canonical presentation comparator
// (less(a,b) == true means a ranks before b). add rejects items that would
// land beyond position k, so the retained memory is bounded by k regardless
// of input size. k is clamped to 1; a k<=0 collector drops everything.
type boundedCollector[T any] struct {
	items []T
	k     int
	less  func(a, b T) bool // canonical presentation less: a ranks before b
}

func newBoundedCollector[T any](k int, less func(a, b T) bool) *boundedCollector[T] {
	if k < 1 {
		k = 1
	}
	return &boundedCollector[T]{
		items: make([]T, 0, k),
		k:     k,
		less:  less,
	}
}

func (c *boundedCollector[T]) add(item T) {
	// Find the first index where item ranks before items[idx].
	idx := sort.Search(len(c.items), func(i int) bool {
		return c.less(item, c.items[i])
	})
	if len(c.items) < c.k {
		c.items = append(c.items, item)
		copy(c.items[idx+1:], c.items[idx:len(c.items)-1])
		c.items[idx] = item
		return
	}
	// Full. If idx >= k the item ranks after the worst retained -> drop it.
	if idx >= c.k {
		return
	}
	// Insert item at idx, evicting items[k-1] (the current worst).
	copy(c.items[idx+1:], c.items[idx:c.k-1])
	c.items[idx] = item
}

// sortFinal returns a copy of the retained items sorted by the given
// presentation ordering. Since items are already kept in ascending order,
// this is a stable copy; lessFinal is accepted for symmetry with callers
// that may re-sort with a different tie-breaker.
func (c *boundedCollector[T]) sortFinal(lessFinal func(a, b T) bool) []T {
	out := append([]T(nil), c.items...)
	sort.SliceStable(out, func(i, j int) bool { return lessFinal(out[i], out[j]) })
	return out
}
