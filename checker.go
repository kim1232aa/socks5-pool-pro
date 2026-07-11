package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CheckProxies concurrently verifies a list of candidate proxies against
// testURL - the sole criterion for "alive" is a real HTTP round-trip to
// testURL through DialUpstream (any response counts, not just 2xx - a
// 3xx/4xx still proves the tunnel and TLS handshake work end to end).
// Non-forwarding resources such as Cloudflare Worker ProxyIP endpoints are
// ignored here. A raw TCP connect to port 443 only proves that a port is open,
// not that the required TLS/SNI reverse path works, and would create a
// misleading "available" result. main.go keeps those resources in the
// separate candidate catalog instead.
//
// Connectivity is checked FIRST, and geo lookup runs only on the handful
// that pass. Doing geo up-front on every candidate (often thousands) would
// blow straight through ip-api.com's ~45 req/min free-tier limit, and the
// 429 responses used to get misparsed into garbage "country" values.
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

	for _, p := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func(px Proxy) {
			defer wg.Done()
			defer func() { <-sem }()

			start := time.Now()

			if !isForwardingProtocol(px.Protocol) {
				return
			} else {
				if !checkURL(px, testURL, timeout) {
					markUnreachable(px.Key())
					return
				}
				px.LatencyMs = time.Since(start).Milliseconds()

				// Best-effort: discover the REAL exit IP (how the outside
				// world sees this proxy) and geolocate THAT, so country is
				// trustworthy. All of this is non-fatal - a node stays
				// alive even if the exit/geo probes are rate-limited; it
				// just falls back to source-supplied or front-IP geo.
				px.ExitIP = probeExitIP(px, timeout)
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

				if px.ExitIP != "" {
					if c, ci, co := LookupGeo(px.ExitIP, timeout); c != "" && c != "Unknown" {
						px.Country, px.City, px.Continent = c, ci, co
					}
				}
				if px.Country == "" {
					c, ci, co := LookupGeo(px.IP, timeout)
					px.Country = strings.TrimSpace(c)
					px.City = strings.TrimSpace(ci)
					px.Continent = strings.TrimSpace(co)
				}

				px.Anonymity = probeAnonymity(px, timeout)
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
// Any status code counts - the point is proving the full round trip
// (TCP/SOCKS5-or-CONNECT handshake, and TLS for an https:// URL) actually
// works for that target, not that the target returns success. The
// response body is never read (Close is called immediately after headers
// arrive) so this stays cheap even against a heavy page.
func checkURL(px Proxy, testURL string, timeout time.Duration) bool {
	return checkURLContext(context.Background(), px, testURL, timeout)
}

// checkURLContext is the cancellation-aware form used by request-scoped manual
// verification. Background health work keeps the historical checkURL wrapper,
// while a canceled HTTP request can now stop an in-flight dial/handshake and
// prevent later retry attempts from starting.
func checkURLContext(parent context.Context, px Proxy, testURL string, timeout time.Duration) bool {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// probeExitIP fetches the proxy's real exit IP by asking a lenient
// "what's my IP" service through the tunnel. Returns "" on any failure -
// callers treat it as best-effort and never drop a node over it.
func probeExitIP(px Proxy, timeout time.Duration) string {
	return probeExitIPContext(context.Background(), px, timeout)
}

func probeExitIPContext(parent context.Context, px Proxy, timeout time.Duration) string {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org/", nil)
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

// baselineExitIP is our own direct (non-proxied) public egress IP. A proxy
// whose measured exit IP equals this doesn't actually change your public
// IP (it's transparent, or the whole host sits behind a transparent egress
// proxy). Set once by InitBaselineExit; "" if the probe failed.
var (
	baselineExitIP string
	baselineExitMu sync.RWMutex
)

// InitBaselineExit measures our direct egress. A failed attempt is not cached,
// so periodic health work can retry after a transient DNS/network failure.
func InitBaselineExit(timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("https://api.ipify.org/")
	if err != nil {
		log.Printf("[baseline] direct egress probe failed; IP-change state is unknown and will be retried: %v", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[baseline] direct egress probe returned %s; will retry", resp.Status)
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		log.Printf("[baseline] direct egress probe returned an invalid IP; will retry")
		return false
	}
	baselineExitMu.Lock()
	changed := baselineExitIP != ip
	baselineExitIP = ip
	baselineExitMu.Unlock()
	if changed {
		log.Printf("[baseline] our direct egress IP = %s (proxies exiting from this IP are transparent)", ip)
	}
	return true
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
	baseline := BaselineExitIP()
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return DialUpstreamContext(ctx, px, addr, timeout)
			},
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Get("https://httpbin.org/get")
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

// LookupGeo queries ip-api.com for IP geolocation, returning the ISO
// country code (e.g. "US"), city, and continent code (e.g. "NA" - one of
// the standard AS/NA/EU/AF/SA/OC/AN codes, the same scheme used by the
// EDT-Pages source feeds). Only called for nodes that passed the
// connectivity check and whose source didn't already supply geo.
//
// It validates the HTTP status and the JSON "status" field, returning
// "Unknown" on any error or rate-limit (429). Returning the ISO code
// keeps country values consistent with the source feeds (EDT/ProxyIP also
// use codes), so country-filtered groups match regardless of source.
func LookupGeo(ip string, timeout time.Duration) (country, city, continent string) {
	return LookupGeoContext(context.Background(), ip, timeout)
}

func LookupGeoContext(parent context.Context, ip string, timeout time.Duration) (country, city, continent string) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	client := &http.Client{}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://ip-api.com/json/"+ip+"?fields=status,countryCode,city,continentCode", nil)
	if err != nil {
		return "Unknown", "", ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return "Unknown", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "Unknown", "", "" // typically 429 rate-limited
	}

	var r struct {
		Status        string `json:"status"`
		CountryCode   string `json:"countryCode"`
		City          string `json:"city"`
		ContinentCode string `json:"continentCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.Status != "success" || r.CountryCode == "" {
		return "Unknown", "", ""
	}
	return r.CountryCode, r.City, r.ContinentCode
}
