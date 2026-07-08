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
// "proxyip" entries don't speak a forwarding protocol (see parser.go), so
// they only get a lightweight raw TCP reachability probe instead.
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
// The second return value, unreachable, is the set of addresses (Proxy.Addr())
// that were actually dialed and genuinely failed to connect - as opposed to
// ones that connected fine but got excluded from alive for a policy reason
// (a transparent proxy dropped by requireIPChange). Callers use this
// distinction to avoid marking a perfectly reachable, just policy-filtered
// node as "unavailable" (see ProxyPool.Update).
func CheckProxies(proxies []Proxy, timeout time.Duration, maxConcurrent int, requireIPChange bool, testURL string) (alive []Proxy, unreachable map[string]bool) {
	var (
		mu      sync.Mutex
		dropped int // transparent proxies filtered by requireIPChange
		wg      sync.WaitGroup
		sem     = make(chan struct{}, maxConcurrent)
	)
	unreachable = make(map[string]bool, len(proxies))
	markUnreachable := func(addr string) {
		mu.Lock()
		unreachable[addr] = true
		mu.Unlock()
	}

	for _, p := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func(px Proxy) {
			defer wg.Done()
			defer func() { <-sem }()

			start := time.Now()

			if px.Protocol == "proxyip" {
				// proxyip nodes don't forward, so there's no "exit" to
				// probe; just prove reachability and trust source geo.
				if !checkReachable(px, timeout) {
					markUnreachable(px.Addr())
					return
				}
				px.LatencyMs = time.Since(start).Milliseconds()
			} else {
				if !checkURL(px, testURL, timeout) {
					markUnreachable(px.Addr())
					return
				}
				px.LatencyMs = time.Since(start).Milliseconds()

				// Best-effort: discover the REAL exit IP (how the outside
				// world sees this proxy) and geolocate THAT, so country is
				// trustworthy. All of this is non-fatal - a node stays
				// alive even if the exit/geo probes are rate-limited; it
				// just falls back to source-supplied or front-IP geo.
				px.ExitIP = probeExitIP(px, timeout)
				px.IPChanged = px.ExitIP != "" && baselineExitIP != "" && px.ExitIP != baselineExitIP

				// Drop transparent proxies that don't actually change the
				// exit IP - but only when we can positively tell (we have
				// both a baseline and a measured exit that match). Unknown
				// exits are kept rather than falsely dropped. This is a
				// policy exclusion, not a connectivity failure - the node
				// genuinely answered - so it does NOT go into unreachable.
				if requireIPChange && baselineExitIP != "" && px.ExitIP != "" && !px.IPChanged {
					mu.Lock()
					dropped++
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
		log.Printf("[checker] dropped %d transparent proxies (exit IP == our own egress %s; disable with -require-ip-change=false)", dropped, baselineExitIP)
	}
	return alive, unreachable
}

// checkURL verifies a forwarding-capable proxy by fetching testURL through
// the upstream tunnel and checking that a real HTTP response comes back.
// Any status code counts - the point is proving the full round trip
// (TCP/SOCKS5-or-CONNECT handshake, and TLS for an https:// URL) actually
// works for that target, not that the target returns success. The
// response body is never read (Close is called immediately after headers
// arrive) so this stays cheap even against a heavy page.
func checkURL(px Proxy, testURL string, timeout time.Duration) bool {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
				return DialUpstream(px, addr, timeout)
			},
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Get(testURL)
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
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
				return DialUpstream(px, addr, timeout)
			},
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Get("http://api.ipify.org/")
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
	baselineExitIP   string
	baselineExitOnce sync.Once
)

// InitBaselineExit measures our own direct egress IP once at startup.
func InitBaselineExit(timeout time.Duration) {
	baselineExitOnce.Do(func() {
		client := &http.Client{Timeout: timeout}
		resp, err := client.Get("http://api.ipify.org/")
		if err != nil {
			log.Printf("[baseline] direct egress probe failed (transparent-proxy filter disabled): %v", err)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		ip := strings.TrimSpace(string(body))
		if net.ParseIP(ip) != nil {
			baselineExitIP = ip
			log.Printf("[baseline] our direct egress IP = %s (proxies exiting from this IP are transparent)", ip)
		}
	})
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
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
				return DialUpstream(px, addr, timeout)
			},
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Get("http://httpbin.org/get")
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

	leak := baselineExitIP != "" && strings.Contains(r.Origin, baselineExitIP)
	proxyHdr := false
	for _, h := range proxyLeakHeaders {
		if v, ok := r.Headers[h]; ok {
			proxyHdr = true
			if baselineExitIP != "" && strings.Contains(v, baselineExitIP) {
				leak = true
			}
		}
	}
	switch {
	case leak:
		return "transparent"
	case proxyHdr:
		return "anonymous"
	default:
		return "elite"
	}
}

// checkReachable is a minimal TCP-connect probe for "proxyip" entries,
// which don't speak SOCKS5/HTTP and so can't be verified via checkURL.
func checkReachable(px Proxy, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", px.Addr(), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
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
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://ip-api.com/json/" + ip + "?fields=status,countryCode,city,continentCode")
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
