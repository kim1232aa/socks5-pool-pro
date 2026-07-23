package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func auditTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *net.TCPConn, 1)
	go func() {
		conn, _ := listener.AcceptTCP()
		accepted <- conn
	}()
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server := <-accepted
	_ = listener.Close()
	return client, server
}

func TestAuditExternalClientSpoofsLoopbackHost(t *testing.T) {
	handler := NewStatusServer(NewProxyPool(), &ConfigStore{}).handler()
	req := httptest.NewRequest(http.MethodGet, "http://management.example/api/status?compact=1", nil)
	req.Host = "localhost:8080"
	req.RemoteAddr = "203.0.113.77:54321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("external request with spoofed loopback Host = %d, expected current bypass 200", rec.Code)
	}
}

func TestAuditSuccessfulEmptySourceDeletesLastGoodInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()

	source := Source{ID: "source-a", Name: "Source A", URL: server.URL, Format: FormatJSONArray, Protocol: "http"}
	proxies, err := fetchSourceWithClient(source, server.Client(), testSourceFetchPolicy(1))
	if err != nil || len(proxies) != 0 {
		t.Fatalf("empty source result = %d, %v; want zero proxies and nil error", len(proxies), err)
	}

	catalog := &CandidateCatalog{}
	old := Proxy{IP: "192.0.2.10", Port: "8080", Protocol: "http", SourceName: source.ID, SourceNames: []string{source.ID}}
	labels := map[string]string{source.ID: source.Name}
	first := catalog.begin([]Proxy{old}, labels, nil, 0)
	catalog.complete(first, nil, nil, nil)
	second := catalog.begin(proxies, labels, nil, 0)
	catalog.complete(second, nil, nil, nil)
	if got := len(catalog.snapshot.Load().records); got != 0 {
		t.Fatalf("catalog records = %d, expected current source-authoritative deletion", got)
	}
}

func TestAuditConfigMutationSurvivesPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	destinationDirectory := filepath.Join(dir, "pool_config.json")
	if err := os.Mkdir(destinationDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	store := &ConfigStore{path: destinationDirectory, cfg: defaultPoolConfig()}
	const changed = "https://example.com/health"
	if err := store.SetCheckURL(changed); err == nil {
		t.Fatal("SetCheckURL unexpectedly persisted over a directory")
	}
	if got := store.CheckURL(); got != changed {
		t.Fatalf("in-memory URL = %q, want failed mutation to remain reproducibly applied", got)
	}
}

func TestAuditDefaultHealthPassesPort80OnlyProxy(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "www.google.com:80" {
			http.Error(w, "CONNECT target denied", http.StatusForbidden)
			return
		}
		hijacker := w.(http.Hijacker)
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n"))
	}))
	defer proxyServer.Close()
	u, _ := url.Parse(proxyServer.URL)
	px := Proxy{IP: u.Hostname(), Port: u.Port(), Protocol: "http"}
	if !checkURL(px, defaultCheckURL, time.Second) {
		t.Fatal("port-80-only proxy did not pass current default HTTP health check")
	}
	if checkURL(px, "https://example.com/", time.Second) {
		t.Fatal("port-80-only proxy unexpectedly forwarded HTTPS")
	}
}

func TestAuditHTTPConnectRelayDoesNotPropagateClientHalfClose(t *testing.T) {
	client, relayLeft := auditTCPPair(t)
	relayRight, target := auditTCPPair(t)
	defer client.Close()
	defer target.Close()

	wrappedHTTPConnect := &bufConn{Conn: relayRight, r: bufio.NewReader(relayRight)}
	relayDone := make(chan struct{})
	go func() {
		relay(relayLeft, wrappedHTTPConnect)
		close(relayDone)
	}()

	_, _ = client.Write([]byte("request"))
	_ = client.CloseWrite()
	got := make([]byte, len("request"))
	if _, err := io.ReadFull(target, got); err != nil {
		t.Fatal(err)
	}
	_ = target.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var one [1]byte
	if _, err := target.Read(one[:]); err == io.EOF {
		t.Fatal("HTTP CONNECT relay unexpectedly propagated EOF")
	} else if err == nil {
		t.Fatal("unexpected extra tunneled byte")
	}

	_ = target.Close()
	_ = client.Close()
	select {
	case <-relayDone:
	case <-time.After(time.Second):
		t.Fatal("relay did not stop during cleanup")
	}
}

func TestAuditDedupeDiscardsAlternateCredentials(t *testing.T) {
	bad := Proxy{IP: "192.0.2.90", Port: "8080", Protocol: "http", Username: "old", Password: "bad", SourceName: "a-source"}
	good := bad
	good.Username, good.Password, good.SourceName = "rotated", "good", "b-source"
	got := dedupeCandidates([]Proxy{good, bad})
	if len(got) != 1 || got[0].Username != bad.Username || got[0].Password != bad.Password {
		t.Fatalf("dedupe = %#v; want deterministic discarded alternate credential pair", got)
	}
}

func TestAuditSOCKSDomainInjectsHTTPConnectHeaders(t *testing.T) {
	domain := "example.com HTTP/1.1\r\nX-Audit: injected\r\nIgnore"
	wire := []byte{socks5Version, cmdConnect, 0, atypDomain, byte(len(domain))}
	wire = append(wire, []byte(domain)...)
	wire = append(wire, 0x01, 0xbb) // port 443
	target, _, err := readConnectRequest(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("current SOCKS parser rejected control characters: %v", err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan string, 1)
	go func() {
		conn, _ := listener.Accept()
		if conn == nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		var raw strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			raw.WriteString(line)
			if readErr != nil || line == "\r\n" {
				break
			}
		}
		received <- raw.String()
		_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	_, _ = DialUpstream(Proxy{IP: host, Port: port, Protocol: "http"}, target, time.Second)
	raw := <-received
	if !strings.Contains(raw, "\r\nX-Audit: injected\r\n") {
		t.Fatalf("raw CONNECT request did not contain injected header: %q", raw)
	}
}
