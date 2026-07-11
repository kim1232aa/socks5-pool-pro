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
