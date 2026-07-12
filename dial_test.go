package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stalledTestUpstream struct {
	Proxy       Proxy
	RequestSeen chan struct{}
	Closed      chan struct{}
	Active      atomic.Int64
}

func newStalledTestUpstream(t *testing.T, protocol string) *stalledTestUpstream {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	upstream := &stalledTestUpstream{
		Proxy:       Proxy{IP: host, Port: port, Protocol: protocol},
		RequestSeen: make(chan struct{}, 128),
		Closed:      make(chan struct{}, 128),
	}
	var handlers sync.WaitGroup
	var connections sync.Map
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(conn, struct{}{})
			upstream.Active.Add(1)
			handlers.Add(1)
			go func(conn net.Conn) {
				defer handlers.Done()
				defer upstream.Active.Add(-1)
				defer connections.Delete(conn)
				defer conn.Close()

				reader := bufio.NewReader(conn)
				switch protocol {
				case "socks5":
					var header [2]byte
					if _, err := io.ReadFull(reader, header[:]); err != nil {
						return
					}
					if _, err := io.ReadFull(reader, make([]byte, int(header[1]))); err != nil {
						return
					}
				case "http":
					for {
						line, err := reader.ReadString('\n')
						if err != nil {
							return
						}
						if line == "\r\n" {
							break
						}
					}
				default:
					return
				}
				upstream.RequestSeen <- struct{}{}
				// Deliberately never send the method/CONNECT response. The only
				// expected unblock is the client actively closing on cancellation.
				_, _ = reader.ReadByte()
				upstream.Closed <- struct{}{}
			}(conn)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		connections.Range(func(key, _ any) bool {
			_ = key.(net.Conn).Close()
			return true
		})
		<-acceptDone
		handlers.Wait()
	})
	return upstream
}

func TestDialHTTPConnectAcceptsAnySuccessful2xxResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, err = conn.Write([]byte("HTTP/1.1 204 No Content\r\n\r\n"))
		done <- err
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialHTTPConnect(Proxy{IP: host, Port: port, Protocol: "http"}, "example.test:443", time.Second)
	if err != nil {
		t.Fatalf("dialHTTPConnect() error = %v, want successful 204 CONNECT", err)
	}
	_ = conn.Close()
	if err := <-done; err != nil {
		t.Fatalf("test upstream error = %v", err)
	}
}

func TestDialUpstreamContextCancelsStalledHandshakeAndClosesConnection(t *testing.T) {
	for _, protocol := range []string{"http", "socks5"} {
		t.Run(protocol, func(t *testing.T) {
			upstream := newStalledTestUpstream(t, protocol)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				conn, err := DialUpstreamContext(ctx, upstream.Proxy, "example.test:443", 5*time.Second)
				if conn != nil {
					_ = conn.Close()
				}
				done <- err
			}()
			select {
			case <-upstream.RequestSeen:
			case <-time.After(time.Second):
				t.Fatal("upstream did not receive handshake")
			}

			canceledAt := time.Now()
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("DialUpstreamContext() error = %v, want context.Canceled", err)
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatal("dial did not return within 300ms of cancellation")
			}
			select {
			case <-upstream.Closed:
			case <-time.After(300 * time.Millisecond):
				t.Fatal("upstream did not observe connection close within 300ms")
			}
			if elapsed := time.Since(canceledAt); elapsed > 300*time.Millisecond {
				t.Fatalf("canceled handshake took %s", elapsed)
			}
			deadline := time.Now().Add(300 * time.Millisecond)
			for upstream.Active.Load() != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if active := upstream.Active.Load(); active != 0 {
				t.Fatalf("active stalled connections = %d, want 0", active)
			}
		})
	}
}

func TestDialUpstreamContextSuccessfulTunnelSurvivesInternalContextCleanup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var handlers sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func(conn net.Conn) {
				defer handlers.Done()
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
					return
				}
				_, _ = io.Copy(conn, reader)
			}(conn)
		}
	}()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	px := Proxy{IP: host, Port: port, Protocol: "http"}
	for i := 0; i < 25; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		conn, err := DialUpstreamContext(ctx, px, "example.test:443", time.Second)
		if err != nil {
			cancel()
			t.Fatalf("iteration %d dial error: %v", i, err)
		}
		// Cancellation after ownership transfer must not let an internal
		// watcher close the successfully returned tunnel.
		cancel()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		payload := []byte("ok")
		if _, err := conn.Write(payload); err != nil {
			conn.Close()
			t.Fatalf("iteration %d returned tunnel was closed after success: %v", i, err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, got); err != nil || string(got) != string(payload) {
			conn.Close()
			t.Fatalf("iteration %d echo = %q, %v", i, got, err)
		}
		_ = conn.Close()
	}
	_ = listener.Close()
	handlers.Wait()
}

func TestDialUpstreamClassifiesHTTPConnectFailures(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        string
		wantKind      UpstreamErrorKind
		affectsHealth bool
	}{
		{name: "target refusal", status: "403 Forbidden", wantKind: UpstreamErrorTarget},
		{name: "proxy authentication", status: "407 Proxy Authentication Required", wantKind: UpstreamErrorAuth, affectsHealth: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, acceptErr := listener.Accept()
				if acceptErr != nil {
					done <- acceptErr
					return
				}
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, readErr := reader.ReadString('\n')
					if readErr != nil {
						done <- readErr
						return
					}
					if line == "\r\n" {
						break
					}
				}
				_, writeErr := conn.Write([]byte("HTTP/1.1 " + test.status + "\r\nContent-Length: 0\r\n\r\n"))
				done <- writeErr
			}()

			host, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			conn, err := DialUpstream(Proxy{IP: host, Port: port, Protocol: "http"}, "example.test:443", time.Second)
			if conn != nil {
				_ = conn.Close()
			}
			var upstreamErr *UpstreamError
			if !errors.As(err, &upstreamErr) || upstreamErr.Kind != test.wantKind {
				t.Fatalf("DialUpstream() error = %#v, want kind %v", err, test.wantKind)
			}
			if got := upstreamFailureAffectsHealth(err); got != test.affectsHealth {
				t.Fatalf("upstreamFailureAffectsHealth() = %v, want %v", got, test.affectsHealth)
			}
			if err := <-done; err != nil {
				t.Fatalf("test upstream error: %v", err)
			}
		})
	}
}

func TestDialUpstreamClassifiesEndpointAndInputFailures(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddress := listener.Addr().String()
	_ = listener.Close()
	host, port, err := net.SplitHostPort(closedAddress)
	if err != nil {
		t.Fatal(err)
	}

	_, err = DialUpstream(Proxy{IP: host, Port: port, Protocol: "http"}, "example.test:443", 250*time.Millisecond)
	var connectErr *UpstreamError
	if !errors.As(err, &connectErr) || connectErr.Kind != UpstreamErrorConnect || !upstreamFailureAffectsHealth(err) {
		t.Fatalf("closed endpoint error = %#v, want health-affecting connect error", err)
	}

	_, err = DialUpstream(Proxy{Protocol: "http"}, "bad host\r\nX-Test: injected:443", time.Second)
	var targetErr *UpstreamError
	if !errors.As(err, &targetErr) || targetErr.Kind != UpstreamErrorTarget || upstreamFailureAffectsHealth(err) {
		t.Fatalf("invalid target error = %#v, want non-health target error", err)
	}

	_, err = DialUpstream(Proxy{Protocol: "proxyip"}, "example.test:443", time.Second)
	var protocolErr *UpstreamError
	if !errors.As(err, &protocolErr) || protocolErr.Kind != UpstreamErrorProtocol || !upstreamFailureAffectsHealth(err) {
		t.Fatalf("unsupported protocol error = %#v, want health-affecting protocol error", err)
	}
}

func TestDialCredentialCandidatesRetriesOnlyAuthenticationFailuresAndPromotes(t *testing.T) {
	proxy := Proxy{
		IP: "192.0.2.80", Port: "8080", Protocol: "http",
		Username: "old", Password: "wrong",
		CredentialAlternates: []ProxyCredential{
			{Username: "also-wrong", Password: "bad"},
			{Username: "working", Password: "secret"},
		},
	}
	var attempted []ProxyCredential
	var budgets []time.Duration
	attempt := func(_ context.Context, candidate Proxy, _ string, budget time.Duration) (net.Conn, error) {
		attempted = append(attempted, ProxyCredential{Username: candidate.Username, Password: candidate.Password})
		budgets = append(budgets, budget)
		if candidate.Username != "working" {
			return nil, newUpstreamError(UpstreamErrorAuth, "test auth", errors.New("rejected"))
		}
		client, peer := net.Pipe()
		_ = peer.Close()
		return client, nil
	}

	conn, verified, err := dialUpstreamCredentialCandidatesContext(context.Background(), proxy, "example.test:443", 600*time.Millisecond, attempt)
	if err != nil {
		t.Fatalf("credential candidate dial error = %v", err)
	}
	_ = conn.Close()
	if len(attempted) != 3 {
		t.Fatalf("credential attempts = %#v, want all three declarations", attempted)
	}
	if verified.Username != "working" || verified.Password != "secret" {
		t.Fatalf("promoted credential = %q/%q", verified.Username, verified.Password)
	}
	if len(verified.CredentialAlternates) != 2 || verified.CredentialAlternates[0].Username != "old" || verified.CredentialAlternates[1].Username != "also-wrong" {
		t.Fatalf("promoted alternatives = %#v", verified.CredentialAlternates)
	}
	for index, budget := range budgets {
		if budget <= 0 || budget > 600*time.Millisecond {
			t.Fatalf("attempt %d budget = %s, want a bounded share of the 600ms total", index, budget)
		}
		if index > 0 && budget < budgets[index-1] {
			t.Fatalf("attempt budgets = %v, later declarations must not receive less time", budgets)
		}
	}
}

func TestDialCredentialCandidatesStopsOnNonAuthenticationFailure(t *testing.T) {
	proxy := Proxy{
		IP: "192.0.2.81", Port: "8080", Protocol: "http",
		Username: "primary", Password: "secret",
		CredentialAlternates: []ProxyCredential{{Username: "must-not-run", Password: "secret"}},
	}
	attempts := 0
	attempt := func(_ context.Context, _ Proxy, _ string, _ time.Duration) (net.Conn, error) {
		attempts++
		return nil, newUpstreamError(UpstreamErrorTarget, "test target", errors.New("forbidden"))
	}

	_, verified, err := dialUpstreamCredentialCandidatesContext(context.Background(), proxy, "example.test:443", time.Second, attempt)
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.Kind != UpstreamErrorTarget {
		t.Fatalf("candidate dial error = %#v, want target failure", err)
	}
	if attempts != 1 {
		t.Fatalf("non-authentication failure tried %d credentials, want 1", attempts)
	}
	if verified.Username != proxy.Username || verified.Password != proxy.Password {
		t.Fatalf("failed dial changed credential: %+v", verified)
	}
}

func TestDialCredentialCandidatesSharesOneBoundedDeadline(t *testing.T) {
	proxy := Proxy{
		IP: "192.0.2.82", Port: "8080", Protocol: "http",
		Username: "one", Password: "bad",
		CredentialAlternates: []ProxyCredential{
			{Username: "two", Password: "bad"},
			{Username: "three", Password: "bad"},
		},
	}
	attempts := 0
	attempt := func(ctx context.Context, _ Proxy, _ string, _ time.Duration) (net.Conn, error) {
		attempts++
		<-ctx.Done()
		return nil, newUpstreamError(UpstreamErrorAuth, "test stalled auth", ctx.Err())
	}
	started := time.Now()
	_, _, err := dialUpstreamCredentialCandidatesContext(context.Background(), proxy, "example.test:443", 300*time.Millisecond, attempt)
	elapsed := time.Since(started)
	if !isUpstreamAuthenticationFailure(err) {
		t.Fatalf("bounded credential dial error = %#v, want auth failure", err)
	}
	if attempts != 3 {
		t.Fatalf("stalled credential attempts = %d, want every fair-share attempt", attempts)
	}
	if elapsed < 250*time.Millisecond || elapsed > 550*time.Millisecond {
		t.Fatalf("credential attempts elapsed = %s, want one bounded 300ms total", elapsed)
	}
}

func TestValidateUpstreamTargetRejectsTextInjectionAndNonDecimalPorts(t *testing.T) {
	for _, target := range []string{
		"example.test:0",
		"example.test:+443",
		"example.test:-1",
		"example.test:65536",
		"example .test:443",
		"example.test\r\nX-Injected: yes:443",
		"[fe80::1%eth0]:443",
	} {
		t.Run(target, func(t *testing.T) {
			if err := validateUpstreamTarget(target); err == nil {
				t.Fatalf("validateUpstreamTarget(%q) unexpectedly succeeded", target)
			}
		})
	}
	for _, target := range []string{"example.test:443", "Example.TEST.:65535", "192.0.2.1:80", "[2001:db8::1]:443"} {
		t.Run("valid "+target, func(t *testing.T) {
			if err := validateUpstreamTarget(target); err != nil {
				t.Fatalf("validateUpstreamTarget(%q) = %v", target, err)
			}
		})
	}
}
