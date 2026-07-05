package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Blocked countries: China mainland + Hong Kong (can't reach Google).
var blockedCountries = map[string]bool{
	"china":     true,
	"hong kong": true,
	"cn":        true,
	"hk":        true,
}

// CheckProxies concurrently verifies a list of candidate proxies.
// Forwarding-capable proxies (socks5/http/https) are verified via a real
// Google connectivity round-trip through DialUpstream, and their latency
// is recorded. "proxyip" entries don't speak a forwarding protocol (see
// parser.go), so they only get a lightweight raw TCP reachability probe.
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

			// Only look up geo when the source didn't already supply it
			// (EDT-Pages/proxyip feeds do; the plain-text feed doesn't) -
			// avoids hammering ip-api.com's rate limit unnecessarily.
			if px.Country == "" {
				country, city := LookupGeo(px.IP, timeout)
				px.Country = strings.TrimSpace(country)
				px.City = strings.TrimSpace(city)
			}

			if blockedCountries[strings.ToLower(px.Country)] {
				return
			}

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

// LookupGeo queries ip-api.com for IP geolocation. Only called when a
// source doesn't already supply country/city metadata.
func LookupGeo(ip string, timeout time.Duration) (country, city string) {
	conn, err := net.DialTimeout("tcp", "ip-api.com:80", timeout)
	if err != nil {
		return "Unknown", ""
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	req := fmt.Sprintf("GET /csv/%s?fields=country,city HTTP/1.1\r\nHost: ip-api.com\r\nConnection: close\r\n\r\n", ip)
	conn.Write([]byte(req))

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return "Unknown", ""
	}

	body := string(buf[:n])
	for i := 0; i < len(body)-3; i++ {
		if body[i:i+4] == "\r\n\r\n" {
			body = body[i+4:]
			break
		}
	}

	for i, c := range body {
		if c == ',' {
			return body[:i], body[i+1:]
		}
	}
	return body, ""
}
