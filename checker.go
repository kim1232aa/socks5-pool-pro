package main

import (
	"context"
	"encoding/json"
	"io"
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

			if px.Protocol == "proxyip" {
				// proxyip nodes don't forward, so there's no "exit" to
				// probe; just prove reachability and trust source geo.
				if !checkReachable(px, timeout) {
					return
				}
				px.LatencyMs = time.Since(start).Milliseconds()
			} else {
				// Connectivity check uses a rate-limit-free endpoint so
				// aliveness never depends on the geo service being up.
				if !checkGoogle(px, timeout) {
					return
				}
				px.LatencyMs = time.Since(start).Milliseconds()

				// Best-effort: discover the REAL exit IP (how the outside
				// world sees this proxy) and geolocate THAT, so country is
				// trustworthy. All of this is non-fatal - a node stays
				// alive even if the exit/geo probes are rate-limited; it
				// just falls back to source-supplied or front-IP geo.
				px.ExitIP = probeExitIP(px, timeout)
				if px.ExitIP != "" {
					if c, ci := LookupGeo(px.ExitIP, timeout); c != "" && c != "Unknown" {
						px.Country, px.City = c, ci
					}
				}
				if px.Country == "" {
					c, ci := LookupGeo(px.IP, timeout)
					px.Country = strings.TrimSpace(c)
					px.City = strings.TrimSpace(ci)
				}
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
// generate_204 endpoint through the upstream tunnel. Google doesn't rate-
// limit this, so it's a reliable aliveness signal (unlike a geo service).
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
