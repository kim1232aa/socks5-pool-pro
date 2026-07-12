package main

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

const maxCredentialAlternates = 8

type ProxyCredential struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Proxy represents a single upstream node, regardless of which source or
// protocol it came from.
type Proxy struct {
	IP       string
	Port     string
	Username string
	Password string
	// CredentialAlternates retains a small bounded set of declarations for an
	// endpoint advertised with different credentials. Candidate identity and
	// health state remain endpoint-based, but validation can try every variant
	// instead of discarding a working login before it is tested.
	CredentialAlternates []ProxyCredential `json:"credential_alternates,omitempty"`

	// Protocol is one of "socks5", "http", "https", or "proxyip".
	// "proxyip" resources do not speak a generic proxy protocol (see parser.go)
	// and are never selected as an upstream for forwarding.
	Protocol string

	Country string
	City    string
	// Continent is the ISO-ish continent code (AS/NA/EU/AF/SA/OC/AN) of
	// Country, from the same LookupGeo call - used to group the dashboard's
	// country filter by continent, the way EDT-Pages' own panel does.
	Continent string

	// SourceName identifies which configured Source produced this entry.
	SourceName string
	// SourceNames retains every source that advertised this protocol+address.
	// SourceName remains the deterministic primary name for older API/UI
	// consumers; source-aware routing should consult both fields.
	SourceNames []string `json:"SourceNames,omitempty"`
	// SourceIDs is the stable configured-source provenance used to retire a
	// node from routing when every source that supplied it is disabled/deleted.
	// Display names above remain user-facing and may be renamed or duplicated.
	SourceIDs []string `json:"SourceIDs,omitempty"`
	// SourceRetired prevents background rechecks from making a node routable
	// after every source that supplied it has been disabled or deleted.
	SourceRetired bool `json:"source_retired,omitempty"`
	// Hard routing boundaries, distinct from an ordinary transient health
	// failure that may use the historical all-unavailable fallback.
	HealthInvalidated bool `json:"health_invalidated,omitempty"`
	PolicyExcluded    bool `json:"policy_excluded,omitempty"`

	// LatencyMs is the round-trip time observed during the last health
	// check, in milliseconds. Zero if never measured.
	LatencyMs int64

	// SpeedKbps is the download throughput measured by an on-demand speed
	// test (see speedtest.go), in kilobits/sec. Zero if never tested.
	SpeedKbps float64
	// SpeedTestedAt is the Unix timestamp of the last successful throughput
	// test. SpeedBytes and SpeedDurationMs describe the sample behind
	// SpeedKbps so consumers can distinguish a real full-size measurement
	// from a stale or partial result.
	SpeedTestedAt   int64
	SpeedBytes      int64
	SpeedDurationMs int64

	// ExitIP is the address the outside world actually sees when traffic
	// is sent through this proxy, measured during the health check by
	// asking a geo service through the tunnel. It can differ from IP for
	// chained/transparent proxies (or when the whole host sits behind a
	// transparent egress proxy). Country/City reflect the geolocation of
	// this exit IP, not of IP. Empty if the exit probe couldn't determine
	// it (e.g. geo service rate-limited).
	ExitIP string

	// IPChanged is true when the proxy's exit IP genuinely differs from
	// our own direct egress - i.e. using it actually changes your public
	// IP. It is meaningful only when IPChangeKnown is true.
	IPChanged     bool
	IPChangeKnown bool

	// Anonymity is "elite", "anonymous", "transparent", or "" (unknown),
	// classified by whether the proxy leaks your real IP / advertises
	// itself via request headers.
	Anonymity string

	// Available reflects the most recent health check that actually tested
	// this node: true if it passed, false if it was tested and failed.
	// Nodes are never dropped from the pool just for failing a check - only
	// this flag flips, so a node that starts working again (common for
	// free/rotating proxies) self-heals on its next successful check
	// instead of having to be rediscovered from a source feed.
	Available bool `json:"available"`
}

func (p Proxy) Addr() string {
	return net.JoinHostPort(p.IP, p.Port)
}

func (p Proxy) String() string {
	return p.urlWithScheme(p.Protocol)
}

// ConsumerURL returns a proxy URL that a standard external client can dial.
// Internally, the source label "https" means an HTTP proxy that can CONNECT
// HTTPS destinations; dialHTTPConnect intentionally speaks plain HTTP to both
// "http" and "https" labels. Exporting https:// for that latter case would
// incorrectly make standard clients attempt TLS to the proxy itself.
func (p Proxy) ConsumerURL() string {
	scheme := p.Protocol
	if scheme == "https" {
		scheme = "http"
	}
	return p.urlWithScheme(scheme)
}

func (p Proxy) urlWithScheme(scheme string) string {
	if scheme == "" {
		scheme = "socks5"
	}
	u := &url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(p.IP, p.Port),
	}
	if p.Username != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u.String()
}

// Key returns the protocol-aware identity used for validation and pool state.
// The same ip:port may legitimately support one advertised protocol but not
// another, so HTTP and SOCKS declarations must never share failure state.
func (p Proxy) Key() string {
	return p.Protocol + "://" + p.Addr()
}

func (p Proxy) credentialCandidates() []Proxy {
	out := make([]Proxy, 0, 1+len(p.CredentialAlternates))
	seen := make(map[ProxyCredential]bool, 1+len(p.CredentialAlternates))
	appendCredential := func(credential ProxyCredential) {
		if seen[credential] {
			return
		}
		seen[credential] = true
		candidate := p
		candidate.Username = credential.Username
		candidate.Password = credential.Password
		out = append(out, candidate)
	}
	appendCredential(ProxyCredential{Username: p.Username, Password: p.Password})
	for _, credential := range p.CredentialAlternates {
		appendCredential(credential)
	}
	return out
}

func (p Proxy) promoteCredential(successful Proxy) Proxy {
	oldPrimary := ProxyCredential{Username: p.Username, Password: p.Password}
	newPrimary := ProxyCredential{Username: successful.Username, Password: successful.Password}
	p.Username, p.Password = successful.Username, successful.Password
	p.CredentialAlternates = nil
	appendAlternative := func(credential ProxyCredential) {
		if credential == newPrimary || len(p.CredentialAlternates) >= maxCredentialAlternates {
			return
		}
		for _, existing := range p.CredentialAlternates {
			if existing == credential {
				return
			}
		}
		p.CredentialAlternates = append(p.CredentialAlternates, credential)
	}
	appendAlternative(oldPrimary)
	for _, credential := range successful.CredentialAlternates {
		appendAlternative(credential)
	}
	return p
}

// parseProxyURL parses strings like "socks5://user:pass@1.2.3.4:1080" or
// "http://1.2.3.4:8080" into a partially-filled Proxy (IP/Port/Username/
// Password/Protocol only).
func parseProxyURL(raw string) (Proxy, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Proxy{}, err
	}
	if u.Hostname() == "" || u.Port() == "" {
		return Proxy{}, fmt.Errorf("proxy URL missing host or port: %q", raw)
	}
	// Validate port is numeric.
	if _, err := strconv.Atoi(u.Port()); err != nil {
		return Proxy{}, fmt.Errorf("invalid port in proxy URL %q: %w", raw, err)
	}
	px := Proxy{
		IP:       u.Hostname(),
		Port:     u.Port(),
		Protocol: u.Scheme,
	}
	if u.User != nil {
		px.Username = u.User.Username()
		px.Password, _ = u.User.Password()
	}
	return px, nil
}
