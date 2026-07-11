package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DialUpstream establishes a raw tunnel to target through the given upstream
// proxy. It preserves the historical timeout-only API for callers that do not
// have a request context.
func DialUpstream(px Proxy, target string, timeout time.Duration) (net.Conn, error) {
	return DialUpstreamContext(context.Background(), px, target, timeout)
}

// DialUpstreamContext is the cancellation-aware form. Context cancellation
// interrupts both the TCP dial and every blocking SOCKS/HTTP handshake read or
// write; no detached handshake is left running after this function returns.
func DialUpstreamContext(parent context.Context, px Proxy, target string, timeout time.Duration) (net.Conn, error) {
	if parent == nil {
		parent = context.Background()
	}
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	defer cancel()

	switch px.Protocol {
	case "socks5":
		return dialSOCKS5Context(ctx, px, target)
	case "http", "https":
		return dialHTTPConnectContext(ctx, px, target)
	default:
		return nil, fmt.Errorf("protocol %q cannot be used as a forwarding upstream", px.Protocol)
	}
}

func dialSOCKS5(px Proxy, target string, timeout time.Duration) (net.Conn, error) {
	return DialUpstreamContext(context.Background(), px, target, timeout)
}

func dialSOCKS5Context(ctx context.Context, px Proxy, target string) (result net.Conn, err error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", px.Addr())
	if err != nil {
		return nil, err
	}
	stopCancellationWatch := watchUpstreamHandshake(ctx, conn)
	watchStopped := false
	defer func() {
		if !watchStopped {
			stopCancellationWatch()
		}
		if err != nil {
			_ = conn.Close()
		}
	}()
	setUpstreamHandshakeDeadline(ctx, conn)

	methods := []byte{0x00}
	if px.Username != "" {
		methods = append(methods, 0x02)
	}
	greeting := append([]byte{socks5Version, byte(len(methods))}, methods...)
	if _, writeErr := conn.Write(greeting); writeErr != nil {
		return nil, upstreamHandshakeError(ctx, writeErr)
	}

	buf := make([]byte, 2)
	if _, readErr := io.ReadFull(conn, buf); readErr != nil {
		return nil, upstreamHandshakeError(ctx, readErr)
	}
	if buf[0] != socks5Version {
		return nil, fmt.Errorf("not socks5")
	}

	switch buf[1] {
	case 0x00:
		// no auth required
	case 0x02:
		if px.Username == "" {
			return nil, fmt.Errorf("upstream requires auth, none configured")
		}
		authReq := []byte{0x01, byte(len(px.Username))}
		authReq = append(authReq, []byte(px.Username)...)
		authReq = append(authReq, byte(len(px.Password)))
		authReq = append(authReq, []byte(px.Password)...)
		if _, writeErr := conn.Write(authReq); writeErr != nil {
			return nil, upstreamHandshakeError(ctx, writeErr)
		}
		authResp := make([]byte, 2)
		if _, readErr := io.ReadFull(conn, authResp); readErr != nil {
			return nil, upstreamHandshakeError(ctx, readErr)
		}
		if authResp[1] != 0x00 {
			return nil, fmt.Errorf("upstream rejected auth")
		}
	case 0xFF:
		return nil, fmt.Errorf("upstream has no acceptable auth method")
	default:
		return nil, fmt.Errorf("unsupported auth method: %d", buf[1])
	}

	host, portStr, splitErr := net.SplitHostPort(target)
	if splitErr != nil {
		return nil, splitErr
	}
	var port int
	if _, scanErr := fmt.Sscanf(portStr, "%d", &port); scanErr != nil {
		return nil, scanErr
	}

	req := []byte{socks5Version, cmdConnect, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, atypIPv4)
			req = append(req, ip4...)
		} else {
			req = append(req, atypIPv6)
			req = append(req, ip...)
		}
	} else {
		req = append(req, atypDomain, byte(len(host)))
		req = append(req, []byte(host)...)
	}
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, writeErr := conn.Write(req); writeErr != nil {
		return nil, upstreamHandshakeError(ctx, writeErr)
	}

	// The CONNECT reply length depends on ATYP. Read the fixed header first,
	// then exactly the advertised address and port so no reply bytes leak into
	// the returned tunnel.
	header := make([]byte, 4)
	if _, readErr := io.ReadFull(conn, header); readErr != nil {
		return nil, upstreamHandshakeError(ctx, readErr)
	}
	if header[1] != 0x00 {
		return nil, fmt.Errorf("upstream connect failed, status: %d", header[1])
	}

	var addrLen int
	switch header[3] {
	case atypIPv4:
		addrLen = net.IPv4len
	case atypIPv6:
		addrLen = net.IPv6len
	case atypDomain:
		lenByte := make([]byte, 1)
		if _, readErr := io.ReadFull(conn, lenByte); readErr != nil {
			return nil, upstreamHandshakeError(ctx, readErr)
		}
		addrLen = int(lenByte[0])
	default:
		return nil, fmt.Errorf("upstream connect reply: unknown address type %d", header[3])
	}
	if _, readErr := io.ReadFull(conn, make([]byte, addrLen+2)); readErr != nil {
		return nil, upstreamHandshakeError(ctx, readErr)
	}
	// Stop and join the watcher before the final context check. Once joined,
	// cancellation cannot race with a successful return and close a tunnel
	// that we hand to the caller with a nil error.
	stopCancellationWatch()
	watchStopped = true
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// dialHTTPConnect tunnels to target via the HTTP CONNECT method. Used for
// both "http" and "https" tagged upstreams: the tag reflects what the
// source list advertised about the proxy's capability, not the wire
// protocol spoken to reach it (both use plain HTTP CONNECT).
func dialHTTPConnect(px Proxy, target string, timeout time.Duration) (net.Conn, error) {
	return DialUpstreamContext(context.Background(), px, target, timeout)
}

func dialHTTPConnectContext(ctx context.Context, px Proxy, target string) (result net.Conn, err error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", px.Addr())
	if err != nil {
		return nil, err
	}
	stopCancellationWatch := watchUpstreamHandshake(ctx, conn)
	watchStopped := false
	defer func() {
		if !watchStopped {
			stopCancellationWatch()
		}
		if err != nil {
			_ = conn.Close()
		}
	}()
	setUpstreamHandshakeDeadline(ctx, conn)

	var sb strings.Builder
	fmt.Fprintf(&sb, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if px.Username != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(px.Username + ":" + px.Password))
		fmt.Fprintf(&sb, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	sb.WriteString("\r\n")
	if _, writeErr := conn.Write([]byte(sb.String())); writeErr != nil {
		return nil, upstreamHandshakeError(ctx, writeErr)
	}

	// bufio.Reader may buffer tunneled bytes following the CONNECT response.
	// bufConn keeps draining it first so those bytes are not lost.
	br := bufio.NewReader(conn)
	resp, readErr := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if readErr != nil {
		return nil, upstreamHandshakeError(ctx, readErr)
	}
	_ = resp.Body.Close()
	// RFC 9110 defines any 2xx response as a successful CONNECT tunnel.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("upstream CONNECT failed: %s", resp.Status)
	}
	stopCancellationWatch()
	watchStopped = true
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufConn{Conn: conn, r: br}, nil
}

// watchUpstreamHandshake actively closes conn when ctx is canceled, which
// unblocks protocol reads/writes immediately. The returned stop function waits
// for the watcher to exit; therefore canceling an internal timeout after a
// successful return cannot race and close the caller-owned tunnel.
func watchUpstreamHandshake(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

func setUpstreamHandshakeDeadline(ctx context.Context, conn net.Conn) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
}

func upstreamHandshakeError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// bufConn is a net.Conn whose reads are served from a bufio.Reader first -
// used so bytes already buffered while parsing a preceding protocol exchange
// aren't lost when the raw connection is returned.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
}
