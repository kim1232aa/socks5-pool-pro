package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// proxyURLRegex matches "scheme://[user:pass@]ip:port" occurrences in plain
// text proxy lists (the original socks5-proxy.github.io format).
var proxyURLRegex = regexp.MustCompile(`(?i)(socks5|https?)://(?:[^\s@/]+@)?(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}):(\d+)`)

const maxFetchBytes = 64 << 20 // 64MB safety cap for source downloads

// FetchSource downloads and parses a single Source's node list, tagging
// every result with the source's name.
func FetchSource(src Source) ([]Proxy, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Get(src.URL)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return nil, fmt.Errorf("read body failed: %w", err)
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

	for i := range proxies {
		proxies[i].SourceName = src.Name
	}

	log.Printf("[fetch] %s: %d proxies from %s", src.Name, len(proxies), src.URL)
	return proxies, nil
}

// parseTextRegex extracts "scheme://ip:port" occurrences from a plain text
// or HTML page (e.g. socks5-proxy.github.io).
func parseTextRegex(body []byte) ([]Proxy, error) {
	matches := proxyURLRegex.FindAllStringSubmatch(string(body), -1)
	seen := make(map[string]bool)
	var proxies []Proxy

	for _, m := range matches {
		px := Proxy{
			Protocol: strings.ToLower(m[1]),
			IP:       m[2],
			Port:     m[3],
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
	Proxy    string `json:"proxy"`
	Protocol string `json:"protocol"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	Country  string `json:"country"`
	City     string `json:"city"`
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
// Cloudflare edge IPs used as the "ProxyIP" parameter in Worker/VLESS style
// reverse-tunnel tools. These do NOT speak SOCKS5/HTTP proxy protocol
// themselves, so entries parsed here are tagged Protocol="proxyip" and are
// never selected as a forwarding upstream (see pool.go / server.go).
type proxyIPFile struct {
	Data []proxyIPEntry `json:"data"`
}

type proxyIPEntry struct {
	IP   string `json:"ip"`
	Port []int  `json:"port"`
	Meta struct {
		Country string `json:"country"`
		City    string `json:"city"`
	} `json:"meta"`
}

func parseProxyIPJSON(body []byte) ([]Proxy, error) {
	var f proxyIPFile
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("parse proxyip json: %w", err)
	}

	seen := make(map[string]bool)
	var proxies []Proxy

	for _, e := range f.Data {
		if e.IP == "" || len(e.Port) == 0 {
			continue
		}
		for _, port := range e.Port {
			if port <= 0 {
				continue
			}
			px := Proxy{
				IP:       e.IP,
				Port:     strconv.Itoa(port),
				Protocol: "proxyip",
				Country:  e.Meta.Country,
				City:     e.Meta.City,
			}
			key := px.Key()
			if seen[key] {
				continue
			}
			seen[key] = true
			proxies = append(proxies, px)
		}
	}
	return proxies, nil
}
