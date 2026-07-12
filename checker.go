package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	publicIPLookupURL   = "https://api.ipify.org/"
	publicIPMaxBytes    = int64(64)
	defaultGeoLookupURL = "https://ipwho.is/"
	geoResponseMaxBytes = int64(64 << 10)
	geoCityMaxBytes     = 256
	geoCityMaxRunes     = 128
	geoCacheTTL         = 24 * time.Hour
	geoCacheMaxEntries  = 20_000
)

type geoCacheEntry struct {
	country   string
	city      string
	continent string
	expiresAt time.Time
}

type geoLookupCall struct {
	done    chan struct{}
	entry   geoCacheEntry
	success bool
}

var geoCache = struct {
	sync.Mutex
	entries  map[string]geoCacheEntry
	inflight map[string]*geoLookupCall
}{
	entries:  make(map[string]geoCacheEntry),
	inflight: make(map[string]*geoLookupCall),
}

// CheckProxies concurrently verifies a list of candidate proxies against
// testURL - the sole criterion for "alive" is a real HTTP round-trip to
// testURL through DialUpstream. The built-in connectivity endpoint must return
// its expected 204, while a custom operator endpoint may return any 2xx.
// Non-forwarding resources such as Cloudflare Worker ProxyIP endpoints are
// ignored here. A raw TCP connect to port 443 only proves that a port is open,
// not that the required TLS/SNI reverse path works, and would create a
// misleading "available" result. main.go keeps those resources in the
// separate candidate catalog instead.
//
// Connectivity is checked FIRST, and geo lookup runs only on the handful that
// pass. Doing geo up-front on every candidate (often thousands) would exhaust
// a public provider's quota, and rate-limit responses must never be mistaken
// for country data.
//
// There is no additional country-based filter on top of the URL check -
// an earlier version also dropped nodes geolocated to China/Hong Kong, but
// that was redundant with (and sometimes contradicted) the real
// connectivity result: if a node genuinely can't reach testURL, it's
// already rejected above regardless of where it's geolocated.
//
// The second return value, unreachable, is the set of protocol-aware proxy
// keys (Proxy.Key())
// that were actually dialed and genuinely failed to connect - as opposed to
// ones that connected fine but got excluded from alive for a policy reason
// (a transparent proxy dropped by requireIPChange). Callers use this
// distinction to avoid marking a perfectly reachable, just policy-filtered
// node as "unavailable" (see ProxyPool.Update).
func CheckProxies(proxies []Proxy, timeout time.Duration, maxConcurrent int, requireIPChange bool, testURL string) (alive []Proxy, unreachable map[string]bool) {
	alive, unreachable, _ = checkProxiesDetailed(proxies, timeout, maxConcurrent, requireIPChange, testURL)
	return alive, unreachable
}

// checkProxiesDetailed also returns candidates that completed the configured
// health request but were excluded by policy (currently require-ip-change).
// Keeping that outcome separate prevents the candidate catalog from calling a
// checked transparent proxy either "deferred" or a connectivity failure.
func checkProxiesDetailed(proxies []Proxy, timeout time.Duration, maxConcurrent int, requireIPChange bool, testURL string) (alive []Proxy, unreachable, policyFiltered map[string]bool) {
	return checkProxiesDetailedContext(context.Background(), proxies, timeout, maxConcurrent, requireIPChange, testURL)
}

func checkProxiesDetailedContext(parent context.Context, proxies []Proxy, timeout time.Duration, maxConcurrent int, requireIPChange bool, testURL string) (alive []Proxy, unreachable, policyFiltered map[string]bool) {
	if parent == nil {
		parent = context.Background()
	}
	baseline := BaselineExitIP()
	var (
		mu      sync.Mutex
		dropped int // transparent proxies filtered by requireIPChange
		wg      sync.WaitGroup
		sem     = make(chan struct{}, maxConcurrent)
	)
	unreachable = make(map[string]bool, len(proxies))
	policyFiltered = make(map[string]bool)
	markUnreachable := func(key string) {
		mu.Lock()
		unreachable[key] = true
		mu.Unlock()
	}

checkLoop:
	for _, p := range proxies {
		select {
		case sem <- struct{}{}:
		case <-parent.Done():
			break checkLoop
		}
		wg.Add(1)
		go func(px Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			nodeContext, cancelNode := context.WithTimeout(parent, timeout)
			defer cancelNode()

			if !isForwardingProtocol(px.Protocol) {
				return
			} else {
				checked, ok, latency := checkCredentialCandidates(nodeContext, px, testURL, timeout)
				if !ok {
					if parent.Err() == nil {
						markUnreachable(px.Key())
					}
					return
				}
				px = checked
				px.LatencyMs = latency.Milliseconds()

				// Best-effort: discover the REAL exit IP (how the outside
				// world sees this proxy) and geolocate THAT, so country is
				// trustworthy. All of this is non-fatal - a node stays
				// alive even if the exit/geo probes are rate-limited; it
				// just falls back to source-supplied or front-IP geo.
				px.ExitIP = probeExitIPContext(nodeContext, px, timeout)
				px.IPChangeKnown = px.ExitIP != "" && baseline != ""
				px.IPChanged = px.IPChangeKnown && px.ExitIP != baseline

				// Drop transparent proxies that don't actually change the
				// exit IP - but only when we can positively tell (we have
				// both a baseline and a measured exit that match). Unknown
				// exits are kept rather than falsely dropped. This is a
				// policy exclusion, not a connectivity failure - the node
				// genuinely answered - so it does NOT go into unreachable.
				if requireIPChange && px.IPChangeKnown && !px.IPChanged {
					mu.Lock()
					dropped++
					policyFiltered[px.Key()] = true
					mu.Unlock()
					return
				}

				normalizeProxyGeoFields(&px)
				if lookupIP := proxyGeoLookupTarget(px); lookupIP != "" {
					c, ci, co := LookupGeoContext(nodeContext, lookupIP, timeout)
					if c != "" && c != "Unknown" {
						px.Country, px.City, px.Continent = c, ci, co
					} else if strings.TrimSpace(px.Country) == "" {
						px.Country = "Unknown"
					}
				}

				px.Anonymity = probeAnonymityContext(nodeContext, px, timeout)
			}
			if parent.Err() != nil {
				return
			}

			mu.Lock()
			alive = append(alive, px)
			mu.Unlock()
		}(p)
	}

	wg.Wait()
	if dropped > 0 {
		log.Printf("[checker] dropped %d transparent proxies (exit IP == our own egress %s; disable with -require-ip-change=false)", dropped, baseline)
	}
	return alive, unreachable, policyFiltered
}

// checkURL verifies a forwarding-capable proxy by fetching testURL through
// the upstream tunnel and checking that a real HTTP response comes back.
// Redirects and non-2xx responses fail: otherwise a captive/intercepting proxy
// could return its own error page and be advertised as healthy. The built-in
// connectivity endpoint is stricter and must return 204. The response body is
// never read (Close is called immediately after headers arrive), so this stays
// cheap even against a heavy page.
func checkURL(px Proxy, testURL string, timeout time.Duration) bool {
	return checkURLContext(context.Background(), px, testURL, timeout)
}

// checkCredentialCandidates validates all bounded credential declarations for
// one endpoint while sharing a single per-node deadline. Authentication
// rejections normally arrive quickly, allowing the next declaration to be
// tried, but a stalled endpoint cannot multiply timeout by the number of
// credentials. On success the working pair is promoted to the primary fields
// so every later probe and consumer uses the credential that was verified.
func checkCredentialCandidates(parent context.Context, px Proxy, testURL string, timeout time.Duration) (Proxy, bool, time.Duration) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return px, false, 0
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := time.Now()
	candidates := px.credentialCandidates()
	deadline, _ := ctx.Deadline()
	for index, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		remaining := time.Until(deadline)
		attemptsLeft := len(candidates) - index
		if remaining <= 0 || attemptsLeft <= 0 {
			break
		}
		attemptBudget := remaining / time.Duration(attemptsLeft)
		attemptContext, cancelAttempt := context.WithTimeout(ctx, attemptBudget)
		ok := checkURLContext(attemptContext, candidate, testURL, attemptBudget)
		cancelAttempt()
		if ok {
			return px.promoteCredential(candidate), true, time.Since(started)
		}
	}
	return px, false, time.Since(started)
}

// checkURLContext is the cancellation-aware form used by request-scoped manual
// verification. Background health work keeps the historical checkURL wrapper,
// while a canceled HTTP request can now stop an in-flight dial/handshake and
// prevent later retry attempts from starting.
func checkURLContext(parent context.Context, px Proxy, testURL string, timeout time.Duration) bool {
	ok, _ := checkURLContextDetailed(parent, px, testURL, timeout)
	return ok
}

func checkURLContextDetailed(parent context.Context, px Proxy, testURL string, timeout time.Duration) (bool, error) {
	return checkURLWithDialContext(parent, testURL, timeout, func(ctx context.Context, _, addr string) (net.Conn, error) {
		return DialUpstreamContext(ctx, px, addr, timeout)
	})
}

func checkURLCredentialCandidatesContext(parent context.Context, px Proxy, testURL string, timeout time.Duration) (Proxy, bool, error) {
	dialer := newCredentialCandidateDialer(px, timeout)
	ok, err := checkURLWithDialContext(parent, testURL, timeout, dialer.DialContext)
	if verified, found := dialer.Verified(); found {
		return verified, ok, err
	}
	return px, ok, err
}

func checkURLWithDialContext(parent context.Context, testURL string, timeout time.Duration, dialContext func(context.Context, string, string) (net.Conn, error)) (bool, error) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return false, fmt.Errorf("health-check timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	transport := &http.Transport{
		DialContext:       dialContext,
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// A redirect only proves the proxy can reach the redirect destination,
			// not that the configured health target itself returned success.
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	if !healthResponseStatusAccepted(testURL, resp.StatusCode) {
		return false, fmt.Errorf("health target returned %s", resp.Status)
	}
	return true, nil
}

func healthResponseStatusAccepted(testURL string, status int) bool {
	if strings.TrimSpace(testURL) == defaultCheckURL {
		return status == http.StatusNoContent
	}
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// probeExitIP fetches the proxy's real exit IP by asking a lenient
// "what's my IP" service through the tunnel. Returns "" on any failure -
// callers treat it as best-effort and never drop a node over it.
func probeExitIP(px Proxy, timeout time.Duration) string {
	return probeExitIPContext(context.Background(), px, timeout)
}

func probeExitIPContext(parent context.Context, px Proxy, timeout time.Duration) string {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return DialUpstreamContext(ctx, px, addr, timeout)
			},
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicIPLookupURL, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return readPublicIP(resp.Body)
}

// baselineExitIP is our latest direct (non-proxied) public egress IP. A proxy
// whose measured exit IP equals this doesn't actually change your public IP
// (it's transparent, or the whole host sits behind a transparent egress
// proxy). It remains "" until the first successful refresh.
var (
	baselineExitIP string
	baselineExitMu sync.RWMutex
)

// InitBaselineExit preserves the historical startup API. RefreshBaselineExit
// uses the same operation and may be called periodically so a changed host
// egress address does not leave transparent-proxy classification stale.
func InitBaselineExit(timeout time.Duration) bool {
	return RefreshBaselineExit(timeout)
}

func RefreshBaselineExit(timeout time.Duration) bool {
	success, _ := RefreshBaselineExitWithChange(timeout)
	return success
}

func RefreshBaselineExitWithChange(timeout time.Duration) (success, changed bool) {
	return refreshBaselineExitWithURLChange(publicIPLookupURL, timeout)
}

func refreshBaselineExitWithURL(endpoint string, timeout time.Duration) bool {
	success, _ := refreshBaselineExitWithURLChange(endpoint, timeout)
	return success
}

func refreshBaselineExitWithURLChange(endpoint string, timeout time.Duration) (success, changed bool) {
	client := newDirectHTTPClient(timeout)
	transport, _ := client.Transport.(*http.Transport)
	if transport != nil {
		defer transport.CloseIdleConnections()
	}
	resp, err := client.Get(endpoint)
	if err != nil {
		log.Printf("[baseline] direct egress probe failed; IP-change state is unknown and will be retried: %v", err)
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[baseline] direct egress probe returned %s; will retry", resp.Status)
		return false, false
	}
	ip := readPublicIP(resp.Body)
	if ip == "" {
		log.Printf("[baseline] direct egress probe returned an invalid IP; will retry")
		return false, false
	}
	baselineExitMu.Lock()
	changed = baselineExitIP != ip
	baselineExitIP = ip
	baselineExitMu.Unlock()
	if changed {
		log.Printf("[baseline] our direct egress IP = %s (proxies exiting from this IP are transparent)", ip)
	}
	return true, changed
}

// newDirectHTTPClient deliberately has no Proxy callback. In particular it
// must not inherit HTTP_PROXY/HTTPS_PROXY: the baseline is meaningful only
// when it measures this process' direct egress rather than an environment-wide
// forwarding proxy. Redirects are also refused so the direct trust boundary
// cannot silently move to an unrelated endpoint.
func newDirectHTTPClient(timeout time.Duration) *http.Client {
	transport := newNoProxyTransport()
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newNoProxyTransport() *http.Transport {
	var transport *http.Transport
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.Proxy = nil
	transport.DisableCompression = true
	return transport
}

func readPublicIP(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, publicIPMaxBytes+1))
	if err != nil || int64(len(data)) > publicIPMaxBytes {
		return ""
	}
	ip := strings.TrimSpace(string(data))
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	return parsed.String()
}

func BaselineExitIP() string {
	baselineExitMu.RLock()
	defer baselineExitMu.RUnlock()
	return baselineExitIP
}

// proxyLeakHeaders are request headers a proxy may inject that reveal it's
// a proxy (and sometimes the client's real IP).
var proxyLeakHeaders = []string{
	"X-Forwarded-For", "Via", "X-Real-Ip", "Forwarded",
	"Client-Ip", "Proxy-Connection", "X-Proxy-Id", "X-Forwarded",
}

// probeAnonymity classifies a proxy as elite/anonymous/transparent by
// fetching an endpoint that echoes the request headers it received, through
// the tunnel. Best-effort: returns "" (unknown) if the judge is
// unreachable. "transparent" = your real IP leaks; "anonymous" = it's
// detectable as a proxy but hides your IP; "elite" = neither.
func probeAnonymity(px Proxy, timeout time.Duration) string {
	return probeAnonymityContext(context.Background(), px, timeout)
}

func probeAnonymityContext(parent context.Context, px Proxy, timeout time.Duration) string {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	baseline := BaselineExitIP()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return DialUpstreamContext(ctx, px, addr, timeout)
			},
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://httpbin.org/get", nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var r struct {
		Origin  string            `json:"origin"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return ""
	}

	leak := baseline != "" && strings.Contains(r.Origin, baseline)
	proxyHdr := false
	for rawName, v := range r.Headers {
		for _, h := range proxyLeakHeaders {
			if !strings.EqualFold(rawName, h) {
				continue
			}
			proxyHdr = true
			if baseline != "" && strings.Contains(v, baseline) {
				leak = true
			}
			break
		}
	}
	switch {
	case leak:
		return "transparent"
	case proxyHdr:
		return "anonymous"
	case baseline == "":
		return ""
	default:
		return "elite"
	}
}

// LookupGeo queries an HTTPS geolocation endpoint, returning the ISO country
// code (e.g. "US"), city, and continent code (e.g. "NA" - one of the standard
// AS/NA/EU/AF/SA/OC/AN codes used by the EDT-Pages feeds). It is best-effort
// and only runs after proxy connectivity succeeded.
//
// The response is never transparently decompressed, is capped at 64 KiB, and
// every returned display/grouping field is validated before entering pool
// state. This prevents a compromised/rate-limited provider from injecting
// control characters or unbounded strings into the API and dashboard.
func LookupGeo(ip string, timeout time.Duration) (country, city, continent string) {
	return LookupGeoContext(context.Background(), ip, timeout)
}

func LookupGeoContext(parent context.Context, ip string, timeout time.Duration) (country, city, continent string) {
	return lookupGeoContextWithBaseURL(parent, ip, timeout, defaultGeoLookupURL)
}

// proxyGeoLookupTarget returns the address whose location still needs to be
// resolved. Source-supplied ISO country/continent data is authoritative enough
// when the measured exit is the same endpoint IP; a different exit must be
// looked up because the front address can be in another country entirely.
func proxyGeoLookupTarget(px Proxy) string {
	_, countryOK := normalizeCountryCode(px.Country)
	_, continentOK := normalizeContinentCode(px.Continent)
	sourceGeoValid := countryOK && continentOK
	if px.ExitIP != "" {
		if sameIPAddress(px.ExitIP, px.IP) && sourceGeoValid {
			return ""
		}
		return px.ExitIP
	}
	if sourceGeoValid {
		return ""
	}
	return px.IP
}

func normalizeProxyGeoFields(px *Proxy) bool {
	if px == nil {
		return false
	}
	country, countryOK := normalizeCountryCode(px.Country)
	if countryOK {
		px.Country = country
	} else {
		px.Country = ""
	}
	continent, continentOK := normalizeContinentCode(px.Continent)
	if continentOK {
		px.Continent = continent
	} else {
		px.Continent = ""
	}
	city, cityOK := normalizeGeoCity(px.City)
	if cityOK {
		px.City = city
	} else {
		px.City = ""
	}
	return countryOK && continentOK
}

func sameIPAddress(left, right string) bool {
	leftIP, rightIP := net.ParseIP(strings.TrimSpace(left)), net.ParseIP(strings.TrimSpace(right))
	return leftIP != nil && rightIP != nil && leftIP.Equal(rightIP)
}

// lookupGeoContextWithBaseURL keeps production on a fixed HTTPS provider while
// allowing deterministic httptest coverage without reaching the public
// network. baseURL is internal-only and is not derived from user input.
func lookupGeoContextWithBaseURL(parent context.Context, ip string, timeout time.Duration, baseURL string) (country, city, continent string) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return "Unknown", "", ""
	}
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil || strings.TrimSpace(ip) != ip {
		return "Unknown", "", ""
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return "Unknown", "", ""
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cacheKey := endpoint.String() + "\x00" + parsedIP.String()
	if cached, call, leader := geoCacheLookupOrStart(cacheKey, time.Now()); cached != nil {
		return cached.country, cached.city, cached.continent
	} else if !leader {
		select {
		case <-call.done:
			if call.success {
				return call.entry.country, call.entry.city, call.entry.continent
			}
			return "Unknown", "", ""
		case <-ctx.Done():
			return "Unknown", "", ""
		}
	} else {
		country, city, continent = fetchGeoContext(ctx, parsedIP.String(), endpoint)
		success := country != "" && country != "Unknown"
		geoCacheFinish(cacheKey, call, geoCacheEntry{country: country, city: city, continent: continent}, success, time.Now())
		return country, city, continent
	}
}

func fetchGeoContext(ctx context.Context, ip string, endpoint *url.URL) (country, city, continent string) {
	requestURL := *endpoint
	requestURL.Path = strings.TrimRight(requestURL.Path, "/") + "/" + ip
	query := requestURL.Query()
	query.Set("fields", "success,country_code,city,continent_code")
	requestURL.RawQuery = query.Encode()

	transport := newNoProxyTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return "Unknown", "", ""
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return "Unknown", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "Unknown", "", ""
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return "Unknown", "", ""
	}
	if resp.ContentLength > geoResponseMaxBytes {
		return "Unknown", "", ""
	}

	var r struct {
		Success       bool   `json:"success"`
		CountryCode   string `json:"country_code"`
		City          string `json:"city"`
		ContinentCode string `json:"continent_code"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, geoResponseMaxBytes+1))
	if err != nil || int64(len(body)) > geoResponseMaxBytes || json.Unmarshal(body, &r) != nil || !r.Success {
		return "Unknown", "", ""
	}
	country, ok := normalizeCountryCode(r.CountryCode)
	if !ok {
		return "Unknown", "", ""
	}
	continent, ok = normalizeContinentCode(r.ContinentCode)
	if !ok {
		return "Unknown", "", ""
	}
	city, ok = normalizeGeoCity(r.City)
	if !ok {
		return "Unknown", "", ""
	}
	return country, city, continent
}

// geoCacheLookupOrStart serves a fresh successful result, joins an existing
// request for the same provider/IP, or elects the caller as the single leader.
// Coalescing is important during a concurrent health pass: a TTL cache alone
// would still allow every goroutine to miss and consume provider quota at once.
func geoCacheLookupOrStart(key string, now time.Time) (cached *geoCacheEntry, call *geoLookupCall, leader bool) {
	geoCache.Lock()
	defer geoCache.Unlock()
	if entry, ok := geoCache.entries[key]; ok {
		if now.Before(entry.expiresAt) {
			copy := entry
			return &copy, nil, false
		}
		delete(geoCache.entries, key)
	}
	if call := geoCache.inflight[key]; call != nil {
		return nil, call, false
	}
	call = &geoLookupCall{done: make(chan struct{})}
	geoCache.inflight[key] = call
	return nil, call, true
}

func geoCacheFinish(key string, call *geoLookupCall, entry geoCacheEntry, success bool, now time.Time) {
	geoCache.Lock()
	defer geoCache.Unlock()
	if success {
		entry.expiresAt = now.Add(geoCacheTTL)
		if _, exists := geoCache.entries[key]; !exists && len(geoCache.entries) >= geoCacheMaxEntries {
			// The cache is a quota shield, not a user-visible database. Arbitrary
			// bounded eviction avoids an O(n) LRU scan on every new address once a
			// large deployment reaches the cap; every retained item still observes
			// its TTL on lookup.
			for cacheKey := range geoCache.entries {
				delete(geoCache.entries, cacheKey)
				break
			}
		}
		geoCache.entries[key] = entry
	}
	call.entry = entry
	call.success = success
	delete(geoCache.inflight, key)
	close(call.done)
}

// resetGeoLookupCacheForTest clears completed entries between deterministic
// tests. In-flight calls are deliberately retained so resetting cannot strand
// goroutines already waiting on their completion channel.
func resetGeoLookupCacheForTest() {
	geoCache.Lock()
	geoCache.entries = make(map[string]geoCacheEntry)
	geoCache.Unlock()
}

func normalizeCountryCode(raw string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return "", false
	}
	return code, true
}

func normalizeContinentCode(raw string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	switch code {
	case "AF", "AN", "AS", "EU", "NA", "OC", "SA":
		return code, true
	default:
		return "", false
	}
}

func normalizeGeoCity(raw string) (string, bool) {
	city := strings.TrimSpace(raw)
	if city == "" {
		return "", true
	}
	if len(city) > geoCityMaxBytes || !utf8.ValidString(city) || utf8.RuneCountInString(city) > geoCityMaxRunes {
		return "", false
	}
	for _, r := range city {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return "", false
		}
	}
	return city, true
}
