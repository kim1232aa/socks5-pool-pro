package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeProxyRejectsMalformedCandidates(t *testing.T) {
	tests := []struct {
		name string
		in   Proxy
		ok   bool
	}{
		{"ipv4", Proxy{IP: "8.8.8.8", Port: "8080", Protocol: "HTTP"}, true},
		{"ipv6", Proxy{IP: "2606:4700:4700::1111", Port: "1080", Protocol: "socks5"}, true},
		{"hostname", Proxy{IP: "proxy.example.test", Port: "3128", Protocol: "https"}, true},
		{"invalid ipv4", Proxy{IP: "999.999.999.999", Port: "8080", Protocol: "http"}, false},
		{"zero port", Proxy{IP: "198.51.100.10", Port: "0", Protocol: "http"}, false},
		{"large port", Proxy{IP: "198.51.100.10", Port: "65536", Protocol: "http"}, false},
		{"unsupported protocol", Proxy{IP: "198.51.100.10", Port: "8080", Protocol: "ftp"}, false},
		{"malformed hostname", Proxy{IP: "proxy example", Port: "8080", Protocol: "http"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeProxy(tt.in)
			if ok != tt.ok {
				t.Fatalf("normalizeProxy(%#v) ok=%v, want %v", tt.in, ok, tt.ok)
			}
			if ok && got.Protocol != "http" && tt.in.Protocol == "HTTP" {
				t.Fatalf("protocol was not normalized: %#v", got)
			}
		})
	}
}

func TestNormalizeFetchedProxyRejectsNonPublicLiteralIPsButKeepsHostnames(t *testing.T) {
	tests := []struct {
		name string
		host string
		ok   bool
	}{
		{"zero network", "0.103.177.131", false},
		{"rfc1918 10", "10.0.0.1", false},
		{"carrier grade nat", "100.64.0.1", false},
		{"loopback", "127.0.0.1", false},
		{"link local", "169.254.169.254", false},
		{"rfc1918 172", "172.16.0.1", false},
		{"rfc1918 192", "192.168.1.1", false},
		{"benchmark", "198.18.0.1", false},
		{"documentation v4", "203.0.113.1", false},
		{"multicast", "224.0.0.1", false},
		{"reserved", "240.0.0.1", false},
		{"unspecified v6", "::", false},
		{"loopback v6", "::1", false},
		{"nat64", "64:ff9b::1", false},
		{"documentation v6", "2001:db8::1", false},
		{"six to four", "2002:c000:0204::1", false},
		{"unique local", "fd00::1", false},
		{"link local v6", "fe80::1", false},
		{"multicast v6", "ff02::1", false},
		{"public v4", "8.8.8.8", true},
		{"public v6", "2606:4700:4700::1111", true},
		{"hostname unchanged", "proxy.example.test", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeFetchedProxy(Proxy{IP: tt.host, Port: "1080", Protocol: "socks5"})
			if ok != tt.ok {
				t.Fatalf("normalizeFetchedProxy(%q) ok=%v, want %v (got %#v)", tt.host, ok, tt.ok, got)
			}
		})
	}
}

func TestNormalizeProxyKeepsProxyIPLiteralPublicAndFixedTo443(t *testing.T) {
	tests := []struct {
		name string
		in   Proxy
		ok   bool
	}{
		{"public 443", Proxy{IP: "1.1.1.1", Port: "443", Protocol: "proxyip"}, true},
		{"private", Proxy{IP: "10.0.0.1", Port: "443", Protocol: "proxyip"}, false},
		{"documentation", Proxy{IP: "192.0.2.1", Port: "443", Protocol: "proxyip"}, false},
		{"hostname", Proxy{IP: "proxy.example.test", Port: "443", Protocol: "proxyip"}, false},
		{"wrong port", Proxy{IP: "1.1.1.1", Port: "8443", Protocol: "proxyip"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := normalizeProxy(tt.in)
			if ok != tt.ok {
				t.Fatalf("normalizeProxy(%#v) ok=%v, want %v", tt.in, ok, tt.ok)
			}
		})
	}
}

func TestFetchSourceFiltersInvalidEntriesIndividuallyBeforeSampling(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("8.8.8.8:8080\n0.103.177.131:8015\n999.999.999.999:80\n1.1.1.1:65536\n"))
	}))
	defer feed.Close()

	proxies, err := FetchSource(Source{
		Name: "test", URL: feed.URL, Format: FormatPlainList, Protocol: "http", AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("FetchSource() error = %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("FetchSource() returned %d proxies, want only valid one: %#v", len(proxies), proxies)
	}
	if got, want := proxies[0].Key(), "http://8.8.8.8:8080"; got != want {
		t.Fatalf("proxy key = %q, want %q", got, want)
	}
}

func TestParseTextRegexKeepsAuthenticatedAndIPv6ProxyURLs(t *testing.T) {
	proxies, err := parseTextRegex([]byte("try socks5://alice:secret@[2001:db8::7]:1080, then http://198.51.100.20:8080."))
	if err != nil {
		t.Fatalf("parseTextRegex() error = %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("parseTextRegex() returned %d proxies, want 2: %#v", len(proxies), proxies)
	}
	if got, want := proxies[0].String(), "socks5://alice:secret@[2001:db8::7]:1080"; got != want {
		t.Fatalf("first proxy = %q, want %q", got, want)
	}
	if got, want := proxies[1].Key(), "http://198.51.100.20:8080"; got != want {
		t.Fatalf("second proxy key = %q, want %q", got, want)
	}
}

func TestParseProxyIPJSONKeepsOne443ResourcePerIPAndContinent(t *testing.T) {
	body := []byte(`{"data":[
		{"ip":"1.1.1.1","port":[80,443,8443],"meta":{"country":"DE","city":"Frankfurt","continent":"EU"}},
		{"ip":"8.8.8.8","port":[80,2053],"meta":{"country":"NL","city":"Amsterdam","continent":"EU"}},
		{"ip":"1.1.1.1","port":[443],"meta":{"country":"DE","city":"Frankfurt","continent":"EU"}}
	]}`)
	proxies, err := parseProxyIPJSON(body)
	if err != nil {
		t.Fatalf("parseProxyIPJSON() error = %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("parseProxyIPJSON() returned %d entries, want one unique 443 resource: %#v", len(proxies), proxies)
	}
	got := proxies[0]
	if got.Key() != "proxyip://1.1.1.1:443" || got.Country != "DE" || got.City != "Frankfurt" || got.Continent != "EU" {
		t.Fatalf("parsed ProxyIP metadata = %#v", got)
	}
}

func TestParseProxyIPJSONAcceptsScalarPort(t *testing.T) {
	body := []byte(`{"data":[{"ip":"8.8.8.8","port":443,"meta":{"country":"JP","continent":"AS"}}]}`)
	proxies, err := parseProxyIPJSON(body)
	if err != nil {
		t.Fatalf("parseProxyIPJSON() error = %v", err)
	}
	if len(proxies) != 1 || proxies[0].Key() != "proxyip://8.8.8.8:443" {
		t.Fatalf("scalar-port ProxyIP result = %#v", proxies)
	}
}

func TestParseProxyIPJSONRejectsHostnameResources(t *testing.T) {
	body := []byte(`{"data":[{"ip":"proxy.example.com","port":[443]},{"ip":"8.8.4.4","port":[443]}]}`)
	proxies, err := parseProxyIPJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 || proxies[0].IP != "8.8.4.4" {
		t.Fatalf("ProxyIP hostname was retained: %#v", proxies)
	}
}

func TestParseProxyIPJSONRejectsNonPublicLiteralResources(t *testing.T) {
	body := []byte(`{"data":[
		{"ip":"0.103.177.131","port":[443]},
		{"ip":"10.0.0.1","port":[443]},
		{"ip":"100.64.0.1","port":[443]},
		{"ip":"127.0.0.1","port":[443]},
		{"ip":"169.254.1.1","port":[443]},
		{"ip":"192.0.2.1","port":[443]},
		{"ip":"224.0.0.1","port":[443]},
		{"ip":"2001:db8::1","port":[443]},
		{"ip":"1.0.0.1","port":[443]}
	]}`)
	proxies, err := parseProxyIPJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 || proxies[0].Key() != "proxyip://1.0.0.1:443" {
		t.Fatalf("ProxyIP parser retained non-public literals: %#v", proxies)
	}
}

func TestParseEDTJSONKeepsSourceContinentMetadata(t *testing.T) {
	proxies, err := parseEDTJSON([]byte(`[{"proxy":"socks5://198.51.100.20:1080","country":"JP","city":"Tokyo","continent":"AS"}]`))
	if err != nil {
		t.Fatalf("parseEDTJSON() error = %v", err)
	}
	if len(proxies) != 1 || proxies[0].Country != "JP" || proxies[0].City != "Tokyo" || proxies[0].Continent != "AS" {
		t.Fatalf("EDT metadata = %#v", proxies)
	}
}
