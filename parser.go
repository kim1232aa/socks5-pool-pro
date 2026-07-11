package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// proxyURLRegex extracts URL-shaped proxy entries from free text/HTML.  The
// URL parser below performs the actual host/port validation, which means text
// feeds can contain authenticated, IPv6, or hostname-based upstreams instead
// of being restricted to bare IPv4 entries.
var proxyURLRegex = regexp.MustCompile(`(?i)(?:socks5|https?)://[^\s<>"']+`)

const maxFetchBytes = 64 << 20 // 64MB safety cap for source downloads

const (
	sourceFetchAttempts        = 3
	sourceFetchAttemptTimeout  = 12 * time.Second
	sourceFetchTotalTimeout    = 30 * time.Second
	sourceFetchRetryDelay      = 250 * time.Millisecond
	maxConcurrentSourceFetches = 4
	sourceFetchQueueTimeout    = 2 * time.Minute
)

// refreshPool intentionally starts one goroutine per configured source so a
// slow feed cannot serialize the whole refresh. Keep the actual network/body
// work bounded here: otherwise a large custom source set could retain one
// 64 MiB response per source at the same time.
var sourceFetchSlots = make(chan struct{}, maxConcurrentSourceFetches)

type sourceFetchPolicy struct {
	Attempts     int
	TotalTimeout time.Duration
	RetryDelay   time.Duration
}

var defaultSourceFetchPolicy = sourceFetchPolicy{
	Attempts:     sourceFetchAttempts,
	TotalTimeout: sourceFetchTotalTimeout,
	RetryDelay:   sourceFetchRetryDelay,
}

// FetchSource downloads and parses a single Source's node list, tagging
// every result with the source's name.
func FetchSource(src Source) ([]Proxy, error) {
	queueCtx, cancel := context.WithTimeout(context.Background(), sourceFetchQueueTimeout)
	defer cancel()
	select {
	case sourceFetchSlots <- struct{}{}:
		defer func() { <-sourceFetchSlots }()
	case <-queueCtx.Done():
		return nil, fmt.Errorf("source fetch queue is full: %w", queueCtx.Err())
	}

	client, transport, err := newSourceHTTPClient(src)
	if err != nil {
		return nil, err
	}
	defer transport.CloseIdleConnections()
	return fetchSourceWithClient(src, client, defaultSourceFetchPolicy)
}

// sourceIPResolver is the small net.Resolver surface needed by the guarded
// dialer. Keeping it injectable makes DNS-policy tests deterministic without
// weakening the production client.
type sourceIPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

func newSourceHTTPClient(src Source) (*http.Client, *http.Transport, error) {
	if _, err := validateSourceURL(src.URL, src.AllowPrivate); err != nil {
		return nil, nil, err
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           guardedSourceDialContext(net.DefaultResolver, src.AllowPrivate),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   1,
		MaxConnsPerHost:       1,
		IdleConnTimeout:       15 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: sourceFetchAttemptTimeout,
	}
	client := &http.Client{
		Timeout:       sourceFetchAttemptTimeout,
		Transport:     transport,
		CheckRedirect: sourceRedirectPolicy(src.AllowPrivate),
	}
	return client, transport, nil
}

func sourceRedirectPolicy(allowPrivate bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("source redirect limit exceeded")
		}
		if _, err := validateSourceURL(req.URL.String(), allowPrivate); err != nil {
			return fmt.Errorf("source redirect rejected: %w", err)
		}
		return nil
	}
}

// validateSourceURL performs syntax and literal-address checks both when a
// source is saved and before each fetch/redirect. Hostnames are resolved and
// checked by guardedSourceDialContext immediately before the TCP connection,
// closing the usual validation-to-dial DNS rebinding gap.
func validateSourceURL(raw string, allowPrivate bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid source url: must be a full http:// or https:// address")
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("invalid source url: fragments are not allowed")
	}
	if port := u.Port(); port != "" {
		value, portErr := strconv.ParseUint(port, 10, 16)
		if portErr != nil || value == 0 {
			return nil, fmt.Errorf("invalid source url: port must be between 1 and 65535")
		}
	}
	if ip := net.ParseIP(strings.Trim(u.Hostname(), "[]")); ip != nil && !allowPrivate && isDisallowedSourceIP(ip) {
		return nil, fmt.Errorf("source url target is private or reserved; set allow_private=true only for a trusted source")
	}
	return u, nil
}

func guardedSourceDialContext(resolver sourceIPResolver, allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: sourceFetchAttemptTimeout, KeepAlive: 15 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid source dial address: %w", err)
		}
		if allowPrivate {
			return dialer.DialContext(ctx, network, address)
		}

		resolved, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve source host %q: %w", host, err)
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("resolve source host %q: no addresses", host)
		}
		for _, candidate := range resolved {
			if candidate.Zone != "" || candidate.IP == nil || isDisallowedSourceIP(candidate.IP) {
				// Reject a mixed public/private answer as a unit. Silently selecting the
				// public member would let an attacker change which member is returned on
				// a later connection and makes policy behavior resolver-order dependent.
				return nil, fmt.Errorf("source host %q resolved to a private or reserved address", host)
			}
		}

		var dialErrors []error
		for _, candidate := range resolved {
			target := net.JoinHostPort(candidate.IP.String(), port)
			conn, dialErr := dialer.DialContext(ctx, network, target)
			if dialErr == nil {
				return conn, nil
			}
			dialErrors = append(dialErrors, dialErr)
		}
		return nil, fmt.Errorf("dial source host %q: %w", host, errors.Join(dialErrors...))
	}
}

var disallowedSourceNetworks = mustSourceNetworks(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "100::/64", "2001::/23", "2001:db8::/32",
	"fc00::/7", "fe80::/10", "ff00::/8",
)

func mustSourceNetworks(values ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic(err)
		}
		out = append(out, network)
	}
	return out
}

func isDisallowedSourceIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, network := range disallowedSourceNetworks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// fetchSourceWithClient keeps retry timing injectable for fast deterministic
// tests. Production attempts are individually bounded by the client's timeout
// and collectively bounded by policy.TotalTimeout.
func fetchSourceWithClient(src Source, client *http.Client, policy sourceFetchPolicy) ([]Proxy, error) {
	body, err := downloadSource(src, client, policy)
	if err != nil {
		return nil, err
	}

	var proxies []Proxy
	switch src.Format {
	case FormatEDTJSON:
		proxies, err = parseEDTJSON(body)
	case FormatProxyIPJSON:
		proxies, err = parseProxyIPJSON(body)
	case FormatTextRegex:
		proxies, err = parseTextRegex(body)
	case FormatPlainList:
		proxies, err = parsePlainList(body, src.Protocol)
	case FormatJSONArray:
		proxies, err = parseJSONArray(body, src.Protocol)
	default:
		return nil, fmt.Errorf("unknown source format: %q", src.Format)
	}
	if err != nil {
		return nil, err
	}

	valid := proxies[:0]
	for _, px := range proxies {
		px, ok := normalizeProxy(px)
		if !ok {
			continue
		}
		px.SourceName = src.Name
		valid = append(valid, px)
	}
	proxies = valid

	log.Printf("[fetch] %s: %d proxies from %s", safeLogLabel(src.Name), len(proxies), safeSourceURL(src.URL))
	return proxies, nil
}

func downloadSource(src Source, client *http.Client, policy sourceFetchPolicy) ([]byte, error) {
	if client == nil {
		return nil, fmt.Errorf("fetch failed: nil HTTP client")
	}
	if policy.Attempts < 1 {
		policy.Attempts = 1
	}
	if policy.TotalTimeout <= 0 {
		policy.TotalTimeout = sourceFetchTotalTimeout
	}
	if policy.RetryDelay < 0 {
		policy.RetryDelay = 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), policy.TotalTimeout)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= policy.Attempts; attempt++ {
		body, retryable, err := downloadSourceAttempt(ctx, client, src.URL)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable || attempt == policy.Attempts {
			return nil, fmt.Errorf("fetch failed after %d attempt(s): %w", attempt, err)
		}

		delay := policy.RetryDelay * time.Duration(1<<(attempt-1))
		log.Printf("[fetch] %s: attempt %d/%d failed (%v), retrying in %s", safeLogLabel(src.Name), attempt, policy.Attempts, err, delay)
		if err := waitSourceRetry(ctx, delay); err != nil {
			return nil, fmt.Errorf("fetch failed after %d attempt(s): %w", attempt, errors.Join(lastErr, err))
		}
	}
	return nil, fmt.Errorf("fetch failed: %w", lastErr)
}

func downloadSourceAttempt(ctx context.Context, client *http.Client, sourceURL string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, false, safeSourceURLError("create source request", sourceURL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, isRetryableNetworkError(err), safeSourceURLError("fetch source", sourceURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		// Drain a small error page so keep-alive connections remain reusable,
		// but never let a source return an unbounded error response.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		_ = resp.Body.Close()
		err := fmt.Errorf("unexpected status: %d", resp.StatusCode)
		return nil, isRetryableSourceStatus(resp.StatusCode), err
	}

	// Reading limit+1 makes oversize detection unambiguous without parsing a
	// truncated response. The body is closed before every return and before a
	// possible retry starts.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, isRetryableNetworkError(readErr), safeSourceURLError("read source response", sourceURL, readErr)
	}
	if len(body) > maxFetchBytes {
		return nil, false, fmt.Errorf("source response exceeds %d byte limit", maxFetchBytes)
	}
	if closeErr != nil {
		return nil, isRetryableNetworkError(closeErr), safeSourceURLError("close source response", sourceURL, closeErr)
	}
	return body, false, nil
}

func isRetryableSourceStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func isRetryableNetworkError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}

func waitSourceRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// normalizeProxy makes source parsing strict before a candidate consumes a
// bounded health-check slot.  Public feeds regularly contain values such as
// 999.999.999.999:99999 or protocol labels unsupported by this service; if
// those entries enter the sampled set they crowd out real nodes.  Hostnames
// remain supported for custom sources, while ports and protocol labels are
// normalized to the exact forms understood by DialUpstream.
func normalizeProxy(px Proxy) (Proxy, bool) {
	px.IP = strings.TrimSpace(px.IP)
	px.Port = strings.TrimSpace(px.Port)
	px.Protocol = strings.ToLower(strings.TrimSpace(px.Protocol))
	if px.IP == "" || px.Port == "" || !isProxyProtocol(px.Protocol) {
		return Proxy{}, false
	}
	port, err := strconv.ParseUint(px.Port, 10, 16)
	if err != nil || port == 0 || port > 65535 {
		return Proxy{}, false
	}
	px.Port = strconv.FormatUint(port, 10)
	if ip := net.ParseIP(px.IP); ip != nil {
		// Canonicalise IP spellings so equivalent IPv6/IPv4 representations
		// share the same protocol-aware key during deduplication.
		px.IP = ip.String()
	} else {
		px.IP = strings.ToLower(strings.TrimSuffix(px.IP, "."))
		if looksLikeIPv4Literal(px.IP) || !validProxyHostname(px.IP) {
			return Proxy{}, false
		}
	}
	return px, true
}

func isProxyProtocol(protocol string) bool {
	switch protocol {
	case "socks5", "http", "https", "proxyip":
		return true
	default:
		return false
	}
}

func isForwardingProtocol(protocol string) bool {
	return protocol == "socks5" || protocol == "http" || protocol == "https"
}

func looksLikeIPv4Literal(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != net.IPv4len {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// validProxyHostname accepts normal DNS names without attempting resolution.
// This keeps custom domain-based upstreams working while rejecting whitespace,
// URL fragments, and malformed labels before they reach net.Dial.
func validProxyHostname(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// parseTextRegex extracts "scheme://ip:port" occurrences from a plain text
// or HTML page (e.g. socks5-proxy.github.io).
func parseTextRegex(body []byte) ([]Proxy, error) {
	matches := proxyURLRegex.FindAllString(string(body), -1)
	seen := make(map[string]bool)
	var proxies []Proxy

	for _, raw := range matches {
		// Markdown/HTML prose often attaches punctuation right after a URL.
		// Strip only common trailing delimiters; credentials and IPv6 brackets
		// inside the URL are left intact for parseProxyURL.
		raw = strings.TrimRight(raw, ".,;:!?)]}>")
		px, err := parseProxyURL(raw)
		if err != nil {
			continue
		}
		key := px.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		proxies = append(proxies, px)
	}
	return proxies, nil
}

var ipPortRegex = regexp.MustCompile(`(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}):(\d+)`)

// parsePlainList parses newline-separated "ip:port" entries (no scheme),
// e.g. monosans/proxy-list's proxies/socks5.txt. protocol tags every
// resulting entry since the file itself doesn't encode one.
func parsePlainList(body []byte, protocol string) ([]Proxy, error) {
	if protocol == "" {
		return nil, fmt.Errorf("plain-list source requires a protocol")
	}
	return extractIPPortEntries(string(body), protocol), nil
}

// parseJSONArray parses a JSON array of "ip:port" strings, e.g.
// fyvri/fresh-proxy-list's classic/socks5.json. protocol tags every
// resulting entry since the file itself doesn't encode one.
func parseJSONArray(body []byte, protocol string) ([]Proxy, error) {
	if protocol == "" {
		return nil, fmt.Errorf("json-array source requires a protocol")
	}
	var entries []string
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("parse json array: %w", err)
	}
	return extractIPPortEntries(strings.Join(entries, "\n"), protocol), nil
}

func extractIPPortEntries(text string, protocol string) []Proxy {
	matches := ipPortRegex.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool)
	var proxies []Proxy
	for _, m := range matches {
		px := Proxy{IP: m[1], Port: m[2], Protocol: protocol}
		key := px.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		proxies = append(proxies, px)
	}
	return proxies
}

// edtEntry mirrors one element of the EDT-Pages/Proxy-List JSON feeds
// (data/socks5.json, data/http.json, data/https.json).
type edtEntry struct {
	Proxy     string `json:"proxy"`
	Protocol  string `json:"protocol"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	Country   string `json:"country"`
	City      string `json:"city"`
	Continent string `json:"continent"`
}

func parseEDTJSON(body []byte) ([]Proxy, error) {
	var entries []edtEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("parse EDT json: %w", err)
	}

	seen := make(map[string]bool)
	var proxies []Proxy

	for _, e := range entries {
		px := Proxy{}
		if e.Proxy != "" {
			if parsed, err := parseProxyURL(e.Proxy); err == nil {
				px = parsed
			}
		}
		if px.IP == "" {
			px.IP = e.IP
			if e.Port != 0 {
				px.Port = strconv.Itoa(e.Port)
			}
		}
		if px.Protocol == "" {
			px.Protocol = strings.ToLower(e.Protocol)
		}
		if px.IP == "" || px.Port == "" {
			continue
		}
		px.Country = e.Country
		px.City = e.City
		px.Continent = e.Continent

		key := px.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		proxies = append(proxies, px)
	}
	return proxies, nil
}

// proxyIPFile mirrors the shape of https://zip.cm.edu.kg/all.json: a list of
// external reverse-proxy/jump endpoints used by Cloudflare Worker/VLESS style
// tools as their "ProxyIP" parameter. These do NOT speak SOCKS5/HTTP proxy
// protocol themselves, so entries parsed here are tagged Protocol="proxyip"
// and are never selected as a forwarding upstream (see pool.go / server.go).
type proxyIPFile struct {
	Data []proxyIPEntry `json:"data"`
}

type proxyIPEntry struct {
	IP   string       `json:"ip"`
	Port proxyIPPorts `json:"port"`
	Meta struct {
		Country   string `json:"country"`
		City      string `json:"city"`
		Continent string `json:"continent"`
	} `json:"meta"`
}

// The upstream admin accepts both the current array form (`"port":[443]`)
// and older/scalar records (`"port":443`). Keep the parser equally tolerant
// so a harmless source-shape variation cannot empty the ProxyIP catalog.
type proxyIPPorts []int

func (p *proxyIPPorts) UnmarshalJSON(data []byte) error {
	var many []int
	if err := json.Unmarshal(data, &many); err == nil {
		*p = many
		return nil
	}
	var one int
	if err := json.Unmarshal(data, &one); err != nil {
		return fmt.Errorf("proxyip port must be an integer or integer array: %w", err)
	}
	*p = []int{one}
	return nil
}

func parseProxyIPJSON(body []byte) ([]Proxy, error) {
	var f proxyIPFile
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("parse proxyip json: %w", err)
	}

	seen := make(map[string]bool)
	var proxies []Proxy

	for _, e := range f.Data {
		if e.IP == "" || !containsPort(e.Port, 443) {
			continue
		}
		// EdgeTunnel-style ProxyIP consumers use these Cloudflare reverse
		// endpoints on port 443 and select the IP itself. Emitting every port
		// advertised for the same address inflated country totals and made the
		// catalog diverge from the upstream resource browser.
		px := Proxy{
			IP:        e.IP,
			Port:      strconv.Itoa(443),
			Protocol:  "proxyip",
			Country:   e.Meta.Country,
			City:      e.Meta.City,
			Continent: e.Meta.Continent,
		}
		key := px.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		proxies = append(proxies, px)
	}
	return proxies, nil
}

func containsPort(ports []int, want int) bool {
	for _, port := range ports {
		if port == want {
			return true
		}
	}
	return false
}
