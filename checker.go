package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Blocked countries: China mainland + Hong Kong (can't reach Google).
// Keyed by both ISO code and full name so it works whichever a source
// supplies (LookupGeo now normalizes to ISO codes, but source feeds vary).
var blockedCountries = map[string]bool{
	"cn":        true,
	"hk":        true,
	"china":     true,
	"hong kong": true,
}

// CheckProxies concurrently verifies a list of candidate proxies.
// Forwarding-capable proxies (socks5/http/https) are verified via a real
// Google connectivity round-trip through DialUpstream, and their latency
// is recorded. "proxyip" entries don't speak a forwarding protocol (see
// parser.go), so they only get a lightweight raw TCP reachability probe.
//
// Connectivity is checked FIRST, and geo lookup runs only on the handful
// that pass. Doing geo up-front on every candidate (often thousands) would
// blow straight through ip-api.com's ~45 req/min free-tier limit, and the
// 429 responses used to get misparsed into garbage "country" values.
func CheckProxies(proxies []Proxy, timeout time.Duration, maxConcurrent int) []Proxy {
	var (
		mu    sync.Mutex
		alive []Proxy
		wg    sync.WaitGroup
		sem   = make(chan struct{}, maxConcurrent)
	)

	for _, p := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func(px Proxy) {
			defer wg.Done()
			defer func() { <-sem }()

			start := time.Now()
			var ok bool
			if px.Protocol == "proxyip" {
				ok = checkReachable(px, timeout)
			} else {
				ok = checkGoogle(px, timeout)
			}
			if !ok {
				return
			}
			px.LatencyMs = time.Since(start).Milliseconds()

			// Geo only for survivors, and only when the source didn't
			// already supply it (EDT-Pages/proxyip feeds do).
			if px.Country == "" {
				country, city := LookupGeo(px.IP, timeout)
				px.Country = strings.TrimSpace(country)
				px.City = strings.TrimSpace(city)
			}
			if blockedCountries[strings.ToLower(px.Country)] {
				return
			}

			mu.Lock()
			alive = append(alive, px)
			mu.Unlock()
		}(p)
	}

	wg.Wait()
	return alive
}

// checkGoogle verifies a forwarding-capable proxy by fetching Google's
// generate_204 endpoint through the upstream tunnel. Works uniformly for
// socks5/http/https upstreams since DialUpstream already establishes the
// tunnel to the target before this ever touches protocol-specific bytes.
func checkGoogle(px Proxy, timeout time.Duration) bool {
	conn, err := DialUpstream(px, "www.google.com:80", timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	req := "GET /generate_204 HTTP/1.1\r\nHost: www.google.com\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return false
	}

	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil || n < 4 {
		return false
	}
	return string(resp[:4]) == "HTTP"
}

// checkReachable is a minimal TCP-connect probe for "proxyip" entries,
// which don't speak SOCKS5/HTTP and so can't be verified via checkGoogle.
func checkReachable(px Proxy, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", px.Addr(), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// LookupGeo queries ip-api.com for IP geolocation, returning the ISO
// country code (e.g. "US") and city. Only called for nodes that passed
// the connectivity check and whose source didn't already supply geo.
//
// It validates the HTTP status and the JSON "status" field, returning
// "Unknown" on any error or rate-limit (429). Returning the ISO code
// keeps country values consistent with the source feeds (EDT/ProxyIP also
// use codes), so country-filtered groups match regardless of source.
func LookupGeo(ip string, timeout time.Duration) (country, city string) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://ip-api.com/json/" + ip + "?fields=status,countryCode,city")
	if err != nil {
		return "Unknown", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "Unknown", "" // typically 429 rate-limited
	}

	var r struct {
		Status      string `json:"status"`
		CountryCode string `json:"countryCode"`
		City        string `json:"city"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.Status != "success" || r.CountryCode == "" {
		return "Unknown", ""
	}
	return r.CountryCode, r.City
}
