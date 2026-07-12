package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UpstreamErrorKind separates failures of the configured upstream itself from
// target-specific refusals. A client asking for a closed or policy-blocked
// destination must not globally mark an otherwise healthy proxy unavailable.
type UpstreamErrorKind uint8

const (
	UpstreamErrorUnknown UpstreamErrorKind = iota
	UpstreamErrorConnect
	UpstreamErrorAuth
	UpstreamErrorProtocol
	UpstreamErrorTarget
)

// UpstreamError is returned by every failed DialUpstream handshake. Err stays
// in the unwrap chain so context cancellation/deadline checks remain intact.
type UpstreamError struct {
	Kind UpstreamErrorKind
	Op   string
	Err  error
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return "upstream error"
	}
	if e.Op == "" {
		return fmt.Sprint(e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *UpstreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func newUpstreamError(kind UpstreamErrorKind, op string, err error) error {
	if err == nil {
		err = fmt.Errorf("upstream operation failed")
	}
	return &UpstreamError{Kind: kind, Op: op, Err: err}
}

// upstreamFailureAffectsHealth reports whether the proxy endpoint itself is at
// fault. Unknown errors fail safe: they affect only the current client request.
func upstreamFailureAffectsHealth(err error) bool {
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	switch upstreamErr.Kind {
	case UpstreamErrorConnect, UpstreamErrorAuth, UpstreamErrorProtocol:
		return true
	default:
		return false
	}
}

func isUpstreamAuthenticationFailure(err error) bool {
	var upstreamErr *UpstreamError
	return errors.As(err, &upstreamErr) && upstreamErr.Kind == UpstreamErrorAuth
}

// DialUpstream establishes a raw tunnel to target through the given upstream
// proxy. It preserves the historical timeout-only API for callers that do not
// have a request context.
func DialUpstream(px Proxy, target string, timeout time.Duration) (net.Conn, error) {
	return DialUpstreamContext(context.Background(), px, target, timeout)
}

type upstreamCredentialDialAttempt func(context.Context, Proxy, string, time.Duration) (net.Conn, error)

// DialUpstreamCredentialCandidatesContext retries the bounded credential set
// only after a definitive upstream-authentication failure. Connection,
// protocol, cancellation, and target-specific failures stop immediately: a
// different password cannot repair those conditions and blindly traversing
// every declaration would multiply latency and load.
//
// All attempts share one deadline. Dividing the remaining time by the number
// of declarations left prevents a stalled authentication exchange from
// starving a later valid credential. The successful declaration is promoted
// in the returned Proxy; callers that own a ProxyPool can persist it with
// UpdateVerifiedCredentialsAtGeneration.
func DialUpstreamCredentialCandidatesContext(parent context.Context, px Proxy, target string, timeout time.Duration) (net.Conn, Proxy, error) {
	return dialUpstreamCredentialCandidatesContext(parent, px, target, timeout, DialUpstreamContext)
}

func dialUpstreamCredentialCandidatesContext(parent context.Context, px Proxy, target string, timeout time.Duration, attempt upstreamCredentialDialAttempt) (net.Conn, Proxy, error) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return nil, px, newUpstreamError(UpstreamErrorProtocol, "select credential budget", fmt.Errorf("credential retry timeout must be positive"))
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	candidates := px.credentialCandidates()
	var lastErr error
	for index, candidate := range candidates {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if lastErr != nil {
				return nil, px, lastErr
			}
			return nil, px, ctxErr
		}
		attemptsLeft := len(candidates) - index
		attemptBudget := time.Until(deadline) / time.Duration(attemptsLeft)
		if attemptBudget <= 0 {
			break
		}
		attemptContext, cancelAttempt := context.WithTimeout(ctx, attemptBudget)
		conn, err := attempt(attemptContext, candidate, target, attemptBudget)
		cancelAttempt()
		if err == nil {
			return conn, px.promoteCredential(candidate), nil
		}
		lastErr = err
		if !isUpstreamAuthenticationFailure(err) {
			return nil, px, err
		}
	}
	if lastErr != nil {
		return nil, px, lastErr
	}
	return nil, px, context.DeadlineExceeded
}

// credentialCandidateDialer adapts the retry helper to net/http.Transport and
// remembers which declaration established the tunnel. It is safe for the
// transport to invoke DialContext concurrently, even though current health and
// speed requests normally need only one connection.
type credentialCandidateDialer struct {
	proxy   Proxy
	timeout time.Duration

	mu       sync.Mutex
	verified Proxy
	found    bool
}

func newCredentialCandidateDialer(px Proxy, timeout time.Duration) *credentialCandidateDialer {
	return &credentialCandidateDialer{proxy: px, timeout: timeout}
}

func (d *credentialCandidateDialer) DialContext(ctx context.Context, _, target string) (net.Conn, error) {
	conn, verified, err := DialUpstreamCredentialCandidatesContext(ctx, d.proxy, target, d.timeout)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.verified = verified
	d.found = true
	d.mu.Unlock()
	return conn, nil
}

func (d *credentialCandidateDialer) Verified() (Proxy, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.found {
		return Proxy{}, false
	}
	return cloneProxy(d.verified), true
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
	case "socks5", "http", "https":
		// Valid forwarding protocol; validate the authority below before putting it
		// on either a length-framed SOCKS request or a text HTTP CONNECT request.
	default:
		return nil, newUpstreamError(UpstreamErrorProtocol, "select upstream protocol", fmt.Errorf("protocol %q cannot be used as a forwarding upstream", px.Protocol))
	}
	if err := validateUpstreamTarget(target); err != nil {
		return nil, newUpstreamError(UpstreamErrorTarget, "validate target", err)
	}

	switch px.Protocol {
	case "socks5":
		return dialSOCKS5Context(ctx, px, target)
	case "http", "https":
		return dialHTTPConnectContext(ctx, px, target)
	}
	return nil, newUpstreamError(UpstreamErrorProtocol, "select upstream protocol", fmt.Errorf("protocol %q cannot be used as a forwarding upstream", px.Protocol))
}

func validateUpstreamTarget(target string) error {
	if strings.TrimSpace(target) != target {
		return fmt.Errorf("target contains surrounding whitespace")
	}
	host, rawPort, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return fmt.Errorf("target must be a host:port authority")
	}
	if rawPort == "" {
		return fmt.Errorf("target port must be between 1 and 65535")
	}
	for _, char := range rawPort {
		if char < '0' || char > '9' {
			return fmt.Errorf("target port must contain decimal digits only")
		}
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("target port must be between 1 and 65535")
	}
	if ip := net.ParseIP(host); ip == nil && !validProxyHostname(host) {
		return fmt.Errorf("target host is not a valid IP address or DNS name")
	}
	return nil
}

func dialSOCKS5(px Proxy, target string, timeout time.Duration) (net.Conn, error) {
	return DialUpstreamContext(context.Background(), px, target, timeout)
}

func dialSOCKS5Context(ctx context.Context, px Proxy, target string) (result net.Conn, err error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", px.Addr())
	if err != nil {
		return nil, newUpstreamError(UpstreamErrorConnect, "connect to SOCKS5 upstream", err)
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
		return nil, newUpstreamError(UpstreamErrorProtocol, "write SOCKS5 greeting", upstreamHandshakeError(ctx, writeErr))
	}

	buf := make([]byte, 2)
	if _, readErr := io.ReadFull(conn, buf); readErr != nil {
		return nil, newUpstreamError(UpstreamErrorProtocol, "read SOCKS5 method", upstreamHandshakeError(ctx, readErr))
	}
	if buf[0] != socks5Version {
		return nil, newUpstreamError(UpstreamErrorProtocol, "read SOCKS5 method", fmt.Errorf("unexpected version %d", buf[0]))
	}

	switch buf[1] {
	case 0x00:
		// no auth required
	case 0x02:
		if px.Username == "" {
			return nil, newUpstreamError(UpstreamErrorAuth, "authenticate SOCKS5 upstream", fmt.Errorf("upstream requires auth, none configured"))
		}
		if len(px.Username) > 255 || len(px.Password) > 255 {
			return nil, newUpstreamError(UpstreamErrorAuth, "authenticate SOCKS5 upstream", fmt.Errorf("upstream credentials exceed 255 bytes"))
		}
		authReq := []byte{0x01, byte(len(px.Username))}
		authReq = append(authReq, []byte(px.Username)...)
		authReq = append(authReq, byte(len(px.Password)))
		authReq = append(authReq, []byte(px.Password)...)
		if _, writeErr := conn.Write(authReq); writeErr != nil {
			return nil, newUpstreamError(UpstreamErrorAuth, "write SOCKS5 authentication", upstreamHandshakeError(ctx, writeErr))
		}
		authResp := make([]byte, 2)
		if _, readErr := io.ReadFull(conn, authResp); readErr != nil {
			return nil, newUpstreamError(UpstreamErrorAuth, "read SOCKS5 authentication", upstreamHandshakeError(ctx, readErr))
		}
		if authResp[0] != socks5AuthVersion {
			return nil, newUpstreamError(UpstreamErrorProtocol, "read SOCKS5 authentication", fmt.Errorf("unexpected authentication version %d", authResp[0]))
		}
		if authResp[1] != 0x00 {
			return nil, newUpstreamError(UpstreamErrorAuth, "authenticate SOCKS5 upstream", fmt.Errorf("upstream rejected auth"))
		}
	case 0xFF:
		return nil, newUpstreamError(UpstreamErrorAuth, "authenticate SOCKS5 upstream", fmt.Errorf("upstream has no acceptable auth method"))
	default:
		return nil, newUpstreamError(UpstreamErrorProtocol, "read SOCKS5 method", fmt.Errorf("unsupported auth method: %d", buf[1]))
	}

	host, portStr, splitErr := net.SplitHostPort(target)
	if splitErr != nil {
		return nil, newUpstreamError(UpstreamErrorTarget, "parse SOCKS5 target", splitErr)
	}
	parsedPort, scanErr := strconv.ParseUint(portStr, 10, 16)
	if scanErr != nil || parsedPort == 0 {
		return nil, newUpstreamError(UpstreamErrorTarget, "parse SOCKS5 target", fmt.Errorf("invalid target port"))
	}
	port := int(parsedPort)

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
		return nil, newUpstreamError(UpstreamErrorProtocol, "write SOCKS5 CONNECT", upstreamHandshakeError(ctx, writeErr))
	}

	// The CONNECT reply length depends on ATYP. Read the fixed header first,
	// then exactly the advertised address and port so no reply bytes leak into
	// the returned tunnel.
	header := make([]byte, 4)
	if _, readErr := io.ReadFull(conn, header); readErr != nil {
		return nil, newUpstreamError(UpstreamErrorProtocol, "read SOCKS5 CONNECT reply", upstreamHandshakeError(ctx, readErr))
	}
	if header[0] != socks5Version || header[2] != 0x00 {
		return nil, newUpstreamError(UpstreamErrorProtocol, "read SOCKS5 CONNECT reply", fmt.Errorf("malformed reply header"))
	}
	if header[1] != 0x00 {
		return nil, newUpstreamError(UpstreamErrorTarget, "connect SOCKS5 target", fmt.Errorf("upstream status %d", header[1]))
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
			return nil, newUpstreamError(UpstreamErrorProtocol, "read SOCKS5 CONNECT reply", upstreamHandshakeError(ctx, readErr))
		}
		addrLen = int(lenByte[0])
	default:
		return nil, newUpstreamError(UpstreamErrorProtocol, "read SOCKS5 CONNECT reply", fmt.Errorf("unknown address type %d", header[3]))
	}
	if _, readErr := io.ReadFull(conn, make([]byte, addrLen+2)); readErr != nil {
		return nil, newUpstreamError(UpstreamErrorProtocol, "read SOCKS5 CONNECT reply", upstreamHandshakeError(ctx, readErr))
	}
	// Stop and join the watcher before the final context check. Once joined,
	// cancellation cannot race with a successful return and close a tunnel
	// that we hand to the caller with a nil error.
	stopCancellationWatch()
	watchStopped = true
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, newUpstreamError(UpstreamErrorProtocol, "complete SOCKS5 handshake", ctxErr)
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
		return nil, newUpstreamError(UpstreamErrorConnect, "connect to HTTP upstream", err)
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
		return nil, newUpstreamError(UpstreamErrorProtocol, "write HTTP CONNECT", upstreamHandshakeError(ctx, writeErr))
	}

	// bufio.Reader may buffer tunneled bytes following the CONNECT response.
	// bufConn keeps draining it first so those bytes are not lost.
	br := bufio.NewReader(conn)
	resp, readErr := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if readErr != nil {
		return nil, newUpstreamError(UpstreamErrorProtocol, "read HTTP CONNECT response", upstreamHandshakeError(ctx, readErr))
	}
	_ = resp.Body.Close()
	// RFC 9110 defines any 2xx response as a successful CONNECT tunnel.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		kind := UpstreamErrorTarget
		if resp.StatusCode == http.StatusProxyAuthRequired {
			kind = UpstreamErrorAuth
		}
		return nil, newUpstreamError(kind, "connect HTTP target", fmt.Errorf("upstream CONNECT failed: %s", resp.Status))
	}
	stopCancellationWatch()
	watchStopped = true
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, newUpstreamError(UpstreamErrorProtocol, "complete HTTP CONNECT handshake", ctxErr)
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

func (b *bufConn) CloseWrite() error {
	if conn, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return fmt.Errorf("underlying connection does not support CloseWrite")
}

func (b *bufConn) CloseRead() error {
	if conn, ok := b.Conn.(interface{ CloseRead() error }); ok {
		return conn.CloseRead()
	}
	return fmt.Errorf("underlying connection does not support CloseRead")
}
