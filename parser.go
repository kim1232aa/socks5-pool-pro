package main

import (
	"bufio"
	"bytes"
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

const maxFetchBytes = 16 << 20 // 16MB cap; bounded for low-memory hosts while covering the largest known feed

const (
	// The largest bundled feed currently contains roughly 230k records. Keep
	// enough headroom for normal growth while bounding the maps/slices produced
	// from a compact JSON response.
	maxSourceParsedRecords   = 300_000
	maxSourceProxyURLBytes   = 16 << 10
	maxSourceAddressBytes    = 512
	maxSourcePortBytes       = 16
	maxSourceCredentialBytes = 255
	maxSourceCountryBytes    = 64
	maxSourceCityBytes       = 512
	maxSourceContinentBytes  = 16
	maxProxyIPPortsPerRecord = 64
)

var (
	// ErrSourceEmpty distinguishes an unexpectedly empty/invalid 200 response
	// from a legitimate authoritative empty feed (Source.AllowEmpty).
	ErrSourceEmpty          = errors.New("source returned no valid proxy records")
	ErrSourceBudgetExceeded = errors.New("source parsing budget exceeded")
)

const (
	sourceFetchAttempts = 3
	// The bundled Fyvri archives are several MiB and can take 30-40 seconds to
	// stream from their upstream host even after headers arrive. Bound every
	// attempt, but leave enough body-read time for those known-good feeds.
	sourceFetchAttemptTimeout  = 45 * time.Second
	sourceFetchTotalTimeout    = 140 * time.Second
	sourceFetchRetryDelay      = 250 * time.Millisecond
	maxConcurrentSourceFetches = 4
	sourceFetchQueueTimeout    = 5 * time.Minute
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

// nonPublicInternetNetworks is shared by source-URL SSRF checks and fetched
// proxy endpoint validation. net.IP.IsGlobalUnicast intentionally includes
// several special-use ranges (for example documentation and benchmarking
// networks), so keep those ranges explicit here instead of treating
// IsGlobalUnicast as equivalent to "usable on the public Internet".
var nonPublicInternetNetworks = mustSourceNetworks(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16",
	"192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
	"2001::/23", "2001:db8::/32", "2002::/16", "2620:4f:8000::/48",
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
	return !isPublicInternetIP(ip)
}

func isPublicInternetIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, network := range nonPublicInternetNetworks {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

// fetchSourceWithClient keeps retry timing injectable for fast deterministic
// tests. Production attempts are individually bounded by the client's timeout
// and collectively bounded by policy.TotalTimeout.
func fetchSourceWithClient(src Source, client *http.Client, policy sourceFetchPolicy) ([]Proxy, error) {
	if isJSONSourceFormat(src.Format) {
		proxies, err := downloadAndParseJSONSource(src, client, policy)
		if err != nil {
			return nil, err
		}
		return finalizeFetchedProxies(src, proxies)
	}

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

	return finalizeFetchedProxies(src, proxies)
}

func isJSONSourceFormat(format string) bool {
	switch format {
	case FormatEDTJSON, FormatProxyIPJSON, FormatJSONArray:
		return true
	default:
		return false
	}
}

func finalizeFetchedProxies(src Source, proxies []Proxy) ([]Proxy, error) {
	valid := proxies[:0]
	for _, px := range proxies {
		if err := validateFetchedProxyFields(px); err != nil {
			return nil, err
		}
		px, ok := normalizeFetchedProxy(px)
		if !ok {
			continue
		}
		px.SourceName = src.Name
		if err := validateFetchedProxyFields(px); err != nil {
			return nil, err
		}
		valid = append(valid, px)
	}
	proxies = valid
	if len(proxies) == 0 && !src.AllowEmpty {
		return nil, fmt.Errorf("%w: %s", ErrSourceEmpty, safeLogLabel(src.Name))
	}

	log.Printf("[fetch] %s: %d proxies from %s", safeLogLabel(src.Name), len(proxies), safeSourceURL(src.URL))
	return proxies, nil
}

func downloadAndParseJSONSource(src Source, client *http.Client, policy sourceFetchPolicy) ([]Proxy, error) {
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
		proxies, retryable, err := downloadAndParseJSONSourceAttempt(ctx, src, client)
		if err == nil {
			return proxies, nil
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

type sourceStreamReader struct {
	reader  io.Reader
	readErr error
}

func (r *sourceStreamReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.readErr = err
	}
	return n, err
}

func downloadAndParseJSONSourceAttempt(ctx context.Context, src Source, client *http.Client) ([]Proxy, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return nil, false, safeSourceURLError("create source request", src.URL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, isRetryableNetworkError(err), safeSourceURLError("fetch source", src.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		_ = resp.Body.Close()
		err := fmt.Errorf("unexpected status: %d", resp.StatusCode)
		return nil, isRetryableSourceStatus(resp.StatusCode), err
	}

	limited := &io.LimitedReader{R: resp.Body, N: maxFetchBytes + 1}
	reader := &sourceStreamReader{reader: limited}
	var proxies []Proxy
	switch src.Format {
	case FormatEDTJSON:
		proxies, err = parseEDTJSONReader(reader)
	case FormatProxyIPJSON:
		proxies, err = parseProxyIPJSONReader(reader)
	case FormatJSONArray:
		proxies, err = parseJSONArrayReader(reader, src.Protocol)
	default:
		err = fmt.Errorf("unknown source format: %q", src.Format)
	}
	var discard [4 << 10]byte
	_, drainErr := io.CopyBuffer(io.Discard, reader, discard[:])
	closeErr := resp.Body.Close()
	if limited.N == 0 {
		return nil, false, fmt.Errorf("source response exceeds %d byte limit", maxFetchBytes)
	}
	if drainErr != nil {
		return nil, isRetryableNetworkError(drainErr), safeSourceURLError("read source response", src.URL, drainErr)
	}
	if reader.readErr != nil {
		return nil, isRetryableNetworkError(reader.readErr), safeSourceURLError("read source response", src.URL, reader.readErr)
	}
	if closeErr != nil {
		return nil, isRetryableNetworkError(closeErr), safeSourceURLError("close source response", src.URL, closeErr)
	}
	if err != nil {
		return nil, false, err
	}
	return proxies, false, nil
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
	// Archive hosts occasionally return a transient 404 while an updated object
	// propagates. It is still bounded by sourceFetchAttempts, so truly
	// missing feeds fail after the same finite three attempts.
	return status == http.StatusNotFound || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
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
		if px.Protocol == "proxyip" && (!isPublicInternetIP(ip) || px.Port != "443") {
			return Proxy{}, false
		}
		// Canonicalise IP spellings so equivalent IPv6/IPv4 representations
		// share the same protocol-aware key during deduplication.
		px.IP = ip.String()
	} else {
		// Cloudflare ProxyIP resources are deliberately narrower than ordinary
		// forwarding candidates: they must remain literal public IPs on 443.
		if px.Protocol == "proxyip" {
			return Proxy{}, false
		}
		px.IP = strings.ToLower(strings.TrimSuffix(px.IP, "."))
		if looksLikeIPv4Literal(px.IP) || !validProxyHostname(px.IP) {
			return Proxy{}, false
		}
	}
	return px, true
}

// normalizeFetchedProxy applies the public-feed trust boundary after syntax
// normalization. Hostname upstreams remain supported without a DNS lookup,
// while literal IPs from a feed must be publicly routable. Source.AllowPrivate
// applies only to the URL used to download a trusted LAN feed; it must not let
// that feed populate the candidate catalog with private or special-use IPs.
func normalizeFetchedProxy(px Proxy) (Proxy, bool) {
	px, ok := normalizeProxy(px)
	if !ok {
		return Proxy{}, false
	}
	if ip := net.ParseIP(px.IP); ip != nil && !isPublicInternetIP(ip) {
		return Proxy{}, false
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
	matches := proxyURLRegex.FindAllIndex(body, maxSourceParsedRecords+1)
	if len(matches) > maxSourceParsedRecords {
		return nil, fmt.Errorf("%w: text source has more than %d records", ErrSourceBudgetExceeded, maxSourceParsedRecords)
	}
	seen := make(map[string]bool, len(matches))
	proxies := make([]Proxy, 0, len(matches))

	for _, match := range matches {
		if match[1]-match[0] > maxSourceProxyURLBytes {
			return nil, sourceFieldBudgetError("proxy URL", match[1]-match[0], maxSourceProxyURLBytes)
		}
		raw := string(body[match[0]:match[1]])
		// Markdown/HTML prose often attaches punctuation right after a URL.
		// Strip only common trailing delimiters; credentials and IPv6 brackets
		// inside the URL are left intact for parseProxyURL.
		raw = strings.TrimRight(raw, ".,;:!?)]}>")
		px, err := parseProxyURL(raw)
		if err != nil {
			continue
		}
		if err := validateFetchedProxyFields(px); err != nil {
			return nil, err
		}
		key := sourceProxyDedupKey(px)
		if seen[key] {
			continue
		}
		seen[key] = true
		proxies = append(proxies, px)
	}
	return proxies, nil
}

// parsePlainList parses newline-separated "ip:port" entries (no scheme),
// e.g. monosans/proxy-list's proxies/socks5.txt. protocol tags every
// resulting entry since the file itself doesn't encode one.
func parsePlainList(body []byte, protocol string) ([]Proxy, error) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if !isForwardingProtocol(protocol) {
		return nil, fmt.Errorf("plain-list source requires a protocol")
	}
	seen := make(map[string]bool)
	proxies := make([]Proxy, 0)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), maxSourceProxyURLBytes+1)
	records := 0
	for scanner.Scan() {
		entry := strings.TrimSpace(scanner.Text())
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		records++
		if records > maxSourceParsedRecords {
			return nil, fmt.Errorf("%w: plain-list source has more than %d records", ErrSourceBudgetExceeded, maxSourceParsedRecords)
		}
		px, ok, err := parseBareProxyAddress(entry, protocol)
		if err != nil {
			return nil, err
		}
		key := sourceProxyDedupKey(px)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		proxies = append(proxies, px)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: plain-list record exceeds %d bytes: %v", ErrSourceBudgetExceeded, maxSourceProxyURLBytes, err)
	}
	return proxies, nil
}

// parseJSONArray parses a JSON array of "ip:port" strings, e.g.
// fyvri/fresh-proxy-list's classic/socks5.json. protocol tags every
// resulting entry since the file itself doesn't encode one.
func parseJSONArray(body []byte, protocol string) ([]Proxy, error) {
	return parseJSONArrayReader(bytes.NewReader(body), protocol)
}

func parseJSONArrayReader(reader io.Reader, protocol string) ([]Proxy, error) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if !isForwardingProtocol(protocol) {
		return nil, fmt.Errorf("json-array source requires a protocol")
	}
	decoder := json.NewDecoder(reader)
	opening, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("parse json array: %w", err)
	}
	if opening == nil {
		if err := requireJSONEOF(decoder); err != nil {
			return nil, fmt.Errorf("parse json array: %w", err)
		}
		return nil, nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '[' {
		return nil, fmt.Errorf("parse json array: expected an array")
	}
	seen := make(map[string]bool)
	proxies := make([]Proxy, 0)
	records := 0
	for decoder.More() {
		records++
		if records > maxSourceParsedRecords {
			return nil, fmt.Errorf("%w: json-array source has more than %d records", ErrSourceBudgetExceeded, maxSourceParsedRecords)
		}
		var entry string
		if err := decoder.Decode(&entry); err != nil {
			return nil, fmt.Errorf("parse json array record %d: %w", records, err)
		}
		if len(entry) > maxSourceProxyURLBytes {
			return nil, sourceFieldBudgetError("json-array address", len(entry), maxSourceProxyURLBytes)
		}
		px, ok, err := parseBareProxyAddress(entry, protocol)
		if err != nil {
			return nil, err
		}
		key := sourceProxyDedupKey(px)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		proxies = append(proxies, px)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("parse json array closing delimiter: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("parse json array: %w", err)
	}
	return proxies, nil
}

func parseBareProxyAddress(entry, protocol string) (Proxy, bool, error) {
	entry = strings.TrimSpace(entry)
	if len(entry) > maxSourceProxyURLBytes {
		return Proxy{}, false, sourceFieldBudgetError("proxy address", len(entry), maxSourceProxyURLBytes)
	}
	host, port, err := net.SplitHostPort(entry)
	if err != nil || host == "" || port == "" {
		return Proxy{}, false, nil
	}
	px := Proxy{IP: host, Port: port, Protocol: protocol}
	if err := validateFetchedProxyFields(px); err != nil {
		return Proxy{}, false, err
	}
	return px, true, nil
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
	return parseEDTJSONReader(bytes.NewReader(body))
}

func parseEDTJSONReader(reader io.Reader) ([]Proxy, error) {
	decoder := json.NewDecoder(reader)
	opening, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("parse EDT json: %w", err)
	}
	if opening == nil {
		if err := requireJSONEOF(decoder); err != nil {
			return nil, fmt.Errorf("parse EDT json: %w", err)
		}
		return nil, nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '[' {
		return nil, fmt.Errorf("parse EDT json: expected an array")
	}
	seen := make(map[string]bool)
	proxies := make([]Proxy, 0)

	records := 0
	for decoder.More() {
		records++
		if records > maxSourceParsedRecords {
			return nil, fmt.Errorf("%w: EDT source has more than %d records", ErrSourceBudgetExceeded, maxSourceParsedRecords)
		}
		var e edtEntry
		if err := decoder.Decode(&e); err != nil {
			return nil, fmt.Errorf("parse EDT json record %d: %w", records, err)
		}
		if len(e.Proxy) > maxSourceProxyURLBytes {
			return nil, sourceFieldBudgetError("EDT proxy URL", len(e.Proxy), maxSourceProxyURLBytes)
		}
		if len(e.IP) > maxSourceAddressBytes {
			return nil, sourceFieldBudgetError("EDT address", len(e.IP), maxSourceAddressBytes)
		}
		if len(e.Country) > maxSourceCountryBytes {
			return nil, sourceFieldBudgetError("EDT country", len(e.Country), maxSourceCountryBytes)
		}
		if len(e.City) > maxSourceCityBytes {
			return nil, sourceFieldBudgetError("EDT city", len(e.City), maxSourceCityBytes)
		}
		if len(e.Continent) > maxSourceContinentBytes {
			return nil, sourceFieldBudgetError("EDT continent", len(e.Continent), maxSourceContinentBytes)
		}
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
		if err := validateFetchedProxyFields(px); err != nil {
			return nil, err
		}

		key := sourceProxyDedupKey(px)
		if seen[key] {
			continue
		}
		seen[key] = true
		proxies = append(proxies, px)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("parse EDT json closing delimiter: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("parse EDT json: %w", err)
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
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("proxyip port must be an integer or integer array: %w", err)
	}
	if first == nil {
		if err := requireJSONEOF(decoder); err != nil {
			return err
		}
		*p = nil
		return nil
	}
	if delimiter, ok := first.(json.Delim); ok {
		if delimiter != '[' {
			return fmt.Errorf("proxyip port must be an integer or integer array")
		}
		ports := make(proxyIPPorts, 0, 4)
		for decoder.More() {
			if len(ports) >= maxProxyIPPortsPerRecord {
				return fmt.Errorf("%w: proxyip record has more than %d ports", ErrSourceBudgetExceeded, maxProxyIPPortsPerRecord)
			}
			var value int
			if err := decoder.Decode(&value); err != nil {
				return fmt.Errorf("proxyip port array: %w", err)
			}
			ports = append(ports, value)
		}
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("proxyip port array: %w", err)
		}
		if err := requireJSONEOF(decoder); err != nil {
			return err
		}
		*p = ports
		return nil
	}
	number, ok := first.(json.Number)
	if !ok {
		return fmt.Errorf("proxyip port must be an integer or integer array")
	}
	value, err := strconv.Atoi(number.String())
	if err != nil {
		return fmt.Errorf("proxyip port must be an integer or integer array: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	*p = []int{value}
	return nil
}

func parseProxyIPJSON(body []byte) ([]Proxy, error) {
	return parseProxyIPJSONReader(bytes.NewReader(body))
}

func parseProxyIPJSONReader(reader io.Reader) ([]Proxy, error) {
	decoder := json.NewDecoder(reader)
	opening, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("parse proxyip json: %w", err)
	}
	if opening == nil {
		if err := requireJSONEOF(decoder); err != nil {
			return nil, fmt.Errorf("parse proxyip json: %w", err)
		}
		return nil, nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("parse proxyip json: expected an object")
	}
	seen := make(map[string]bool)
	proxies := make([]Proxy, 0)
	foundData := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("parse proxyip json field: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("parse proxyip json: object key is not a string")
		}
		if key != "data" {
			if err := skipJSONValue(decoder); err != nil {
				return nil, fmt.Errorf("parse proxyip json field %q: %w", key, err)
			}
			continue
		}
		if foundData {
			return nil, fmt.Errorf("parse proxyip json: duplicate data field")
		}
		foundData = true
		dataOpening, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("parse proxyip data: %w", err)
		}
		if dataOpening == nil {
			continue
		}
		if delimiter, ok := dataOpening.(json.Delim); !ok || delimiter != '[' {
			return nil, fmt.Errorf("parse proxyip data: expected an array")
		}
		records := 0
		for decoder.More() {
			records++
			if records > maxSourceParsedRecords {
				return nil, fmt.Errorf("%w: proxyip source has more than %d records", ErrSourceBudgetExceeded, maxSourceParsedRecords)
			}
			var e proxyIPEntry
			if err := decoder.Decode(&e); err != nil {
				return nil, fmt.Errorf("parse proxyip record %d: %w", records, err)
			}
			literalIP := net.ParseIP(strings.TrimSpace(e.IP))
			if literalIP == nil || !isPublicInternetIP(literalIP) || !containsPort(e.Port, 443) {
				continue
			}
			// EdgeTunnel-style ProxyIP consumers use these Cloudflare reverse
			// endpoints on port 443 and select the IP itself.
			px := Proxy{
				IP:        literalIP.String(),
				Port:      strconv.Itoa(443),
				Protocol:  "proxyip",
				Country:   e.Meta.Country,
				City:      e.Meta.City,
				Continent: e.Meta.Continent,
			}
			if err := validateFetchedProxyFields(px); err != nil {
				return nil, err
			}
			key := sourceProxyDedupKey(px)
			if seen[key] {
				continue
			}
			seen[key] = true
			proxies = append(proxies, px)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, fmt.Errorf("parse proxyip data closing delimiter: %w", err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("parse proxyip json closing delimiter: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("parse proxyip json: %w", err)
	}
	return proxies, nil
}

func validateFetchedProxyFields(px Proxy) error {
	fields := []struct {
		name  string
		value string
		limit int
	}{
		{"proxy address", px.IP, maxSourceAddressBytes},
		{"proxy port", px.Port, maxSourcePortBytes},
		{"proxy protocol", px.Protocol, 32},
		{"proxy username", px.Username, maxSourceCredentialBytes},
		{"proxy password", px.Password, maxSourceCredentialBytes},
		{"proxy country", px.Country, maxSourceCountryBytes},
		{"proxy city", px.City, maxSourceCityBytes},
		{"proxy continent", px.Continent, maxSourceContinentBytes},
		{"proxy source name", px.SourceName, maxSourceNameBytes},
	}
	for _, field := range fields {
		if len(field.value) > field.limit {
			return sourceFieldBudgetError(field.name, len(field.value), field.limit)
		}
	}
	if len(px.SourceNames) > maxConfiguredSources {
		return fmt.Errorf("%w: proxy has more than %d source names", ErrSourceBudgetExceeded, maxConfiguredSources)
	}
	for _, sourceName := range px.SourceNames {
		if len(sourceName) > maxSourceNameBytes {
			return sourceFieldBudgetError("proxy source name", len(sourceName), maxSourceNameBytes)
		}
	}
	if len(px.CredentialAlternates) > maxCredentialAlternates {
		return fmt.Errorf("%w: proxy has more than %d credential alternates", ErrSourceBudgetExceeded, maxCredentialAlternates)
	}
	for _, credential := range px.CredentialAlternates {
		if len(credential.Username) > maxSourceCredentialBytes {
			return sourceFieldBudgetError("alternate proxy username", len(credential.Username), maxSourceCredentialBytes)
		}
		if len(credential.Password) > maxSourceCredentialBytes {
			return sourceFieldBudgetError("alternate proxy password", len(credential.Password), maxSourceCredentialBytes)
		}
	}
	return nil
}

func sourceFieldBudgetError(field string, size, limit int) error {
	return fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrSourceBudgetExceeded, field, size, limit)
}

// sourceProxyDedupKey intentionally includes credentials. Two records can
// advertise the same protocol/address with different authentication and the
// checker must get a chance to try each variant before the refresh pipeline
// merges them into one routable node.
func sourceProxyDedupKey(px Proxy) string {
	return px.Key() + "\x00" + strconv.Itoa(len(px.Username)) + ":" + px.Username + strconv.Itoa(len(px.Password)) + ":" + px.Password
}

func requireJSONEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("unexpected trailing JSON value")
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '[':
		for decoder.More() {
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	_, err = decoder.Token()
	return err
}

func containsPort(ports []int, want int) bool {
	for _, port := range ports {
		if port == want {
			return true
		}
	}
	return false
}
