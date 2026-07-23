package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type StatusServer struct {
	pool                 *ProxyPool
	store                *ConfigStore
	coordinator          *RefreshCoordinator
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

// NewStatusServer preserves the historical no-auth status endpoint behavior.
// Operators that expose the dashboard beyond a trusted local network should
// construct it through NewStatusServerWithAdminCredentials instead.
func NewStatusServer(pool *ProxyPool, store *ConfigStore) *StatusServer {
	return NewStatusServerWithCoordinator(pool, store, defaultRefreshCoordinator)
}

// NewStatusServerWithCoordinator constructs a status server bound to the
// supplied refresh coordinator. A nil coordinator uses the package default.
func NewStatusServerWithCoordinator(pool *ProxyPool, store *ConfigStore, coordinator *RefreshCoordinator) *StatusServer {
	return newStatusServer(pool, store, coordinator, "", "")
}

// NewStatusServerWithAdminCredentials adds optional HTTP Basic Auth to every
// dashboard and API route. Supplying neither credential deliberately keeps the
// legacy no-auth behavior; Config.Validate rejects a partial flag pair in the
// normal application path.
func NewStatusServerWithAdminCredentials(pool *ProxyPool, store *ConfigStore, user, password string) *StatusServer {
	return newStatusServer(pool, store, defaultRefreshCoordinator, user, password)
}

func NewStatusServerWithAdminCredentialsAndCoordinator(pool *ProxyPool, store *ConfigStore, coordinator *RefreshCoordinator, user, password string) *StatusServer {
	return newStatusServer(pool, store, coordinator, user, password)
}

func newStatusServer(pool *ProxyPool, store *ConfigStore, coordinator *RefreshCoordinator, user, password string) *StatusServer {
	if coordinator == nil {
		coordinator = defaultRefreshCoordinator
	}
	s := &StatusServer{
		pool: pool, store: store, coordinator: coordinator,
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
