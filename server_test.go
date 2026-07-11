package main

import (
	"bytes"
	"io"
	"net"
	"strings"
	"sync"
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
