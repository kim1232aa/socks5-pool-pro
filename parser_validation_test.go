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
		{"ipv4", Proxy{IP: "198.51.100.10", Port: "8080", Protocol: "HTTP"}, true},
		{"ipv6", Proxy{IP: "2001:db8::1", Port: "1080", Protocol: "socks5"}, true},
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

func TestFetchSourceFiltersMalformedEntriesBeforeSampling(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("198.51.100.10:8080\n999.999.999.999:80\n198.51.100.11:65536\n"))
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
	if got, want := proxies[0].Key(), "http://198.51.100.10:8080"; got != want {
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
		{"ip":"198.51.100.10","port":[80,443,8443],"meta":{"country":"DE","city":"Frankfurt","continent":"EU"}},
		{"ip":"198.51.100.11","port":[80,2053],"meta":{"country":"NL","city":"Amsterdam","continent":"EU"}},
		{"ip":"198.51.100.10","port":[443],"meta":{"country":"DE","city":"Frankfurt","continent":"EU"}}
	]}`)
	proxies, err := parseProxyIPJSON(body)
	if err != nil {
		t.Fatalf("parseProxyIPJSON() error = %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("parseProxyIPJSON() returned %d entries, want one unique 443 resource: %#v", len(proxies), proxies)
	}
	got := proxies[0]
	if got.Key() != "proxyip://198.51.100.10:443" || got.Country != "DE" || got.City != "Frankfurt" || got.Continent != "EU" {
		t.Fatalf("parsed ProxyIP metadata = %#v", got)
	}
}

func TestParseProxyIPJSONAcceptsScalarPort(t *testing.T) {
	body := []byte(`{"data":[{"ip":"198.51.100.11","port":443,"meta":{"country":"JP","continent":"AS"}}]}`)
	proxies, err := parseProxyIPJSON(body)
	if err != nil {
		t.Fatalf("parseProxyIPJSON() error = %v", err)
	}
	if len(proxies) != 1 || proxies[0].Key() != "proxyip://198.51.100.11:443" {
		t.Fatalf("scalar-port ProxyIP result = %#v", proxies)
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
