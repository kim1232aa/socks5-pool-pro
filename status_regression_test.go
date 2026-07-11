package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNodeViewsMarksOnlySelectedProtocolVariantActive(t *testing.T) {
	pool := NewProxyPool()
	httpProxy := Proxy{
		IP: "198.51.100.40", Port: "8080", Protocol: "http", Available: true,
	}
	socksProxy := Proxy{
		IP: "198.51.100.40", Port: "8080", Protocol: "socks5", Available: true,
	}
	pool.Prime([]Proxy{httpProxy, socksProxy}, nil)

	if !pool.ForceSticky(GroupAny, socksProxy.Key()) {
		t.Fatalf("ForceSticky(%q) = false", socksProxy.Key())
	}

	views := NewStatusServer(pool, &ConfigStore{}).nodeViews()
	active := make([]NodeView, 0, 1)
	for _, view := range views {
		if view.Active {
			active = append(active, view)
		}
	}
	if len(active) != 1 {
		t.Fatalf("active views = %#v, want exactly one", active)
	}
	if got, want := active[0].Key, socksProxy.Key(); got != want {
		t.Fatalf("active key = %q, want selected SOCKS key %q", got, want)
	}
	for _, view := range views {
		if view.Key == httpProxy.Key() && view.Active {
			t.Fatalf("HTTP variant at the same address was incorrectly marked active: %#v", view)
		}
	}
}

func TestProxyIPv6AddressStringAndKeyAreValidAndEscaped(t *testing.T) {
	proxy := Proxy{
		IP:       "2001:db8::42",
		Port:     "1080",
		Protocol: "socks5",
		Username: "user@example",
		Password: "pa:ss/word?",
	}

	if got, want := proxy.Addr(), "[2001:db8::42]:1080"; got != want {
		t.Fatalf("Addr() = %q, want %q", got, want)
	}
	if got, want := proxy.Key(), "socks5://[2001:db8::42]:1080"; got != want {
		t.Fatalf("Key() = %q, want %q", got, want)
	}

	gotURL := proxy.String()
	const wantURL = "socks5://user%40example:pa%3Ass%2Fword%3F@[2001:db8::42]:1080"
	if gotURL != wantURL {
		t.Fatalf("String() = %q, want %q", gotURL, wantURL)
	}
	parsed, err := url.Parse(gotURL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	if got, want := parsed.Hostname(), proxy.IP; got != want {
		t.Fatalf("URL hostname = %q, want %q", got, want)
	}
	if got, want := parsed.Port(), proxy.Port; got != want {
		t.Fatalf("URL port = %q, want %q", got, want)
	}
	if got, want := parsed.User.Username(), proxy.Username; got != want {
		t.Fatalf("URL username = %q, want %q", got, want)
	}
	password, ok := parsed.User.Password()
	if !ok || password != proxy.Password {
		t.Fatalf("URL password = %q, present=%v; want %q", password, ok, proxy.Password)
	}
}

func TestGzipIfAcceptedCompressesJSONAndPreservesPlainResponses(t *testing.T) {
	handler := gzipIfAccepted(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	}))

	gzipped := httptest.NewRecorder()
	gzipRequest := localTestRequest(http.MethodGet, "/api/status", nil)
	gzipRequest.Header.Set("Accept-Encoding", "br, gzip")
	handler.ServeHTTP(gzipped, gzipRequest)
	if got, want := gzipped.Code, http.StatusOK; got != want {
		t.Fatalf("gzip response status = %d, want %d", got, want)
	}
	if got := gzipped.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("gzip Content-Encoding = %q, want gzip", got)
	}
	if got := gzipped.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("gzip Vary = %q, want Accept-Encoding", got)
	}

	reader, err := gzip.NewReader(bytes.NewReader(gzipped.Body.Bytes()))
	if err != nil {
		t.Fatalf("open gzip response: %v", err)
	}
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		reader.Close()
		t.Fatalf("read gzip response: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close gzip response: %v", err)
	}
	var gzipBody map[string]string
	if err := json.Unmarshal(decompressed, &gzipBody); err != nil {
		t.Fatalf("decode decompressed JSON: %v; body=%q", err, decompressed)
	}
	if got, want := gzipBody["status"], "ok"; got != want {
		t.Fatalf("decompressed status = %q, want %q", got, want)
	}

	plain := httptest.NewRecorder()
	plainRequest := localTestRequest(http.MethodGet, "/api/status", nil)
	handler.ServeHTTP(plain, plainRequest)
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("plain Content-Encoding = %q, want none", got)
	}
	var plainBody map[string]string
	if err := json.Unmarshal(plain.Body.Bytes(), &plainBody); err != nil {
		t.Fatalf("decode plain JSON: %v; body=%q", err, plain.Body.Bytes())
	}
	if got, want := plainBody["status"], "ok"; got != want {
		t.Fatalf("plain status = %q, want %q", got, want)
	}
}

func TestDecodeJSONRejectsOversizedBodiesAndAcceptsValidJSON(t *testing.T) {
	validRequest := localTestRequest(http.MethodPost, "/api/nodes", strings.NewReader(`{"key":"socks5://[2001:db8::1]:1080"}`))
	var valid struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(validRequest, &valid); err != nil {
		t.Fatalf("decode valid JSON: %v", err)
	}
	if got, want := valid.Key, "socks5://[2001:db8::1]:1080"; got != want {
		t.Fatalf("decoded key = %q, want %q", got, want)
	}

	overLimit := `{"value":"` + strings.Repeat("x", maxJSONBodyBytes) + `"}`
	overLimitRequest := localTestRequest(http.MethodPost, "/api/nodes", strings.NewReader(overLimit))
	var ignored map[string]string
	err := decodeJSON(overLimitRequest, &ignored)
	if err == nil {
		t.Fatal("decodeJSON accepted a body larger than 1 MiB")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized-body error = %v, want limit error", err)
	}
}
