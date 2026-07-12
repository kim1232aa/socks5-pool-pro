package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type StatusServer struct {
	pool                 *ProxyPool
	store                *ConfigStore
	adminAuthEnabled     bool
	adminUserHash        [sha256.Size]byte
	adminPassHash        [sha256.Size]byte
	serverMu             sync.Mutex
	server               *http.Server
	speedMu              sync.Mutex
	speedRunning         map[string]struct{}
	speedSlots           chan struct{}
	proxyIPVerifyMu      sync.Mutex
	proxyIPVerifyRunning map[string]*proxyIPVerifyCall
	proxyIPVerifySlots   chan struct{}
	nodeVerifyMu         sync.Mutex
	nodeVerifyRunning    map[string]struct{}
	nodeVerifySlots      chan struct{}
	nodeVerifyOps        manualNodeVerifyOperations
	// trustedManagementProxies is an explicit, exact-IP allowlist for a local
	// reverse proxy or container bridge. It never accepts CIDRs and is consulted
	// only when admin authentication is disabled.
	trustedManagementProxies map[string]struct{}
}

// apiBootNonce prevents generation counters that restart at zero from making a
// snapshot token from a previous process look valid after a restart.
var apiBootNonce = newAPIBootNonce()

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

// NewStatusServer preserves the historical no-auth status endpoint behavior.
// Operators that expose the dashboard beyond a trusted local network should
// construct it through NewStatusServerWithAdminCredentials instead.
func NewStatusServer(pool *ProxyPool, store *ConfigStore) *StatusServer {
	return NewStatusServerWithAdminCredentials(pool, store, "", "")
}

// NewStatusServerWithAdminCredentials adds optional HTTP Basic Auth to every
// dashboard and API route. Supplying neither credential deliberately keeps the
// legacy no-auth behavior; Config.Validate rejects a partial flag pair in the
// normal application path.
func NewStatusServerWithAdminCredentials(pool *ProxyPool, store *ConfigStore, user, password string) *StatusServer {
	s := &StatusServer{
		pool: pool, store: store,
		speedRunning:         make(map[string]struct{}),
		speedSlots:           make(chan struct{}, 4),
		proxyIPVerifyRunning: make(map[string]*proxyIPVerifyCall),
		proxyIPVerifySlots:   make(chan struct{}, maxProxyIPVerifyConcurrent),
		nodeVerifyRunning:    make(map[string]struct{}),
		nodeVerifySlots:      make(chan struct{}, maxManualNodeVerifyConcurrent),
		nodeVerifyOps:        defaultManualNodeVerifyOperations(),
	}
	if pool != nil && store != nil {
		_, criterion := pool.HealthCriterion()
		if criterion == "" {
			pool.SetHealthCriterion(store.CheckURL())
		}
	}
	if user != "" || password != "" {
		s.adminAuthEnabled = true
		s.adminUserHash = sha256.Sum256([]byte(user))
		s.adminPassHash = sha256.Sum256([]byte(password))
	}
	return s
}

// SetTrustedManagementProxies permits unauthenticated management traffic from
// exact reverse-proxy peer IPs while retaining the loopback Host requirement.
// Call it during startup, before handler/Start is used. CIDRs, hostnames, ports,
// and malformed values are rejected rather than broadened implicitly.
func (s *StatusServer) SetTrustedManagementProxies(addresses []string) error {
	trusted := make(map[string]struct{}, len(addresses))
	for _, raw := range addresses {
		value := strings.TrimSpace(raw)
		ip := net.ParseIP(value)
		if value == "" || ip == nil {
			return fmt.Errorf("trusted management proxy %q must be an exact IP address", raw)
		}
		trusted[ip.String()] = struct{}{}
	}
	s.trustedManagementProxies = trusted
	return nil
}

// requirePost rejects any request that isn't a POST with 405, before the
// wrapped handler runs. Applied to every state-mutating single-purpose
// endpoint below - without it, a plain GET (e.g. from a link-preview bot,
// browser prefetch, or an accidentally-bookmarked URL) could trigger a
// destructive action like clearing nodes or overwriting routing rules.
// Endpoints that already switch on r.Method themselves (handleSources/
// handleRules/handleGroups, which serve GET+POST from one path) and the
// read-only handleNodeExport endpoint are not wrapped.
func requirePost(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h(w, r)
	}
}

// requireGet permits HEAD as the standard bodyless counterpart of GET. The
// wrapped handler may still build its response, but net/http suppresses the
// body on the wire while retaining the same status and headers.
func requireGet(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		h(w, r)
	}
}

func (s *StatusServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/assets/dashboard.css", requireGet(embeddedDashboardAsset("text/css; charset=utf-8", dashboardCSS)))
	mux.HandleFunc("/assets/dashboard.js", requireGet(embeddedDashboardAsset("text/javascript; charset=utf-8", dashboardJS)))
	mux.HandleFunc("/api/status", requireGet(s.handleAPIStatus))
	mux.HandleFunc("/api/v1/proxies", requireGet(s.handleV1Proxies))
	mux.HandleFunc("/api/v1/proxies/pick", requireGet(s.handleV1ProxyPick))
	mux.HandleFunc("/api/refresh", requirePost(s.handleRefresh))
	mux.HandleFunc("/api/refresh/status", requireGet(s.handleRefreshStatus))
	mux.HandleFunc("/api/health-recheck/status", requireGet(s.handleHealthRecheckStatus))
	mux.HandleFunc("/api/settings/check-url", s.handleCheckURL)

	mux.HandleFunc("/api/nodes", requireGet(s.handleNodes))
	mux.HandleFunc("/api/nodes/page", s.handleNodesPage)
	mux.HandleFunc("/api/candidates/page", s.handleCandidatesPage)
	mux.HandleFunc("/api/proxyip/verify", requirePost(s.handleProxyIPVerify))
	mux.HandleFunc("/api/nodes/switch", requirePost(s.handleNodeSwitch))
	mux.HandleFunc("/api/nodes/auto", requirePost(s.handleNodeAuto))
	mux.HandleFunc("/api/nodes/verify", requirePost(s.handleNodeVerify))
	mux.HandleFunc("/api/nodes/clear-unavailable", requirePost(s.handleNodesClearUnavailable))
	mux.HandleFunc("/api/nodes/speedtest", requirePost(s.handleNodeSpeedtest))
	mux.HandleFunc("/api/nodes/export", requireGet(s.handleNodeExport))

	mux.HandleFunc("/api/sources", s.handleSources)
	mux.HandleFunc("/api/sources/toggle", requirePost(s.handleSourceToggle))
	mux.HandleFunc("/api/sources/delete", requirePost(s.handleSourceDelete))

	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/rules/delete", requirePost(s.handleRuleDelete))
	mux.HandleFunc("/api/rules/move", requirePost(s.handleRuleMove))
	mux.HandleFunc("/api/rules/default", requirePost(s.handleRuleDefault))
	mux.HandleFunc("/api/rules/preset-gfw", requirePost(s.handleRulePresetGFW))

	mux.HandleFunc("/api/groups", s.handleGroups)
	mux.HandleFunc("/api/groups/strategy", requirePost(s.handleGroupStrategy))
	mux.HandleFunc("/api/groups/delete", requirePost(s.handleGroupDelete))
	// A dedicated API catch-all prevents misspelled endpoints from falling
	// through to the dashboard with a misleading 200 text/html response.
	mux.HandleFunc("/api", s.handleAPINotFound)
	mux.HandleFunc("/api/", s.handleAPINotFound)

	// Authentication intentionally wraps the whole mux rather than individual
	// endpoints. This covers the dashboard, read-only APIs, mutating APIs, and
	// any future route registered above without a route being accidentally left
	// public. The sole exception is the exact /healthz liveness path below:
	// container orchestrators need a non-secret probe even when the management
	// surface is protected.
	protected := s.requireAdminAuth(gzipIfAccepted(protectStateChangingRequests(mux)))
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			s.handleHealthz(w, r)
			return
		}
		if r.URL.Path == "/readyz" {
			s.handleReadyz(w, r)
			return
		}
		if !s.adminAuthEnabled && (!isLoopbackManagementHost(r.Host) || !s.isTrustedManagementRemote(r.RemoteAddr)) {
			writeErrCode(w, http.StatusForbidden, "untrusted_client", fmt.Errorf("management API requires a loopback Host and loopback client address when admin authentication is disabled"))
			return
		}
		protected.ServeHTTP(w, r)
	})
	return withAPIResponseMetadata(root)
}

func isLoopbackManagementHost(authority string) bool {
	parsed := &url.URL{Host: strings.TrimSpace(authority)}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// The Host header is controlled by the client and is therefore insufficient
// to prove that an unauthenticated management request originated locally. The
// TCP peer recorded by net/http is the trust boundary; forwarded headers are
// deliberately ignored because an authenticated reverse proxy is required for
// non-loopback deployments.
func (s *StatusServer) isTrustedManagementRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	_, trusted := s.trustedManagementProxies[ip.String()]
	return trusted
}

// protectStateChangingRequests blocks browser cross-site writes while
// preserving curl/service clients that historically send no Origin headers.
// It deliberately does not enable CORS or require a browser-only CSRF token.
func protectStateChangingRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
			writeErrCode(w, http.StatusForbidden, "cross_site_request", fmt.Errorf("cross-site state-changing request rejected"))
			return
		}
		if rawOrigin := strings.TrimSpace(r.Header.Get("Origin")); rawOrigin != "" {
			origin, err := url.Parse(rawOrigin)
			scheme := strings.ToLower(origin.Scheme)
			if err != nil || (scheme != "http" && scheme != "https") || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || !sameOriginAuthority(origin, r.Host) {
				writeErrCode(w, http.StatusForbidden, "origin_mismatch", fmt.Errorf("request Origin does not match Host"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameOriginAuthority(origin *url.URL, requestHost string) bool {
	requestURL := &url.URL{Host: strings.TrimSpace(requestHost)}
	if !strings.EqualFold(strings.TrimSuffix(origin.Hostname(), "."), strings.TrimSuffix(requestURL.Hostname(), ".")) {
		return false
	}
	defaultPort := "80"
	if origin.Scheme == "https" {
		defaultPort = "443"
	}
	originPort := origin.Port()
	if originPort == "" {
		originPort = defaultPort
	}
	requestPort := requestURL.Port()
	if requestPort == "" {
		requestPort = defaultPort
	}
	return originPort == requestPort
}

func (s *StatusServer) Start(addr string) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	s.serverMu.Lock()
	if s.server != nil {
		s.serverMu.Unlock()
		return fmt.Errorf("status server is already running")
	}
	s.server = server
	s.serverMu.Unlock()

	err := server.ListenAndServe()
	s.serverMu.Lock()
	if s.server == server {
		s.server = nil
	}
	s.serverMu.Unlock()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the management HTTP server. It is safe to call
// before Start or after the server has already exited.
func (s *StatusServer) Shutdown(ctx context.Context) error {
	s.serverMu.Lock()
	server := s.server
	s.serverMu.Unlock()
	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}

// requireAdminAuth authenticates with fixed-size SHA-256 digests and
// constant-time comparisons, preventing username/password comparison from
// leaking a prefix match. It is a no-op when the optional credentials are not
// configured, preserving existing local deployments and API consumers.
func (s *StatusServer) requireAdminAuth(next http.Handler) http.Handler {
	if !s.adminAuthEnabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, supplied := r.BasicAuth()
		userHash := sha256.Sum256([]byte(user))
		passHash := sha256.Sum256([]byte(password))
		userOK := subtle.ConstantTimeCompare(userHash[:], s.adminUserHash[:])
		passOK := subtle.ConstantTimeCompare(passHash[:], s.adminPassHash[:])
		if !supplied || userOK&passOK != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="socks5-pool", charset="UTF-8"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleHealthz is intentionally independent of pool state, scrape state,
// configuration, and credentials. It is a liveness endpoint, not a readiness
// or status API, so callers learn only that this HTTP process can respond.
func (s *StatusServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "ok\n")
	}
}

// handleReadyz reports whether the first candidate inventory has been
// published. Unlike /healthz, it may return 503 during startup, but it remains
// deliberately data-free and unauthenticated for container orchestrators.
func (s *StatusServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	ready := s.pool != nil && s.pool.candidates != nil && s.pool.candidates.snapshot.Load() != nil
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !ready {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "not ready\n")
		}
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "ready\n")
	}
}

// ---- view models ----

type NodeView struct {
	Key               string  `json:"key"`
	Addr              string  `json:"addr"`
	Protocol          string  `json:"protocol"`
	Country           string  `json:"country"`
	City              string  `json:"city"`
	Continent         string  `json:"continent"` // AS/NA/EU/AF/SA/OC/AN - groups the dashboard's country filter
	Source            string  `json:"source"`
	ExitIP            string  `json:"exit_ip"`
	IPChanged         bool    `json:"ip_changed"`
	IPChangeKnown     bool    `json:"ip_change_known"`
	Anonymity         string  `json:"anonymity"`
	LatencyMs         int64   `json:"latency_ms"`
	SpeedKbps         float64 `json:"speed_kbps"`
	SpeedTestedAt     int64   `json:"speed_tested_at,omitempty"`
	SpeedBytes        int64   `json:"speed_bytes,omitempty"`
	SpeedDurationMs   int64   `json:"speed_duration_ms,omitempty"`
	Score             float64 `json:"score"`
	Successes         int     `json:"successes"`
	Failures          int     `json:"failures"`
	Active            bool    `json:"active"`    // this node is the ANY group's current upstream
	Available         bool    `json:"available"` // false = last check failed; kept in the pool, hidden by default
	SourceRetired     bool    `json:"source_retired,omitempty"`
	HealthInvalidated bool    `json:"health_invalidated,omitempty"`
	PolicyExcluded    bool    `json:"policy_excluded,omitempty"`
}

// NodeCountrySummary is the small, pool-wide country index used by the
// dashboard's country selector. It is intentionally independent of the
// current page filters, so changing a selector never makes other choices
// disappear merely because their rows are not on this page.
type NodeCountrySummary struct {
	Country   string `json:"country"`
	Continent string `json:"continent,omitempty"`
	Total     int    `json:"total"`
	Available int    `json:"available"`
}

// NodePageResponse is the bounded alternative to the legacy /api/nodes array.
// The dashboard receives just Nodes for one page while retaining the counters,
// country controls, and active-node context needed to render the node tab.
type NodePageResponse struct {
	Nodes               []NodeView           `json:"nodes"`
	SnapshotID          string               `json:"snapshot_id"`
	Page                int                  `json:"page"`
	PageSize            int                  `json:"page_size"`
	PageCount           int                  `json:"page_count"`
	HasNext             bool                 `json:"has_next"`
	FilteredTotal       int                  `json:"filtered_total"`
	PoolTotal           int                  `json:"pool_total"`
	AvailableTotal      int                  `json:"available_total"`
	UnavailableTotal    int                  `json:"unavailable_total"`
	Countries           []NodeCountrySummary `json:"countries"`
	CountryUnknownTotal int                  `json:"country_unknown_total"`
	Active              *NodeView            `json:"active,omitempty"`
}

type GroupView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Strategy  string   `json:"strategy"`
	Count     int      `json:"count"`
	Current   string   `json:"current"`
	Dynamic   bool     `json:"dynamic"` // current rotates per-connection (round-robin/random)
	Pinned    bool     `json:"pinned"`  // manually locked to Current; auto-rotation paused
	Builtin   bool     `json:"builtin"`
	Countries []string `json:"countries,omitempty"`
	Protocols []string `json:"protocols,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	Nodes     []string `json:"nodes,omitempty"`
}

type StatusSummary struct {
	Total        int         `json:"total"`
	ProxyIPTotal int         `json:"proxyip_total"`
	LastScrape   string      `json:"last_scrape"`
	NextScrape   string      `json:"next_scrape"`
	LastScrapeAt string      `json:"last_scrape_at,omitempty"`
	NextScrapeAt string      `json:"next_scrape_at,omitempty"`
	Groups       []GroupView `json:"groups"`
	// Keep active_proxy present even when the pool has no healthy selection.
	// Registration clients depend on a stable top-level extraction shape.
	ActiveProxy          string         `json:"active_proxy"`
	Proxies              []PoolAPIProxy `json:"proxies"`
	AvailableTotal       int            `json:"available_total"`
	UnavailableTotal     int            `json:"unavailable_total"`
	HealthRecheckPending bool           `json:"health_recheck_pending"`
	Scrape               ScrapeInfo     `json:"scrape"`
}

// PoolAPIProxy is a connection-ready, healthy upstream for consumers that
// fetch an address from an IP-pool API. The URL retains protocol and optional
// credentials, so a SOCKS5 upstream is never mistaken for an HTTP proxy.
type PoolAPIProxy struct {
	ProxyURL string `json:"proxy_url"`
	SocksURL string `json:"socks_url,omitempty"`
}

// V1ProxyView is the bounded, metadata-bearing alternative to the unbounded
// legacy /api/status proxies list. It deliberately retains the same
// connection-ready URL fields and never exposes Telegram links.
type V1ProxyView struct {
	ProxyURL string  `json:"proxy_url"`
	SocksURL string  `json:"socks_url,omitempty"`
	Key      string  `json:"key"`
	Protocol string  `json:"protocol"`
	Country  string  `json:"country"`
	City     string  `json:"city,omitempty"`
	Latency  int64   `json:"latency_ms"`
	Speed    float64 `json:"speed_kbps"`
	score    float64
}

type V1ProxyPage struct {
	APIVersion     string        `json:"api_version"`
	SnapshotID     string        `json:"snapshot_id"`
	Proxies        []V1ProxyView `json:"proxies"`
	Page           int           `json:"page"`
	PageSize       int           `json:"page_size"`
	PageCount      int           `json:"page_count"`
	HasNext        bool          `json:"has_next"`
	FilteredTotal  int           `json:"filtered_total"`
	AvailableTotal int           `json:"available_total"`
}

type V1ProxyPickResponse struct {
	APIVersion string      `json:"api_version"`
	SnapshotID string      `json:"snapshot_id"`
	Proxy      V1ProxyView `json:"proxy"`
}

func buildGroupViewsFromSnapshot(state statusPoolSnapshot, groups []Group) []GroupView {
	all := state.Proxies
	views := []GroupView{}
	anyCandidates, anyStrategy := resolveGroup(all, GroupAny, groups)
	anyCursor := state.GroupState[GroupAny]
	anyCurrent, anyOK, anyDynamic := effectiveCurrentFromSnapshot(anyCandidates, anyStrategy, anyCursor, state.Stats)
	anyAddr := ""
	if anyOK {
		anyAddr = anyCurrent.Addr()
	}
	views = append(views, GroupView{
		Name: GroupAny, Strategy: StrategySticky, Count: len(all),
		Current: anyAddr, Dynamic: anyDynamic, Builtin: true,
		Pinned: anyCursor != nil && anyCursor.pinned,
	})

	for _, g := range groups {
		candidates, strategy := resolveGroup(all, g.Name, groups)
		cur, ok, dynamic := effectiveCurrentFromSnapshot(candidates, strategy, state.GroupState[g.Name], state.Stats)
		current := ""
		if ok {
			current = cur.Addr()
		}
		views = append(views, GroupView{
			ID: g.ID, Name: g.Name, Strategy: strategy, Count: len(candidates),
			Current: current, Dynamic: dynamic,
			Countries: g.Countries, Protocols: g.Protocols, Sources: g.Sources, Nodes: g.Nodes,
		})
	}
	return views
}

func effectiveCurrentFromSnapshot(candidates []Proxy, strategy string, cursor *groupCursor, stats map[string]nodeStats) (Proxy, bool, bool) {
	if len(candidates) == 0 {
		return Proxy{}, false, false
	}
	stickyKey, lastPicked := "", ""
	if cursor != nil {
		stickyKey, lastPicked = cursor.stickyKey, cursor.lastPicked
	}
	find := func(key string) (Proxy, bool) {
		for _, candidate := range candidates {
			if candidate.Key() == key {
				return candidate, true
			}
		}
		return Proxy{}, false
	}
	switch strategy {
	case StrategyLatency:
		return bestBy(candidates, func(candidate Proxy) float64 { return float64(candidate.LatencyMs) }, false), true, false
	case StrategySpeed:
		return bestBy(candidates, func(candidate Proxy) float64 { return candidate.SpeedKbps }, true), true, false
	case StrategyScore:
		return bestBy(candidates, func(candidate Proxy) float64 {
			stat, ok := stats[candidate.Key()]
			if !ok {
				return scoreWithStats(candidate, nil)
			}
			return scoreWithStats(candidate, &stat)
		}, true), true, false
	case StrategyRoundRobin, StrategyRandom:
		if candidate, ok := find(lastPicked); ok {
			return candidate, true, true
		}
		return candidates[0], true, true
	default:
		if candidate, ok := find(stickyKey); ok {
			return candidate, true, false
		}
		return candidates[0], true, false
	}
}

// compactStatusPoolSnapshot contains exactly the pool-wide values needed by
// dashboard and compact-status polling. Unlike statusPoolSnapshot it never
// owns a detached copy of the proxy inventory.
type compactStatusPoolSnapshot struct {
	Total              int
	ProxyIPFallback    int
	AvailableTotal     int
	Groups             []GroupView
	Active             Proxy
	ActiveOK           bool
	HealthCheckPending bool
}

// streamingStatusSelection reproduces effectiveCurrentFromSnapshot while a
// caller walks matching proxies one at a time. Keeping both an all-routable
// and an available-only instance lets the caller apply resolveGroup's legacy
// "prefer healthy, otherwise use a soft-unavailable fallback" rule without
// ever materializing either candidate slice.
type streamingStatusSelection struct {
	count         int
	current       Proxy
	metric        float64
	cursorMatched bool
}

func (selection *streamingStatusSelection) observe(px Proxy, strategy string, cursor *groupCursor, score float64) {
	first := selection.count == 0
	selection.count++

	switch strategy {
	case StrategyLatency, StrategySpeed, StrategyScore:
		metric, higher := float64(px.LatencyMs), false
		if strategy == StrategySpeed {
			metric, higher = px.SpeedKbps, true
		} else if strategy == StrategyScore {
			metric, higher = score, true
		}
		if first || metric != 0 && (selection.metric == 0 || higher && metric > selection.metric || !higher && metric < selection.metric) {
			selection.current = px
			selection.metric = metric
		}
	case StrategyRoundRobin, StrategyRandom:
		if first {
			selection.current = px
		}
		lastPicked := ""
		if cursor != nil {
			lastPicked = cursor.lastPicked
		}
		if !selection.cursorMatched && statusProxyHasKey(px, lastPicked) {
			selection.current = px
			selection.cursorMatched = true
		}
	default: // sticky, including the historical empty-strategy fallback
		if first {
			selection.current = px
		}
		stickyKey := ""
		if cursor != nil {
			stickyKey = cursor.stickyKey
		}
		if !selection.cursorMatched && statusProxyHasKey(px, stickyKey) {
			selection.current = px
			selection.cursorMatched = true
		}
	}
}

// statusProxyHasKey compares a cursor identity without allocating a fresh
// Proxy.Key string for every proxy on every dashboard poll. The fallback
// retains exact historical behavior for malformed legacy values.
func statusProxyHasKey(px Proxy, key string) bool {
	prefixLen := len(px.Protocol) + len("://")
	if len(key) < prefixLen || key[:len(px.Protocol)] != px.Protocol || key[len(px.Protocol):prefixLen] != "://" {
		return false
	}
	host, port, err := net.SplitHostPort(key[prefixLen:])
	if err != nil {
		return px.Key() == key
	}
	return host == px.IP && port == px.Port
}

type streamingStatusGroup struct {
	group     Group
	strategy  string
	cursor    *groupCursor
	routable  streamingStatusSelection
	available streamingStatusSelection
}

func (group *streamingStatusGroup) observe(px Proxy, score float64) {
	group.routable.observe(px, group.strategy, group.cursor, score)
	if px.Available {
		group.available.observe(px, group.strategy, group.cursor, score)
	}
}

func (group *streamingStatusGroup) preferred() streamingStatusSelection {
	if group.available.count > 0 {
		return group.available
	}
	return group.routable
}

func statusProxyMatchesGroup(px Proxy, group Group) bool {
	if len(group.Nodes) > 0 && !groupMatchesNode(group.Nodes, px) {
		return false
	}
	if len(group.Countries) > 0 && !containsFold(group.Countries, px.Country) {
		return false
	}
	if len(group.Protocols) > 0 && !containsFold(group.Protocols, px.Protocol) {
		return false
	}
	return len(group.Sources) == 0 || proxyMatchesSources(px, group.Sources)
}

// captureCompactStatusPoolSnapshot performs one read-locked streaming pass
// across the live pool. Its retained memory is O(number of configured groups),
// not O(pool size) or O(pool size * groups): each group keeps only two running
// selections and counters.
func (s *StatusServer) captureCompactStatusPoolSnapshot(groups []Group) compactStatusPoolSnapshot {
	groupStates := make([]streamingStatusGroup, len(groups))
	for i, group := range groups {
		strategy := group.Strategy
		if strategy == "" {
			strategy = StrategySticky
		}
		groupStates[i] = streamingStatusGroup{group: group, strategy: strategy}
	}

	views := make([]GroupView, 0, len(groups)+1)
	s.pool.mu.RLock()
	defer s.pool.mu.RUnlock()

	any := streamingStatusGroup{strategy: StrategySticky, cursor: s.pool.groupState[GroupAny]}
	for i := range groupStates {
		groupStates[i].cursor = s.pool.groupState[groupStates[i].group.Name]
	}

	availableTotal := 0
	for _, px := range s.pool.proxies {
		// Hard routing exclusions win even if a legacy/corrupt cache contains an
		// inconsistent Available=true bit.
		if px.Available && proxyHardRoutable(px) {
			switch px.Protocol {
			case "socks5", "http", "https":
				availableTotal++
			}
		}
		if !proxyHardRoutable(px) {
			continue
		}

		any.observe(px, 0)
		score, scoreReady := 0.0, false
		for i := range groupStates {
			group := &groupStates[i]
			if !statusProxyMatchesGroup(px, group.group) {
				continue
			}
			if group.strategy == StrategyScore && !scoreReady {
				score = s.pool.scoreLocked(px)
				scoreReady = true
			}
			group.observe(px, score)
		}
	}

	anyPreferred := any.preferred()
	anyCurrent, anyOK := Proxy{}, anyPreferred.count > 0
	if anyOK {
		anyCurrent = anyPreferred.current
	}
	views = append(views, GroupView{
		Name: GroupAny, Strategy: StrategySticky, Count: len(s.pool.proxies),
		Current: statusSelectionAddr(anyPreferred), Builtin: true,
		Pinned: any.cursor != nil && any.cursor.pinned,
	})
	for i := range groupStates {
		group := &groupStates[i]
		preferred := group.preferred()
		dynamic := preferred.count > 0 && (group.strategy == StrategyRoundRobin || group.strategy == StrategyRandom)
		views = append(views, GroupView{
			ID: group.group.ID, Name: group.group.Name, Strategy: group.strategy, Count: preferred.count,
			Current: statusSelectionAddr(preferred), Dynamic: dynamic,
			Countries: group.group.Countries, Protocols: group.group.Protocols,
			Sources: group.group.Sources, Nodes: group.group.Nodes,
		})
	}

	return compactStatusPoolSnapshot{
		Total: len(s.pool.proxies), ProxyIPFallback: len(s.pool.proxyIPNodes),
		AvailableTotal: availableTotal, Groups: views, Active: anyCurrent, ActiveOK: anyOK,
		HealthCheckPending: s.pool.healthRecheckPending,
	}
}

func statusSelectionAddr(selection streamingStatusSelection) string {
	if selection.count == 0 {
		return ""
	}
	return selection.current.Addr()
}

// anyCurrentKey returns the protocol-aware identity of the node the ANY group
// would use right now (for marking exactly one active row in the node table).
func (s *StatusServer) anyCurrentKey() string {
	if px, ok, _ := s.pool.EffectiveCurrent(GroupAny, s.store.Groups()); ok {
		return px.Key()
	}
	return ""
}

func (s *StatusServer) buildSummary() StatusSummary {
	return s.buildSummaryWithProxies(true)
}

func (s *StatusServer) buildSummaryWithProxies(includeProxies bool) StatusSummary {
	scrapeState := getScrapeStatusSnapshot()
	last, next := scrapeState.Last, scrapeState.Next
	beijingLoc := time.FixedZone("CST", 8*3600)

	var lastStr, nextStr string
	if !last.IsZero() {
		lastStr = last.In(beijingLoc).Format("2006-01-02 15:04:05")
	}
	if !next.IsZero() {
		nextStr = next.In(beijingLoc).Format("2006-01-02 15:04:05")
	}

	groups := s.store.Groups()
	if !includeProxies {
		poolState := s.captureCompactStatusPoolSnapshot(groups)
		activeProxy := ""
		if poolState.ActiveOK && poolState.Active.Available {
			activeProxy = poolState.Active.ConsumerURL()
		}
		proxyIPTotal := poolState.ProxyIPFallback
		if catalogTotal, loaded := s.pool.candidates.protocolTotal("proxyip"); loaded {
			proxyIPTotal = catalogTotal
		}
		return StatusSummary{
			Total:                poolState.Total,
			ProxyIPTotal:         proxyIPTotal,
			LastScrape:           lastStr,
			NextScrape:           nextStr,
			LastScrapeAt:         formatRFC3339UTC(last),
			NextScrapeAt:         formatRFC3339UTC(next),
			Groups:               poolState.Groups,
			ActiveProxy:          activeProxy,
			AvailableTotal:       poolState.AvailableTotal,
			UnavailableTotal:     poolState.Total - poolState.AvailableTotal,
			HealthRecheckPending: poolState.HealthCheckPending,
			Scrape:               scrapeState.Info,
		}
	}

	includeStats := false
	for _, group := range groups {
		if group.Strategy == StrategyScore {
			includeStats = true
			break
		}
	}
	poolState := s.captureStatusPoolSnapshot(includeStats)
	availableTotal := countAPIPoolProxies(poolState.Proxies)
	proxies := apiPoolProxiesFrom(poolState.Proxies)
	availableTotal = len(proxies)
	activeProxy := ""
	if poolState.ActiveOK && poolState.Active.Available && proxyHardRoutable(poolState.Active) {
		activeProxy = poolState.Active.ConsumerURL()
	}

	proxyIPTotal := 0
	if catalogTotal, loaded := s.pool.candidates.protocolTotal("proxyip"); loaded {
		proxyIPTotal = catalogTotal
	} else {
		proxyIPTotal = s.pool.ProxyIPCount()
	}
	summary := StatusSummary{
		Total:                len(poolState.Proxies),
		ProxyIPTotal:         proxyIPTotal,
		LastScrape:           lastStr,
		NextScrape:           nextStr,
		LastScrapeAt:         formatRFC3339UTC(last),
		NextScrapeAt:         formatRFC3339UTC(next),
		Groups:               buildGroupViewsFromSnapshot(poolState, groups),
		ActiveProxy:          activeProxy,
		Proxies:              proxies,
		AvailableTotal:       availableTotal,
		UnavailableTotal:     len(poolState.Proxies) - availableTotal,
		HealthRecheckPending: s.pool.HealthRecheckPending(),
		Scrape:               scrapeState.Info,
	}
	return summary
}

func formatRFC3339UTC(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// apiPoolProxies exposes only nodes that passed the pool's health check.
// Stale nodes stay in the internal pool for self-healing, but must not be
// returned to the registration service, which dials the selected node
// directly and would otherwise repeatedly fail registration tasks.
func (s *StatusServer) apiPoolProxies() []PoolAPIProxy {
	return apiPoolProxiesFrom(s.pool.All())
}

func apiPoolProxiesFrom(proxies []Proxy) []PoolAPIProxy {
	out := make([]PoolAPIProxy, 0)
	for _, px := range proxies {
		if !px.Available || !proxyHardRoutable(px) {
			continue
		}
		switch px.Protocol {
		case "socks5", "http", "https":
		default:
			continue
		}

		proxyURL := px.ConsumerURL()
		view := PoolAPIProxy{
			ProxyURL: proxyURL,
		}
		if px.Protocol == "socks5" {
			view.SocksURL = proxyURL
		}
		out = append(out, view)
	}
	return out
}

func countAPIPoolProxies(proxies []Proxy) int {
	count := 0
	for _, px := range proxies {
		if !px.Available || !proxyHardRoutable(px) {
			continue
		}
		switch px.Protocol {
		case "socks5", "http", "https":
			count++
		}
	}
	return count
}

type statusPoolSnapshot struct {
	Proxies    []Proxy
	Generation uint64
	Active     Proxy
	ActiveOK   bool
	GroupState map[string]*groupCursor
	Stats      map[string]nodeStats
}

func (s *StatusServer) captureStatusPoolSnapshot(includeStats bool) statusPoolSnapshot {
	s.pool.mu.RLock()
	defer s.pool.mu.RUnlock()
	proxies := make([]Proxy, len(s.pool.proxies))
	for i, proxy := range s.pool.proxies {
		// The compatibility response needs a detached pool snapshot because it
		// emits every healthy proxy after releasing the lock. Compact/dashboard
		// polling uses captureCompactStatusPoolSnapshot and never enters this path.
		proxy.SourceNames = append([]string(nil), proxy.SourceNames...)
		proxy.SourceIDs = nil
		proxy.CredentialAlternates = nil
		proxies[i] = proxy
	}
	active, activeOK := effectiveAnyCurrentLocked(proxies, s.pool.groupState[GroupAny])
	groupState := make(map[string]*groupCursor, len(s.pool.groupState))
	for name, cursor := range s.pool.groupState {
		if cursor == nil {
			continue
		}
		copy := *cursor
		groupState[name] = &copy
	}
	var stats map[string]nodeStats
	if includeStats {
		stats = make(map[string]nodeStats, len(s.pool.stats))
		for key, stat := range s.pool.stats {
			if stat != nil {
				stats[key] = *stat
			}
		}
	}
	return statusPoolSnapshot{
		Proxies: proxies, Generation: s.pool.cacheGeneration, Active: active, ActiveOK: activeOK,
		GroupState: groupState, Stats: stats,
	}
}

// effectiveAnyCurrentLocked mirrors EffectiveCurrent(GroupAny) against a
// detached pool slice while the caller holds the pool lock that protects the
// sticky cursor. Healthy nodes are preferred; if none are healthy the legacy
// no-blackhole fallback still reports the first retained node.
func effectiveAnyCurrentLocked(proxies []Proxy, cursor *groupCursor) (Proxy, bool) {
	if len(proxies) == 0 {
		return Proxy{}, false
	}
	candidates := make([]Proxy, 0, len(proxies))
	for _, px := range proxies {
		if proxyHardRoutable(px) {
			candidates = append(candidates, px)
		}
	}
	if len(candidates) == 0 {
		return Proxy{}, false
	}
	if available := filterAvailable(candidates); len(available) > 0 {
		candidates = available
	}
	stickyKey := ""
	if cursor != nil {
		stickyKey = cursor.stickyKey
	}
	for _, px := range candidates {
		if px.Key() == stickyKey {
			return px, true
		}
	}
	return candidates[0], true
}

type DashboardData struct {
	StatusSummary
	Sources      []Source
	Rules        []Rule
	DefaultGroup string
	GroupOptions []string
	RuleTypes    []string
	Formats      []string
	Strategies   []string
	CheckURL     string
}

func nodeViewOf(px Proxy, activeKey string) NodeView {
	source := px.SourceName
	if len(px.SourceNames) > 0 {
		source = strings.Join(px.SourceNames, ", ")
	}
	return NodeView{
		Key: px.Key(), Addr: px.Addr(), Protocol: px.Protocol,
		Country: px.Country, City: px.City, Continent: px.Continent, Source: source,
		ExitIP: px.ExitIP, IPChanged: px.IPChanged, IPChangeKnown: px.IPChangeKnown, Anonymity: px.Anonymity,
		LatencyMs: px.LatencyMs, SpeedKbps: px.SpeedKbps,
		SpeedTestedAt: px.SpeedTestedAt, SpeedBytes: px.SpeedBytes, SpeedDurationMs: px.SpeedDurationMs,
		Active:        activeKey != "" && px.Key() == activeKey,
		Available:     px.Available && proxyHardRoutable(px),
		SourceRetired: px.SourceRetired, HealthInvalidated: px.HealthInvalidated, PolicyExcluded: px.PolicyExcluded,
	}
}

// nodeViews returns the live forwarding node list with the ANY group's
// current upstream flagged Active.
func (s *StatusServer) nodeViews() []NodeView {
	activeKey := s.anyCurrentKey()
	nodes := make([]NodeView, 0, s.pool.Size())
	for _, px := range s.pool.All() {
		nv := nodeViewOf(px, activeKey)
		nv.Score = s.pool.Score(px)
		nv.Successes, nv.Failures = s.pool.StatsOf(px.Key())
		nodes = append(nodes, nv)
	}
	return nodes
}

func (s *StatusServer) buildDashboardData() DashboardData {
	summary := s.buildSummaryWithProxies(false)

	groupOptions := []string{GroupAny, GroupDirect}
	for _, g := range s.store.Groups() {
		groupOptions = append(groupOptions, g.Name)
	}

	rules := s.store.Rules()
	defaultGroup := GroupAny
	for _, r := range rules {
		if r.Type == RuleMatch {
			defaultGroup = r.Group
			break
		}
	}

	return DashboardData{
		StatusSummary: summary,
		Sources:       safeManagementSources(s.store.Sources()),
		Rules:         rules,
		DefaultGroup:  defaultGroup,
		GroupOptions:  groupOptions,
		RuleTypes:     []string{RuleDomain, RuleDomainSuffix, RuleDomainKeyword, RuleIPCIDR, RuleGeosite},
		Formats:       []string{FormatTextRegex, FormatEDTJSON, FormatProxyIPJSON, FormatPlainList, FormatJSONArray},
		Strategies:    []string{StrategySticky, StrategyRoundRobin, StrategyRandom, StrategyLatency, StrategySpeed, StrategyScore},
		CheckURL:      s.store.CheckURL(),
	}
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

// ---- handlers: dashboard + status ----

func (s *StatusServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	data := s.buildDashboardData()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	dashboardTmpl.Execute(w, data)
}

func (s *StatusServer) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("compact") == "1" {
		summary := s.buildSummaryWithProxies(false)
		candidate := s.compactCandidateStatus()
		writeJSON(w, compactStatusSummary{
			Total: summary.Total, ProxyIPTotal: summary.ProxyIPTotal,
			LastScrape: summary.LastScrape, NextScrape: summary.NextScrape,
			LastScrapeAt: summary.LastScrapeAt, NextScrapeAt: summary.NextScrapeAt,
			Groups: summary.Groups, ActiveProxy: summary.ActiveProxy,
			AvailableTotal: summary.AvailableTotal, UnavailableTotal: summary.UnavailableTotal,
			HealthRecheckPending: summary.HealthRecheckPending,
			Scrape:               summary.Scrape,
			CandidateTotal:       candidate.Total, CandidatePhase: candidate.Phase,
			CandidateSourceErrors: candidate.SourceErrors, CandidateUpdatedAt: candidate.UpdatedAt,
		})
		return
	}
	writeJSON(w, s.buildSummary())
}

func (s *StatusServer) handleV1Proxies(w http.ResponseWriter, r *http.Request) {
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
		return
	}
	protocol, err := validatedV1ProxyProtocol(r.URL.Query().Get("protocol"))
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_protocol", err)
		return
	}
	page, pageSize, err := strictV1PageParams(r)
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_pagination", err)
		return
	}
	all, snapshotID := s.v1HealthyProxySnapshot()
	w.Header().Set("X-Snapshot-ID", snapshotID)
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" && requested != snapshotID {
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	filtered := filterV1ProxyViews(all, protocol, r)
	pageCount := (len(filtered) + pageSize - 1) / pageSize
	if pageCount < 1 {
		pageCount = 1
	}
	if page > pageCount {
		writeErrCode(w, http.StatusBadRequest, "page_out_of_range", fmt.Errorf("page %d exceeds page_count %d", page, pageCount))
		return
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	rows := make([]V1ProxyView, 0, end-start)
	if start < len(filtered) {
		rows = append(rows, filtered[start:end]...)
	}
	writeJSON(w, V1ProxyPage{
		APIVersion: "v1", SnapshotID: snapshotID, Proxies: rows,
		Page: page, PageSize: pageSize, PageCount: pageCount, HasNext: page < pageCount,
		FilteredTotal: len(filtered), AvailableTotal: len(all),
	})
}

func (s *StatusServer) handleV1ProxyPick(w http.ResponseWriter, r *http.Request) {
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
		return
	}
	protocol, err := validatedV1ProxyProtocol(r.URL.Query().Get("protocol"))
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_protocol", err)
		return
	}
	all, snapshotID := s.v1HealthyProxySnapshot()
	snapshotID = formatV1ProxyPickSnapshotID(all)
	w.Header().Set("X-Snapshot-ID", snapshotID)
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" && requested != snapshotID {
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	filtered := filterV1ProxyViews(all, protocol, r)
	if len(filtered) == 0 {
		writeErrCode(w, http.StatusNotFound, "proxy_not_found", fmt.Errorf("no healthy proxy matches the requested filters"))
		return
	}
	selected := filtered[0]
	for _, candidate := range filtered[1:] {
		if candidate.score > selected.score || candidate.score == selected.score && candidate.Key < selected.Key {
			selected = candidate
		}
	}
	writeJSON(w, V1ProxyPickResponse{APIVersion: "v1", SnapshotID: snapshotID, Proxy: selected})
}

func (s *StatusServer) v1HealthyProxySnapshot() ([]V1ProxyView, string) {
	s.pool.mu.RLock()
	views := make([]V1ProxyView, 0, len(s.pool.proxies))
	for _, px := range s.pool.proxies {
		if !px.Available || !proxyHardRoutable(px) {
			continue
		}
		switch px.Protocol {
		case "socks5", "http", "https":
		default:
			continue
		}
		proxyURL := px.ConsumerURL()
		view := V1ProxyView{
			ProxyURL: proxyURL, Key: px.Key(), Protocol: px.Protocol,
			Country: normalizedNodeCountry(px.Country), City: px.City,
			Latency: px.LatencyMs, Speed: px.SpeedKbps, score: s.pool.scoreLocked(px),
		}
		if px.Protocol == "socks5" {
			view.SocksURL = proxyURL
		}
		views = append(views, view)
	}
	s.pool.mu.RUnlock()
	sort.SliceStable(views, func(i, j int) bool { return views[i].Key < views[j].Key })
	return views, formatV1ProxySnapshotID(views)
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

func (s *StatusServer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	operation, accepted := RequestRefresh()
	w.Header().Set("Location", "/api/refresh/status")
	writeJSONStatus(w, http.StatusAccepted, struct {
		RefreshOperation
		Accepted  bool   `json:"accepted"`
		Coalesced bool   `json:"coalesced"`
		StatusURL string `json:"status_url"`
	}{
		RefreshOperation: operation,
		Accepted:         accepted, Coalesced: !accepted, StatusURL: "/api/refresh/status",
	})
}

func (s *StatusServer) handleRefreshStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, getRefreshOperationStatus())
}

func (s *StatusServer) handleHealthRecheckStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, getHealthRecheckOperationStatus())
}

// handleCheckURL gets or sets the health-check target URL - the sole
// criterion for whether a node counts as alive (see checker.go checkURL).
// A successful POST immediately invalidates health learned under the old
// criterion, then schedules one full retained-pool recheck. Source inventory
// is unchanged, so a duplicate source scrape would only add load and delay.
func (s *StatusServer) handleCheckURL(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, map[string]string{"url": s.store.CheckURL()})
	case http.MethodPost:
		var in struct {
			URL string `json:"url"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		requestedURL := strings.TrimSpace(in.URL)
		if err := validateCheckURL(requestedURL); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if requestedURL == s.store.CheckURL() {
			writeJSON(w, struct {
				Status                 string `json:"status"`
				URL                    string `json:"url"`
				Changed                bool   `json:"changed"`
				InvalidatedTotal       int    `json:"invalidated_total"`
				CandidateOutcomesReset int    `json:"candidate_outcomes_reset"`
			}{Status: "ok", URL: requestedURL, Changed: false})
			return
		}
		if err := s.store.SetCheckURL(requestedURL); err != nil {
			writeConfigStoreError(w, err)
			return
		}
		invalidated := s.pool.InvalidateHealth(s.store.CheckURL())
		candidateOutcomesReset := s.pool.candidates.ResetHealthOutcomes()
		s.pool.FlushCache()
		operation, accepted := TriggerFullRecheck(s.pool)
		w.Header().Set("Location", "/api/health-recheck/status")
		writeJSON(w, struct {
			Status                 string                 `json:"status"`
			URL                    string                 `json:"url"`
			Changed                bool                   `json:"changed"`
			InvalidatedTotal       int                    `json:"invalidated_total"`
			CandidateOutcomesReset int                    `json:"candidate_outcomes_reset"`
			HealthRecheck          HealthRecheckOperation `json:"health_recheck"`
			Accepted               bool                   `json:"accepted"`
			StatusURL              string                 `json:"status_url"`
		}{
			Status: "ok", URL: s.store.CheckURL(), Changed: true,
			InvalidatedTotal: invalidated, CandidateOutcomesReset: candidateOutcomesReset,
			HealthRecheck: operation, Accepted: accepted, StatusURL: "/api/health-recheck/status",
		})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
	}
}

// ---- handlers: nodes ----

func (s *StatusServer) handleNodes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Sunset", "Thu, 31 Dec 2026 23:59:59 GMT")
	w.Header().Add("Link", `</api/nodes/page>; rel="successor-version"`)
	writeJSON(w, s.nodeViews())
}

const (
	defaultNodePageSize = 20
	maxNodePageSize     = 100
)

// handleNodesPage serves a bounded, server-filtered page for the dashboard.
// Keep handleNodes above as-is: external callers may still rely on its legacy
// plain JSON array contract.
func (s *StatusServer) handleNodesPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
		return
	}
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" {
		current := s.currentNodeSnapshotID()
		w.Header().Set("X-Snapshot-ID", current)
		if requested != current {
			writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
			return
		}
	}
	page := s.buildNodePage(r)
	w.Header().Set("X-Snapshot-ID", page.SnapshotID)
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" && requested != page.SnapshotID {
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	writeJSON(w, page)
}

func (s *StatusServer) currentNodeSnapshotID() string {
	s.pool.mu.RLock()
	generation := s.pool.cacheGeneration
	s.pool.mu.RUnlock()
	return formatPoolSnapshotID(generation)
}

func (s *StatusServer) buildNodePage(r *http.Request) NodePageResponse {
	page, pageSize := nodePageParams(r)
	query := r.URL.Query()
	search := strings.ToLower(strings.TrimSpace(query.Get("search")))
	countryRaw := strings.TrimSpace(query.Get("country"))
	unknownCountry := strings.EqualFold(countryRaw, "__unknown__") || nodeQueryEnabled(query.Get("country_unknown"))
	country := ""
	if !unknownCountry {
		country = normalizedNodeCountry(countryRaw)
	}
	protocol := strings.ToLower(strings.TrimSpace(query.Get("protocol")))
	onlyChanged := nodeQueryEnabled(query.Get("only_changed"))
	onlyAvailable := nodeQueryEnabled(query.Get("available")) || nodeQueryEnabled(query.Get("hide_unavailable"))
	sortBy := strings.ToLower(strings.TrimSpace(query.Get("sort")))

	s.pool.mu.RLock()
	poolGeneration := s.pool.cacheGeneration
	activeProxy, activeOK := effectiveAnyCurrentLocked(s.pool.proxies, s.pool.groupState[GroupAny])
	activeKey := ""
	if activeOK {
		activeKey = activeProxy.Key()
	}
	views := make([]NodeView, 0, len(s.pool.proxies))
	for _, liveProxy := range s.pool.proxies {
		px := cloneProxy(liveProxy)
		view := nodeViewOf(px, activeKey)
		view.Score = s.pool.scoreLocked(px)
		if stats := s.pool.stats[px.Key()]; stats != nil {
			view.Successes, view.Failures = stats.Successes, stats.Failures
		}
		views = append(views, view)
	}
	s.pool.mu.RUnlock()
	snapshotID := formatPoolSnapshotID(poolGeneration)
	countries := make(map[string]*NodeCountrySummary)
	availableTotal := 0
	unknownCountryTotal := 0
	var active *NodeView
	for _, view := range views {
		if view.Available {
			availableTotal++
		}
		if view.Active {
			activeCopy := view
			active = &activeCopy
		}

		if code := normalizedNodeCountry(view.Country); code != "" {
			summary := countries[code]
			if summary == nil {
				summary = &NodeCountrySummary{Country: code}
				countries[code] = summary
			}
			summary.Total++
			if view.Available {
				summary.Available++
			}
			if summary.Continent == "" && view.Continent != "" {
				summary.Continent = view.Continent
			}
		} else {
			unknownCountryTotal++
		}
	}

	filtered := make([]NodeView, 0, len(views))
	for _, view := range views {
		if search != "" && !strings.Contains(strings.ToLower(view.Addr+" "+view.ExitIP), search) {
			continue
		}
		if country != "" && normalizedNodeCountry(view.Country) != country {
			continue
		}
		if unknownCountry && normalizedNodeCountry(view.Country) != "" {
			continue
		}
		if protocol != "" && strings.ToLower(view.Protocol) != protocol {
			continue
		}
		if onlyChanged && !(view.IPChangeKnown && view.IPChanged) {
			continue
		}
		if onlyAvailable && !view.Available {
			continue
		}
		filtered = append(filtered, view)
	}
	sortNodeViews(filtered, sortBy)

	filteredTotal := len(filtered)
	pageCount := (filteredTotal + pageSize - 1) / pageSize
	if pageCount < 1 {
		pageCount = 1
	}
	if page > pageCount {
		page = pageCount
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > filteredTotal {
		end = filteredTotal
	}
	pageNodes := make([]NodeView, 0, end-start)
	if start < filteredTotal {
		pageNodes = append(pageNodes, filtered[start:end]...)
	}

	countryList := make([]NodeCountrySummary, 0, len(countries))
	for _, summary := range countries {
		countryList = append(countryList, *summary)
	}
	sort.Slice(countryList, func(i, j int) bool { return countryList[i].Country < countryList[j].Country })

	return NodePageResponse{
		Nodes:               pageNodes,
		SnapshotID:          snapshotID,
		Page:                page,
		PageSize:            pageSize,
		PageCount:           pageCount,
		HasNext:             page < pageCount,
		FilteredTotal:       filteredTotal,
		PoolTotal:           len(views),
		AvailableTotal:      availableTotal,
		UnavailableTotal:    len(views) - availableTotal,
		Countries:           countryList,
		CountryUnknownTotal: unknownCountryTotal,
		Active:              active,
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

func (s *StatusServer) handleNodeSwitch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	switch s.pool.forceSticky(GroupAny, in.Key) {
	case forceStickyNotFound:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("node not found: %s", in.Key))
		return
	case forceStickyUnavailable:
		writeErrCode(w, http.StatusConflict, "node_unavailable", fmt.Errorf("节点当前不可用，不能手动切换；请先复检并等待节点恢复"))
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "pinned": "true"})
}

// handleNodeAuto clears the manual lock on the default (ANY) group so the
// periodic auto-rotation resumes.
func (s *StatusServer) handleNodeAuto(w http.ResponseWriter, r *http.Request) {
	s.pool.SetAuto(GroupAny)
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleNodesClearUnavailable is an explicit, user-triggered purge of nodes
// currently marked unavailable. The pool never does this on its own (see
// ProxyPool.Update) - it's only ever invoked by a dashboard button click.
func (s *StatusServer) handleNodesClearUnavailable(w http.ResponseWriter, r *http.Request) {
	if s.pool.HealthRecheckPending() {
		writeErrCode(w, http.StatusConflict, "health_recheck_in_progress", fmt.Errorf("健康标准全量复检尚未完成，暂不能永久清理不可用节点"))
		return
	}
	n := s.pool.ClearUnavailable()
	writeJSON(w, map[string]int{"removed": n})
}

// handleNodeVerify re-probes a node's real exit IP/geo RIGHT NOW (dialing
// through the live tunnel, same as the periodic health check does), so the
// dashboard can answer "is this node's country label still accurate, and
// does it actually work" on demand instead of trusting a label that may be
// up to one scrape cycle (-scrape-interval, default 20m) stale.
func (s *StatusServer) handleNodeVerify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	healthGeneration, healthCheckURL := s.pool.HealthCriterion()
	if healthCheckURL == "" {
		s.pool.SetHealthCriterion(s.store.CheckURL())
		healthGeneration, healthCheckURL = s.pool.HealthCriterion()
	}
	px, ok := s.pool.Find(in.Key)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("node not found: %s", in.Key))
		return
	}
	if err := s.beginManualNodeVerify(in.Key); err != nil {
		w.Header().Set("Retry-After", "2")
		writeErrCode(w, http.StatusTooManyRequests, "node_verify_busy", err)
		return
	}
	defer s.endManualNodeVerify(in.Key)

	prevExitIP, prevCountry := px.ExitIP, px.Country
	verifyCtx, cancel := context.WithTimeout(r.Context(), manualNodeVerifyTotalTimeout)
	defer cancel()
	verified, reachable, attempts, latencyMs, err := runManualNodeVerifyChecks(
		verifyCtx, s.nodeVerifyOps, px, healthCheckURL,
	)
	if err != nil {
		writeManualNodeVerifyCanceled(w, attempts, err)
		return
	}

	exitIP := ""
	country, city, continent := "", "", ""
	if reachable {
		px = verified
		exitIP = s.nodeVerifyOps.probeExitIP(verifyCtx, px, manualNodeVerifyExitTimeout)
	}
	if reachable && exitIP != "" {
		country, city, continent = s.nodeVerifyOps.lookupGeo(verifyCtx, exitIP, manualNodeVerifyGeoTimeout)
		country = strings.TrimSpace(country)
		if strings.EqualFold(country, "Unknown") {
			country = ""
		}
	}
	// Cancellation is not a health observation. In particular, do not let a
	// browser navigation or client timeout mark the node unavailable (or record
	// a success) after the caller has stopped the verification.
	if err := verifyCtx.Err(); err != nil {
		writeManualNodeVerifyCanceled(w, attempts, err)
		return
	}
	if reachable {
		s.pool.UpdateVerifiedCredentialsAtGeneration(in.Key, verified, healthGeneration)
	}

	baseline := BaselineExitIP()
	ipChangeKnown := exitIP != "" && baseline != ""
	ipChanged := ipChangeKnown && exitIP != baseline
	policyAllowed := !s.pool.RequireIPChangePolicy() || !ipChangeKnown || ipChanged

	// Three transport attempts form one explicit health observation, not three
	// independent failures. A success revives immediately; a final failure joins
	// the same three-observation debounce used by background health work so one
	// unlucky manual click cannot evict an intermittently reachable node.
	if !s.pool.ObserveHealthOutcomeAtGeneration(in.Key, reachable, policyAllowed, latencyMs, healthGeneration) {
		if s.pool.HealthGeneration() != healthGeneration {
			writeErrCode(w, http.StatusConflict, "health_criterion_changed", fmt.Errorf("检测标准已改变，结果未应用"))
			return
		}
		writeErr(w, http.StatusConflict, fmt.Errorf("node disappeared while verification was running"))
		return
	}
	available, consecutiveFailures, stateOK := s.pool.HealthStateOf(in.Key)
	if !stateOK {
		writeErr(w, http.StatusConflict, fmt.Errorf("node disappeared while verification was running"))
		return
	}
	if exitIP != "" {
		s.pool.UpdateGeo(in.Key, exitIP, country, city, continent, ipChanged, ipChangeKnown)
	}
	reachableKeys := map[string]bool{}
	policyFiltered := map[string]bool{}
	if reachable {
		reachableKeys[in.Key] = true
	}
	if reachable && !policyAllowed {
		policyFiltered[in.Key] = true
	}
	s.pool.candidates.ApplyHealthOutcomes([]Proxy{px}, reachableKeys, policyFiltered)
	labelMatchKnown, labelMatched := manualNodeLabelMatch(country, prevCountry)
	// Manual verification is an explicit operator action, so make the health
	// state durable before replying instead of leaving it in the debounce window.
	s.pool.FlushCache()

	writeJSON(w, map[string]interface{}{
		"reachable":            reachable,
		"attempts":             attempts,
		"available":            available,
		"consecutive_failures": consecutiveFailures,
		"latency_ms":           latencyMs,
		"exit_ip":              exitIP,
		"country":              country,
		"city":                 city,
		"ip_changed":           ipChanged,
		"ip_change_known":      ipChangeKnown,
		"policy_excluded":      reachable && !policyAllowed,
		"prev_exit_ip":         prevExitIP,
		"prev_country":         prevCountry,
		"label_match_known":    labelMatchKnown,
		"label_matched":        labelMatched,
		"baseline_exit":        baseline,
	})
}

func (s *StatusServer) handleNodeSpeedtest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	healthGeneration := s.pool.HealthGeneration()
	px, ok := s.pool.Find(in.Key)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("node not found: %s", in.Key))
		return
	}
	if err := s.beginSpeedTest(in.Key); err != nil {
		w.Header().Set("Retry-After", "2")
		writeErr(w, http.StatusTooManyRequests, err)
		return
	}
	defer s.endSpeedTest(in.Key)

	result, verified, err := speedTestCredentialCandidatesContext(r.Context(), px, speedTestOperationTimeout)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.pool.UpdateVerifiedCredentialsAtGeneration(in.Key, verified, healthGeneration)
	if !s.pool.UpdateSpeed(in.Key, result.Kbps, result.Bytes, result.DurationMs) {
		writeErr(w, http.StatusConflict, fmt.Errorf("node disappeared while speed test was running"))
		return
	}
	// Speed test results are explicit user actions, so persist them before
	// replying rather than leaving them in the normal debounce window.
	s.pool.FlushCache()
	writeJSON(w, map[string]interface{}{
		"kbps": result.Kbps, "bytes": result.Bytes, "duration_ms": result.DurationMs,
	})
}

func (s *StatusServer) beginSpeedTest(key string) error {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	if _, running := s.speedRunning[key]; running {
		return fmt.Errorf("该节点正在测速")
	}
	select {
	case s.speedSlots <- struct{}{}:
		s.speedRunning[key] = struct{}{}
		return nil
	default:
		return fmt.Errorf("测速任务已满,请稍后重试")
	}
}

func (s *StatusServer) endSpeedTest(key string) {
	s.speedMu.Lock()
	delete(s.speedRunning, key)
	<-s.speedSlots
	s.speedMu.Unlock()
}

// ---- handlers: sources ----

func (s *StatusServer) handleSources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, safeManagementSources(s.store.Sources()))
	case http.MethodPost:
		var in Source
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.store.AddSource(in)
		if err != nil {
			writeConfigStoreError(w, err)
			return
		}
		TriggerRefresh()
		writeJSON(w, safeManagementSource(created))
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
	}
}

func (s *StatusServer) handleSourceToggle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sourceLifecycleMu.Lock()
	defer sourceLifecycleMu.Unlock()
	if err := s.store.ToggleSource(in.ID, in.Enabled); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	retired := s.pool.ApplyEnabledSources(s.store.Sources())
	TriggerRefresh()
	writeJSON(w, map[string]interface{}{"status": "ok", "retired_total": retired})
}

func (s *StatusServer) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sourceLifecycleMu.Lock()
	defer sourceLifecycleMu.Unlock()
	if err := s.store.DeleteSource(in.ID); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	retired := s.pool.ApplyEnabledSources(s.store.Sources())
	TriggerRefresh()
	writeJSON(w, map[string]interface{}{"status": "ok", "retired_total": retired})
}

// ---- handlers: rules ----

func (s *StatusServer) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, s.store.Rules())
	case http.MethodPost:
		var in Rule
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.store.AddRule(in)
		if err != nil {
			writeConfigStoreError(w, err)
			return
		}
		writeJSON(w, created)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
	}
}

func (s *StatusServer) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.DeleteRule(in.ID); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *StatusServer) handleRuleMove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    string `json:"id"`
		Delta int    `json:"delta"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.MoveRule(in.ID, in.Delta); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *StatusServer) handleRulePresetGFW(w http.ResponseWriter, r *http.Request) {
	if err := s.store.InstallGFWPreset(); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *StatusServer) handleRuleDefault(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Group string `json:"group"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.SetDefaultGroup(in.Group); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// ---- handlers: groups ----

func (s *StatusServer) handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, s.store.Groups())
	case http.MethodPost:
		var in Group
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.store.AddGroup(in)
		if err != nil {
			writeConfigStoreError(w, err)
			return
		}
		writeJSON(w, created)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
	}
}

func (s *StatusServer) handleGroupStrategy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       string `json:"id"`
		Strategy string `json:"strategy"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.SetGroupStrategy(in.ID, in.Strategy); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *StatusServer) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.DeleteGroup(in.ID); err != nil {
		writeConfigStoreError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// ---- dashboard template ----

var dashboardTmpl = template.Must(template.New("dashboard").Parse(dashboardHTML))
