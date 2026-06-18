package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
)

var proxyRegex = regexp.MustCompile(`socks5://(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}):(\d+)`)

type Proxy struct {
	IP      string
	Port    string
	Country string
	City    string
}

func (p Proxy) Addr() string {
	return p.IP + ":" + p.Port
}

func (p Proxy) String() string {
	return fmt.Sprintf("socks5://%s:%s", p.IP, p.Port)
}

func Scrape(url string) ([]Proxy, error) {
	t := &http.Transport{}
	t.RegisterProtocol("file", http.NewFileTransport(http.Dir("/")))

	c := &http.Client{Transport: t}
    resp, err := c.Get(url)

	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body failed: %w", err)
	}

	matches := proxyRegex.FindAllStringSubmatch(string(body), -1)
	seen := make(map[string]bool)
	var proxies []Proxy

	for _, m := range matches {
		addr := m[1] + ":" + m[2]
		if seen[addr] {
			continue
		}
		seen[addr] = true
		proxies = append(proxies, Proxy{
			IP:   strings.TrimSpace(m[1]),
			Port: strings.TrimSpace(m[2]),
		})
	}

	log.Printf("[scraper] fetched %d proxies from %s", len(proxies), url)
	return proxies, nil
}
