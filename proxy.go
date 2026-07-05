package main

import (
	"fmt"
	"net/url"
	"strconv"
)

// Proxy represents a single upstream node, regardless of which source or
// protocol it came from.
type Proxy struct {
	IP       string
	Port     string
	Username string
	Password string

	// Protocol is one of "socks5", "http", "https", or "proxyip".
	// "proxyip" nodes do not speak a generic proxy protocol (see sources.go)
	// and are never selected as an upstream for forwarding.
	Protocol string

	Country string
	City    string

	// SourceName identifies which configured Source produced this entry.
	SourceName string

	// LatencyMs is the round-trip time observed during the last health
	// check, in milliseconds. Zero if never measured.
	LatencyMs int64

	// SpeedKbps is the download throughput measured by an on-demand speed
	// test (see speedtest.go), in kilobits/sec. Zero if never tested.
	SpeedKbps float64

	// ExitIP is the address the outside world actually sees when traffic
	// is sent through this proxy, measured during the health check by
	// asking a geo service through the tunnel. It can differ from IP for
	// chained/transparent proxies (or when the whole host sits behind a
	// transparent egress proxy). Country/City reflect the geolocation of
	// this exit IP, not of IP. Empty if the exit probe couldn't determine
	// it (e.g. geo service rate-limited).
	ExitIP string
}

func (p Proxy) Addr() string {
	return p.IP + ":" + p.Port
}

func (p Proxy) String() string {
	scheme := p.Protocol
	if scheme == "" {
		scheme = "socks5"
	}
	if p.Username != "" {
		return fmt.Sprintf("%s://%s:%s@%s", scheme, p.Username, p.Password, p.Addr())
	}
	return fmt.Sprintf("%s://%s", scheme, p.Addr())
}

// Key returns a stable identity for dedup purposes (protocol-agnostic on
// purpose: the same ip:port showing up under two protocols is still the
// same physical dedup target for display, but callers that need
// protocol-specific identity should use Addr()+Protocol directly).
func (p Proxy) Key() string {
	return p.Protocol + "://" + p.Addr()
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
