package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

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

const csrfCookieName = "socks5_pool_csrf"

// protectStateChangingRequests requires a double-submit token for browser
// writes while retaining compatibility with curl and service clients that send
// neither browser fetch metadata nor an Origin header.
func protectStateChangingRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		fetchSite := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))
		if strings.EqualFold(fetchSite, "cross-site") {
			writeErrCode(w, http.StatusForbidden, "cross_site_request", fmt.Errorf("cross-site state-changing request rejected"))
			return
		}
		rawOrigin := strings.TrimSpace(r.Header.Get("Origin"))
		if rawOrigin != "" {
			origin, err := url.Parse(rawOrigin)
			scheme := strings.ToLower(origin.Scheme)
			if err != nil || (scheme != "http" && scheme != "https") || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || !sameOriginAuthority(origin, r.Host) {
				writeErrCode(w, http.StatusForbidden, "origin_mismatch", fmt.Errorf("request Origin does not match Host"))
				return
			}
		}
		if rawOrigin != "" || fetchSite != "" {
			cookie, err := r.Cookie(csrfCookieName)
			token := r.Header.Get("X-CSRF-Token")
			if err != nil || token == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 {
				writeErrCode(w, http.StatusForbidden, "invalid_csrf_token", fmt.Errorf("missing or invalid CSRF token"))
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

func newCSRFToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate CSRF token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}
