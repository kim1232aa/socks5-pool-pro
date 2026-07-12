package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fragmentedReadWriter struct {
	chunks [][]byte
	writes bytes.Buffer
}

func newFragmentedReadWriter(chunks ...[]byte) *fragmentedReadWriter {
	cloned := make([][]byte, len(chunks))
	for i, chunk := range chunks {
		cloned[i] = append([]byte(nil), chunk...)
	}
	return &fragmentedReadWriter{chunks: cloned}
}

func (rw *fragmentedReadWriter) Read(p []byte) (int, error) {
	for len(rw.chunks) > 0 && len(rw.chunks[0]) == 0 {
		rw.chunks = rw.chunks[1:]
	}
	if len(rw.chunks) == 0 {
		return 0, io.EOF
	}

	n := copy(p, rw.chunks[0])
	rw.chunks[0] = rw.chunks[0][n:]
	return n, nil
}

func (rw *fragmentedReadWriter) Write(p []byte) (int, error) {
	return rw.writes.Write(p)
}

func oneByteChunks(data []byte) [][]byte {
	chunks := make([][]byte, len(data))
	for i, b := range data {
		chunks[i] = []byte{b}
	}
	return chunks
}

func TestNegotiateNoAuthReadsFragmentedGreeting(t *testing.T) {
	rw := newFragmentedReadWriter(
		[]byte{socks5Version},
		[]byte{2},
		[]byte{0x02},
		[]byte{socks5NoAuth},
	)

	if err := negotiateNoAuth(rw); err != nil {
		t.Fatalf("negotiateNoAuth() error = %v", err)
	}
	if got, want := rw.writes.Bytes(), []byte{socks5Version, socks5NoAuth}; !bytes.Equal(got, want) {
		t.Fatalf("method reply = %v, want %v", got, want)
	}
}

func TestNegotiateNoAuthRejectsIncompleteMethodList(t *testing.T) {
	// NMETHODS declares two entries, but only one arrives. The server must not
	// accept the partial greeting merely because that first method is no-auth.
	rw := newFragmentedReadWriter([]byte{socks5Version, 2}, []byte{socks5NoAuth})

	if err := negotiateNoAuth(rw); err == nil {
		t.Fatal("negotiateNoAuth() accepted an incomplete method list")
	}
	if rw.writes.Len() != 0 {
		t.Fatalf("incomplete greeting produced reply %v", rw.writes.Bytes())
	}
}

func TestNegotiateNoAuthRejectsClientsWithoutNoAuth(t *testing.T) {
	tests := []struct {
		name     string
		greeting []byte
	}{
		{name: "userpass only", greeting: []byte{socks5Version, 1, 0x02}},
		{name: "zero methods", greeting: []byte{socks5Version, 0}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rw := newFragmentedReadWriter(oneByteChunks(tt.greeting)...)
			if err := negotiateNoAuth(rw); err == nil {
				t.Fatal("negotiateNoAuth() accepted a client without no-auth")
			}
			if got, want := rw.writes.Bytes(), []byte{socks5Version, socks5NoAcceptableMethods}; !bytes.Equal(got, want) {
				t.Fatalf("method reply = %v, want %v", got, want)
			}
		})
	}
}

func TestReadConnectRequestReadsFragmentedAddressTypes(t *testing.T) {
	ipv6 := net.ParseIP("2001:db8::1").To16()
	tests := []struct {
		name  string
		frame []byte
		want  string
	}{
		{
			name:  "IPv4",
			frame: []byte{socks5Version, cmdConnect, 0x00, atypIPv4, 192, 0, 2, 1, 0x1f, 0x90},
			want:  "192.0.2.1:8080",
		},
		{
			name: "domain",
			frame: append(
				[]byte{socks5Version, cmdConnect, 0x00, atypDomain, byte(len("example.com"))},
				append([]byte("example.com"), 0x01, 0xbb)...,
			),
			want: "example.com:443",
		},
		{
			name:  "IPv6",
			frame: append([]byte{socks5Version, cmdConnect, 0x00, atypIPv6}, append(ipv6, 0x00, 0x35)...),
			want:  "[2001:db8::1]:53",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const trailingApplicationData = "already-buffered-payload"
			wire := append(append([]byte(nil), tt.frame...), trailingApplicationData...)
			rw := newFragmentedReadWriter(oneByteChunks(wire)...)

			got, status, err := readConnectRequest(rw)
			if err != nil {
				t.Fatalf("readConnectRequest() error = %v (status %d)", err, status)
			}
			if got != tt.want {
				t.Fatalf("target = %q, want %q", got, tt.want)
			}

			remaining, err := io.ReadAll(rw)
			if err != nil {
				t.Fatal(err)
			}
			if string(remaining) != trailingApplicationData {
				t.Fatalf("remaining data = %q, want %q", remaining, trailingApplicationData)
			}
		})
	}
}

func TestReadConnectRequestValidatesHeader(t *testing.T) {
	tests := []struct {
		name       string
		header     []byte
		wantStatus byte
	}{
		{name: "version", header: []byte{0x04, cmdConnect, 0x00, atypIPv4}, wantStatus: replyGeneralFailure},
		{name: "reserved byte", header: []byte{socks5Version, cmdConnect, 0x01, atypIPv4}, wantStatus: replyGeneralFailure},
		{name: "command", header: []byte{socks5Version, 0x02, 0x00, atypIPv4}, wantStatus: replyCommandNotSupported},
		{name: "address type", header: []byte{socks5Version, cmdConnect, 0x00, 0x02}, wantStatus: replyAddressTypeNotSupported},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, status, err := readConnectRequest(bytes.NewReader(tt.header))
			if err == nil {
				t.Fatal("readConnectRequest() accepted invalid header")
			}
			if status != tt.wantStatus {
				t.Fatalf("reply status = %d, want %d", status, tt.wantStatus)
			}
		})
	}
}

func TestReadConnectRequestRejectsUnsafeDomainsAndZeroPorts(t *testing.T) {
	longLabel := strings.Repeat("a", 64) + ".example"
	tooLongDomain := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62)
	tests := []struct {
		name  string
		frame []byte
	}{
		{name: "CRLF", frame: testSOCKSDomainFrame("example.com\r\nX-Test: injected", 443)},
		{name: "NUL", frame: testSOCKSDomainFrame("example.com\x00", 443)},
		{name: "space", frame: testSOCKSDomainFrame("example .com", 443)},
		{name: "colon", frame: testSOCKSDomainFrame("example.com:80", 443)},
		{name: "brackets", frame: testSOCKSDomainFrame("[example.com]", 443)},
		{name: "slash", frame: testSOCKSDomainFrame("example.com/path", 443)},
		{name: "backslash", frame: testSOCKSDomainFrame(`example.com\path`, 443)},
		{name: "userinfo", frame: testSOCKSDomainFrame("user@example.com", 443)},
		{name: "query", frame: testSOCKSDomainFrame("example.com?x", 443)},
		{name: "fragment", frame: testSOCKSDomainFrame("example.com#x", 443)},
		{name: "empty label", frame: testSOCKSDomainFrame("example..com", 443)},
		{name: "long label", frame: testSOCKSDomainFrame(longLabel, 443)},
		{name: "domain over 253 bytes", frame: testSOCKSDomainFrame(tooLongDomain, 443)},
		{name: "domain port zero", frame: testSOCKSDomainFrame("example.com", 0)},
		{name: "IPv4 port zero", frame: []byte{socks5Version, cmdConnect, 0, atypIPv4, 192, 0, 2, 1, 0, 0}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, status, err := readConnectRequest(bytes.NewReader(test.frame)); err == nil || status != replyHostUnreachable {
				t.Fatalf("readConnectRequest() status=%d error=%v, want host-unreachable rejection", status, err)
			}
		})
	}
}

func TestReadConnectRequestAcceptsStrictDNSBoundaries(t *testing.T) {
	maxDomain := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	for _, domain := range []string{"Example.COM", "xn--bcher-kva.example", maxDomain} {
		t.Run(domain[:min(len(domain), 24)], func(t *testing.T) {
			target, status, err := readConnectRequest(bytes.NewReader(testSOCKSDomainFrame(domain, 65535)))
			if err != nil || status != replySucceeded || target != net.JoinHostPort(domain, "65535") {
				t.Fatalf("strict valid domain result = %q status=%d error=%v", target, status, err)
			}
		})
	}
}

func testSOCKSDomainFrame(domain string, port int) []byte {
	frame := []byte{socks5Version, cmdConnect, 0, atypDomain, byte(len(domain))}
	frame = append(frame, domain...)
	return append(frame, byte(port>>8), byte(port))
}

type deadlineRecordingConn struct {
	net.Conn
	mu        sync.Mutex
	deadlines []time.Time
}

func (c *deadlineRecordingConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, deadline)
	c.mu.Unlock()
	return c.Conn.SetDeadline(deadline)
}

func (c *deadlineRecordingConn) recordedDeadlines() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.deadlines...)
}

func TestHandleConnAcceptsFragmentedFramesAndRelays(t *testing.T) {
	targetListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetListener.Close()

	targetDone := make(chan error, 1)
	go func() {
		conn, err := targetListener.Accept()
		if err != nil {
			targetDone <- err
			return
		}
		defer conn.Close()
		_, err = io.Copy(conn, conn)
		targetDone <- err
	}()

	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	recorded := &deadlineRecordingConn{Conn: serverSide}
	store := &ConfigStore{cfg: PoolConfig{
		Rules: []Rule{{Type: RuleMatch, Group: GroupDirect}},
	}}
	serverDone := make(chan struct{})
	go func() {
		NewServer("", NewProxyPool(), store).handleConn(recorded)
		close(serverDone)
	}()

	for _, fragment := range [][]byte{{socks5Version}, {1}, {socks5NoAuth}} {
		if _, err := clientSide.Write(fragment); err != nil {
			t.Fatal(err)
		}
	}
	methodReply := make([]byte, 2)
	if _, err := io.ReadFull(clientSide, methodReply); err != nil {
		t.Fatal(err)
	}
	if want := []byte{socks5Version, socks5NoAuth}; !bytes.Equal(methodReply, want) {
		t.Fatalf("method reply = %v, want %v", methodReply, want)
	}

	target := targetListener.Addr().(*net.TCPAddr)
	request := []byte{
		socks5Version, cmdConnect, 0x00, atypIPv4,
		target.IP[0], target.IP[1], target.IP[2], target.IP[3],
		byte(target.Port >> 8), byte(target.Port),
	}
	for _, fragment := range oneByteChunks(request) {
		if _, err := clientSide.Write(fragment); err != nil {
			t.Fatal(err)
		}
	}
	connectReply := make([]byte, 10)
	if _, err := io.ReadFull(clientSide, connectReply); err != nil {
		t.Fatal(err)
	}
	if connectReply[1] != replySucceeded {
		t.Fatalf("CONNECT reply status = %d, want success", connectReply[1])
	}

	deadlines := recorded.recordedDeadlines()
	if len(deadlines) < 2 || deadlines[0].IsZero() || !deadlines[len(deadlines)-1].IsZero() {
		t.Fatalf("handshake deadline was not set then cleared: %v", deadlines)
	}

	const payload = "fragmented SOCKS5 request reached relay"
	if _, err := clientSide.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(clientSide, echo); err != nil {
		t.Fatal(err)
	}
	if string(echo) != payload {
		t.Fatalf("relay returned %q, want %q", echo, payload)
	}

	if err := clientSide.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SOCKS5 handler did not stop after client close")
	}
	select {
	case err := <-targetDone:
		if err != nil {
			t.Fatalf("target echo server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("target echo server did not stop")
	}
}

func TestHandleConnRetriesAuthenticationCandidateAndPromotesIt(t *testing.T) {
	targetListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetListener.Close()
	targetDone := make(chan error, 1)
	go func() {
		conn, acceptErr := targetListener.Accept()
		if acceptErr != nil {
			targetDone <- acceptErr
			return
		}
		defer conn.Close()
		_, copyErr := io.Copy(conn, conn)
		targetDone <- copyErr
	}()

	upstream, connectAttempts := newSpeedTestConnectProxyWithAuth(t, "working", "secret")
	upstream.Username, upstream.Password = "old", "wrong"
	upstream.CredentialAlternates = []ProxyCredential{{Username: "working", Password: "secret"}}
	upstream.Available = true
	pool := NewProxyPool()
	pool.Prime([]Proxy{upstream}, nil)
	store := &ConfigStore{cfg: PoolConfig{Rules: []Rule{{Type: RuleMatch, Group: GroupAny}}}}
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	handlerDone := make(chan struct{})
	go func() {
		NewServer("", pool, store).handleConn(serverSide)
		close(handlerDone)
	}()

	_, _ = clientSide.Write([]byte{socks5Version, 1, socks5NoAuth})
	methodReply := make([]byte, 2)
	if _, err := io.ReadFull(clientSide, methodReply); err != nil {
		t.Fatal(err)
	}
	target := targetListener.Addr().(*net.TCPAddr)
	request := []byte{
		socks5Version, cmdConnect, 0x00, atypIPv4,
		target.IP[0], target.IP[1], target.IP[2], target.IP[3],
		byte(target.Port >> 8), byte(target.Port),
	}
	_, _ = clientSide.Write(request)
	connectReply := make([]byte, 10)
	if _, err := io.ReadFull(clientSide, connectReply); err != nil {
		t.Fatal(err)
	}
	if connectReply[1] != replySucceeded {
		t.Fatalf("credential retry CONNECT reply = %d", connectReply[1])
	}
	const payload = "credential alternate carried this tunnel"
	_, _ = clientSide.Write([]byte(payload))
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(clientSide, echo); err != nil || string(echo) != payload {
		t.Fatalf("credential retry relay = %q, %v", echo, err)
	}
	if got := connectAttempts.Load(); got != 2 {
		t.Fatalf("HTTP CONNECT credential attempts = %d, want 2", got)
	}
	got, ok := pool.Find(upstream.Key())
	if !ok || got.Username != "working" || got.Password != "secret" {
		t.Fatalf("forwarding promotion = %+v found=%v", got, ok)
	}

	_ = clientSide.Close()
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("credential retry handler did not stop")
	}
	select {
	case err := <-targetDone:
		if err != nil {
			t.Fatalf("credential retry target: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("credential retry target did not stop")
	}
}

func TestHandleConnTargetRefusalDoesNotMarkUpstreamUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	upstreamDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			upstreamDone <- acceptErr
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				upstreamDone <- readErr
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, writeErr := conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
		upstreamDone <- writeErr
	}()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	upstream := Proxy{IP: host, Port: port, Protocol: "http", Available: true}
	pool := NewProxyPool()
	pool.Prime([]Proxy{upstream}, nil)
	store := &ConfigStore{cfg: PoolConfig{Rules: []Rule{{Type: RuleMatch, Group: GroupAny}}}}
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	handlerDone := make(chan struct{})
	go func() {
		NewServer("", pool, store).handleConn(serverSide)
		close(handlerDone)
	}()

	_, _ = clientSide.Write([]byte{socks5Version, 1, socks5NoAuth})
	methodReply := make([]byte, 2)
	if _, err := io.ReadFull(clientSide, methodReply); err != nil {
		t.Fatal(err)
	}
	_, _ = clientSide.Write(testSOCKSDomainFrame("example.com", 443))
	connectReply := make([]byte, 10)
	if _, err := io.ReadFull(clientSide, connectReply); err != nil {
		t.Fatal(err)
	}
	if connectReply[1] != replyGeneralFailure {
		t.Fatalf("SOCKS reply = %d, want general failure", connectReply[1])
	}
	_ = clientSide.Close()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("SOCKS handler did not finish")
	}
	if err := <-upstreamDone; err != nil {
		t.Fatalf("test upstream error: %v", err)
	}
	got, ok := pool.Find(upstream.Key())
	if !ok || !got.Available {
		t.Fatalf("target refusal changed upstream health: found=%v proxy=%+v", ok, got)
	}
	if successes, failures := pool.StatsOf(upstream.Key()); successes != 0 || failures != 0 {
		t.Fatalf("target refusal changed global reliability stats: successes=%d failures=%d", successes, failures)
	}
}

func TestRelayPropagatesHalfCloseThroughBufferedHTTPConnection(t *testing.T) {
	client, relayLeft := testTCPConnectionPair(t)
	relayRight, target := testTCPConnectionPair(t)
	for _, conn := range []net.Conn{client, relayLeft, relayRight, target} {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	}
	defer client.Close()
	defer target.Close()

	relayDone := make(chan struct{})
	go func() {
		relay(relayLeft, &bufConn{Conn: relayRight, r: bufio.NewReader(relayRight)})
		close(relayDone)
	}()

	const request, response = "request-until-eof", "response-after-eof"
	if _, err := client.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	requestBody, err := io.ReadAll(target)
	if err != nil || string(requestBody) != request {
		t.Fatalf("target read = %q, %v; want propagated EOF after %q", requestBody, err, request)
	}
	if _, err := target.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
	if err := target.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(client)
	if err != nil || string(responseBody) != response {
		t.Fatalf("client read = %q, %v; want %q", responseBody, err, response)
	}
	select {
	case <-relayDone:
	case <-time.After(time.Second):
		t.Fatal("relay did not stop after both half-closes")
	}
}

func testTCPConnectionPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
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

func TestServerConnectionLimitRejectsAndReleasesSlots(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerWithCredentialsAndLimit("", NewProxyPool(), &ConfigStore{}, "", "", 1)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(listener) }()

	first, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	waitForConnectionSlots(t, server, 1)
	second, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := second.Read(one[:]); err == nil {
		t.Fatal("connection above the admission limit remained open")
	}
	_ = second.Close()
	_ = first.Close()
	waitForConnectionSlots(t, server, 0)

	third, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	waitForConnectionSlots(t, server, 1)
	_ = third.Close()
	waitForConnectionSlots(t, server, 0)
	_ = listener.Close()
	if err := <-serveDone; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("serve() shutdown error = %v, want net.ErrClosed", err)
	}
}

func waitForConnectionSlots(t *testing.T, server *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(server.connSlots) != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(server.connSlots); got != want {
		t.Fatalf("active connection slots = %d, want %d", got, want)
	}
}

type temporaryAcceptTestError struct{}

func (temporaryAcceptTestError) Error() string   { return "temporary accept failure" }
func (temporaryAcceptTestError) Timeout() bool   { return false }
func (temporaryAcceptTestError) Temporary() bool { return true }

type temporaryErrorListener struct {
	remaining atomic.Int32
	calls     atomic.Int32
}

func (l *temporaryErrorListener) Accept() (net.Conn, error) {
	l.calls.Add(1)
	if l.remaining.Add(-1) >= 0 {
		return nil, temporaryAcceptTestError{}
	}
	return nil, net.ErrClosed
}

func (l *temporaryErrorListener) Close() error   { return nil }
func (l *temporaryErrorListener) Addr() net.Addr { return testStaticAddr("temporary-listener") }

type testStaticAddr string

func (a testStaticAddr) Network() string { return "test" }
func (a testStaticAddr) String() string  { return string(a) }

func TestServerTemporaryAcceptErrorsBackOff(t *testing.T) {
	listener := &temporaryErrorListener{}
	listener.remaining.Store(2)
	server := NewServerWithCredentialsAndLimit("", NewProxyPool(), &ConfigStore{}, "", "", 1)
	started := time.Now()
	err := server.serve(listener)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("serve() error = %v, want net.ErrClosed", err)
	}
	if got := listener.calls.Load(); got != 3 {
		t.Fatalf("Accept calls = %d, want 3", got)
	}
	if elapsed := time.Since(started); elapsed < socksAcceptRetryInitialDelay*3 {
		t.Fatalf("temporary Accept retries spun too quickly: %s", elapsed)
	}
}

func TestServerShutdownClosesListenerAndActiveConnections(t *testing.T) {
	server := NewServerWithCredentialsAndLimit("127.0.0.1:0", NewProxyPool(), &ConfigStore{}, "", "", 2)
	startDone := make(chan error, 1)
	go func() { startDone <- server.Start() }()

	addr := waitForServerListener(t, server)
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	waitForConnectionSlots(t, server, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() with active long-lived client = %v, want deadline exceeded", err)
	}
	if err := <-startDone; err != nil {
		t.Fatalf("Start() after graceful shutdown = %v, want nil", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := client.Read(one[:]); err == nil {
		t.Fatal("active client remained open after Shutdown")
	}
	waitForConnectionSlots(t, server, 0)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := server.Shutdown(ctx2); err != nil {
		t.Fatalf("Shutdown() after forced clients exited = %v", err)
	}
}

func TestServerShutdownHonorsContextDeadline(t *testing.T) {
	server := NewServerWithCredentialsAndLimit("", NewProxyPool(), &ConfigStore{}, "", "", 1)
	tracked, peer := net.Pipe()
	defer peer.Close()
	if !server.registerActiveConnection(tracked) {
		t.Fatal("failed to register test connection")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := server.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Shutdown ignored context deadline: %s", elapsed)
	}

	// Complete the synthetic handler registration so the waiter spawned by
	// Shutdown can exit; a real serve goroutine does this in its defer path.
	server.unregisterActiveConnection(tracked)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := server.Shutdown(ctx2); err != nil {
		t.Fatalf("second Shutdown after active handler exit = %v", err)
	}
}

func waitForServerListener(t *testing.T, server *Server) string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		server.stateMu.Lock()
		listener := server.listener
		server.stateMu.Unlock()
		if listener != nil {
			return listener.Addr().String()
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("server listener did not start")
	return ""
}

func TestNegotiateUsernamePasswordReadsFragmentedAuthentication(t *testing.T) {
	const (
		username = "local-user"
		password = "correct horse battery staple"
	)

	wire := []byte{socks5Version, 2, socks5NoAuth, socks5UsernamePassword}
	wire = append(wire, socks5AuthVersion, byte(len(username)))
	wire = append(wire, username...)
	wire = append(wire, byte(len(password)))
	wire = append(wire, password...)
	rw := newFragmentedReadWriter(oneByteChunks(wire)...)

	if err := negotiateUsernamePassword(rw, username, password); err != nil {
		t.Fatalf("negotiateUsernamePassword() error = %v", err)
	}
	if got, want := rw.writes.Bytes(), []byte{
		socks5Version, socks5UsernamePassword,
		socks5AuthVersion, socks5AuthSucceeded,
	}; !bytes.Equal(got, want) {
		t.Fatalf("authentication replies = %v, want %v", got, want)
	}
}

func TestNegotiateUsernamePasswordRejectsWrongCredentials(t *testing.T) {
	const (
		expectedUser = "local-user"
		expectedPass = "correct-password"
		providedPass = "wrong-password"
	)

	wire := []byte{socks5Version, 1, socks5UsernamePassword, socks5AuthVersion, byte(len(expectedUser))}
	wire = append(wire, expectedUser...)
	wire = append(wire, byte(len(providedPass)))
	wire = append(wire, providedPass...)
	rw := newFragmentedReadWriter(oneByteChunks(wire)...)

	err := negotiateUsernamePassword(rw, expectedUser, expectedPass)
	if err == nil {
		t.Fatal("negotiateUsernamePassword() accepted incorrect credentials")
	}
	if strings.Contains(err.Error(), expectedPass) || strings.Contains(err.Error(), providedPass) {
		t.Fatalf("authentication error exposed credential material: %q", err)
	}
	if got, want := rw.writes.Bytes(), []byte{
		socks5Version, socks5UsernamePassword,
		socks5AuthVersion, socks5AuthFailure,
	}; !bytes.Equal(got, want) {
		t.Fatalf("authentication replies = %v, want %v", got, want)
	}
}

func TestNewServerWithCredentialsRequiresMethod02(t *testing.T) {
	server := NewServerWithCredentials("", nil, nil, "user", "password")
	if !server.requiresAuthentication() {
		t.Fatal("NewServerWithCredentials() did not require authentication")
	}

	rw := newFragmentedReadWriter(oneByteChunks([]byte{
		socks5Version, 1, socks5NoAuth,
	})...)

	if err := server.negotiate(rw); err == nil {
		t.Fatal("configured server accepted a client without method 0x02")
	}
	if got, want := rw.writes.Bytes(), []byte{socks5Version, socks5NoAcceptableMethods}; !bytes.Equal(got, want) {
		t.Fatalf("method reply = %v, want %v", got, want)
	}
}

func TestNewServerWithoutCredentialsKeepsNoAuthBehavior(t *testing.T) {
	server := NewServer("", nil, nil)
	if server.requiresAuthentication() {
		t.Fatal("NewServer() unexpectedly requires authentication")
	}

	// A legacy no-auth client still works even when it offers other methods
	// first, matching the behavior before optional credentials were added.
	rw := newFragmentedReadWriter(oneByteChunks([]byte{
		socks5Version, 2, socks5UsernamePassword, socks5NoAuth,
	})...)
	if err := server.negotiate(rw); err != nil {
		t.Fatalf("no-auth negotiation error = %v", err)
	}
	if got, want := rw.writes.Bytes(), []byte{socks5Version, socks5NoAuth}; !bytes.Equal(got, want) {
		t.Fatalf("method reply = %v, want %v", got, want)
	}
}

func TestConfigValidateRequiresCompleteSOCKSCredentialPair(t *testing.T) {
	base := Config{
		ListenAddr:     "127.0.0.1:1080",
		StatusAddr:     "127.0.0.1:8080",
		DataDir:        ".",
		ScrapeInterval: time.Minute,
		CheckTimeout:   time.Second,
		MaxConcurrent:  1,
		MaxCandidates:  1,
	}

	for _, tt := range []struct {
		name     string
		user     string
		password string
		wantErr  bool
	}{
		{name: "neither", wantErr: false},
		{name: "both", user: "user", password: "password", wantErr: false},
		{name: "username only", user: "user", wantErr: true},
		{name: "password only", password: "password", wantErr: true},
		{name: "username too long", user: strings.Repeat("u", 256), password: "password", wantErr: true},
		{name: "password too long", user: "user", password: strings.Repeat("p", 256), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.SOCKSUser = tt.user
			cfg.SOCKSPass = tt.password
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
