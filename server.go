package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	socks5Version             = 0x05
	socks5NoAuth              = 0x00
	socks5UsernamePassword    = 0x02
	socks5NoAcceptableMethods = 0xff
	socks5AuthVersion         = 0x01 // RFC 1929
	socks5AuthSucceeded       = 0x00
	socks5AuthFailure         = 0x01
	cmdConnect                = 0x01
	atypIPv4                  = 0x01
	atypDomain                = 0x03
	atypIPv6                  = 0x04

	replySucceeded               = 0x00
	replyGeneralFailure          = 0x01
	replyNetworkUnreachable      = 0x03
	replyHostUnreachable         = 0x04
	replyConnectionRefused       = 0x05
	replyTTLExpired              = 0x06
	replyCommandNotSupported     = 0x07
	replyAddressTypeNotSupported = 0x08

	socks5HandshakeTimeout = 10 * time.Second

	defaultSOCKSMaxClientConnections = 512
	socksAcceptRetryInitialDelay     = 5 * time.Millisecond
	socksAcceptRetryMaxDelay         = time.Second
)

// Server is the local SOCKS5 endpoint that clients connect to. Every
// incoming request is routed through the configured rules to a Group (or
// DIRECT), then forwarded via that group's chosen upstream.
type Server struct {
	listenAddr  string
	pool        *ProxyPool
	store       *ConfigStore
	socksUser   string
	socksPass   string
	connSlots   chan struct{}
	routePolicy *listenerRoutePolicy
	onStop      func(error)

	stateMu        sync.Mutex
	listener       net.Listener
	activeConns    map[net.Conn]struct{}
	activeWG       sync.WaitGroup
	drainOnce      sync.Once
	drainCh        chan struct{}
	shuttingDown   bool
	shutdownCtx    context.Context
	cancelShutdown context.CancelFunc
}

// listenerRoutePolicy selects the route behavior for an individual SOCKS
// listener. A nil policy preserves the original global-rules behavior. The
// group field is a transient, per-listener Group whose ID/Name is
// "listener:<binding.ID>" so each port keeps its own sticky/round-robin
// cursor in the pool's groupState, independent of other listeners that
// target the same logical group. direct short-circuits to a direct dial
// (group mode bound to DIRECT) and never touches the pool.
type listenerRoutePolicy struct {
	mode        string
	direct      bool
	targetGroup string
	group       Group
}

func NewServer(listenAddr string, pool *ProxyPool, store *ConfigStore) *Server {
	return NewServerWithCredentials(listenAddr, pool, store, "", "")
}

// NewServerWithCredentials creates a local SOCKS5 endpoint with optional RFC
// 1929 username/password authentication. Callers must provide both values or
// neither; Config.Validate enforces that for the command-line server. Keeping
// NewServer above preserves the no-auth constructor used by existing callers.
func NewServerWithCredentials(listenAddr string, pool *ProxyPool, store *ConfigStore, socksUser, socksPass string) *Server {
	return NewServerWithCredentialsAndLimit(listenAddr, pool, store, socksUser, socksPass, defaultSOCKSMaxClientConnections)
}

// NewServerWithCredentialsAndLimit preserves the existing constructors while
// allowing the process configuration to set an explicit admission limit. A
// non-positive direct-call value falls back to the safe default.
func NewServerWithCredentialsAndLimit(listenAddr string, pool *ProxyPool, store *ConfigStore, socksUser, socksPass string, maxConnections int) *Server {
	if maxConnections <= 0 {
		maxConnections = defaultSOCKSMaxClientConnections
	}
	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	return &Server{
		listenAddr:     listenAddr,
		pool:           pool,
		store:          store,
		socksUser:      socksUser,
		socksPass:      socksPass,
		connSlots:      make(chan struct{}, maxConnections),
		activeConns:    make(map[net.Conn]struct{}),
		drainCh:        make(chan struct{}),
		shutdownCtx:    shutdownCtx,
		cancelShutdown: cancelShutdown,
	}
}

// NewServerWithSharedAdmissionAndPolicy constructs an additional listener.
// The caller owns the listener lifecycle; a shared admission channel makes
// all listeners observe one process-wide connection cap.
func NewServerWithSharedAdmissionAndPolicy(listenAddr string, pool *ProxyPool, store *ConfigStore, socksUser, socksPass string, slots chan struct{}, policy *listenerRoutePolicy) *Server {
	s := NewServerWithCredentialsAndLimit(listenAddr, pool, store, socksUser, socksPass, cap(slots))
	if slots != nil {
		s.connSlots = slots
	}
	s.routePolicy = policy
	return s
}

func (s *Server) setRoutePolicy(policy *listenerRoutePolicy) {
	s.stateMu.Lock()
	s.routePolicy = policy
	s.stateMu.Unlock()
}

func (s *Server) currentRoutePolicy() *listenerRoutePolicy {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.routePolicy
}

func effectiveListenerGroup(policy *listenerRoutePolicy, groups []Group) Group {
	effective := policy.group
	if policy.mode != ListenerModeGroup || policy.targetGroup == "" {
		return effective
	}
	for _, configured := range groups {
		if configured.Name == policy.targetGroup || configured.ID == policy.targetGroup {
			configured.ID, configured.Name = policy.group.ID, policy.group.Name
			return configured
		}
	}
	return effective
}

func (s *Server) setStopCallback(callback func(error)) {
	s.stateMu.Lock()
	s.onStop = callback
	s.stateMu.Unlock()
}

// StartListener takes ownership of an already-bound listener. Registration is
// synchronous, so callers can persist runtime state only after the port is
// known to be reserved. Serving continues in the background until Shutdown.
func (s *Server) StartListener(ln net.Listener) error {
	if ln == nil {
		return fmt.Errorf("listener is required")
	}
	s.stateMu.Lock()
	if s.shuttingDown {
		s.stateMu.Unlock()
		_ = ln.Close()
		return net.ErrClosed
	}
	if s.listener != nil {
		s.stateMu.Unlock()
		return fmt.Errorf("SOCKS5 server is already running")
	}
	s.listener = ln
	s.stateMu.Unlock()
	log.Printf("[server] SOCKS5 proxy listening on %s", ln.Addr())
	go func() {
		err := s.serve(ln)
		_ = ln.Close()
		s.stateMu.Lock()
		if s.listener == ln {
			s.listener = nil
		}
		shuttingDown := s.shuttingDown
		callback := s.onStop
		s.stateMu.Unlock()
		if errors.Is(err, net.ErrClosed) && shuttingDown {
			err = nil
		}
		if err != nil {
			log.Printf("[server] listener %s stopped: %v", ln.Addr(), err)
		}
		if callback != nil {
			callback(err)
		}
	}()
	return nil
}

func (s *Server) Start() error {
	s.stateMu.Lock()
	if s.shuttingDown {
		s.stateMu.Unlock()
		return nil
	}
	if s.listener != nil {
		s.stateMu.Unlock()
		return fmt.Errorf("SOCKS5 server is already running")
	}
	s.stateMu.Unlock()

	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	defer ln.Close()

	s.stateMu.Lock()
	if s.shuttingDown {
		s.stateMu.Unlock()
		return nil
	}
	if s.listener != nil {
		s.stateMu.Unlock()
		return fmt.Errorf("SOCKS5 server is already running")
	}
	s.listener = ln
	s.stateMu.Unlock()
	defer func() {
		s.stateMu.Lock()
		if s.listener == ln {
			s.listener = nil
		}
		s.stateMu.Unlock()
	}()

	log.Printf("[server] SOCKS5 proxy listening on %s", s.listenAddr)
	err = s.serve(ln)
	if errors.Is(err, net.ErrClosed) && s.isShuttingDown() {
		return nil
	}
	return err
}

// Shutdown stops admission and immediately cancels this Server generation's
// outbound dials and upstream handshakes. Existing relays may finish naturally
// until ctx expires; at that point every tracked connection is force-closed.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.stateMu.Lock()
	if !s.shuttingDown {
		s.shuttingDown = true
		if s.cancelShutdown != nil {
			s.cancelShutdown()
		}
	}
	ln := s.listener
	s.stateMu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}

	s.drainOnce.Do(func() {
		go func() {
			s.activeWG.Wait()
			close(s.drainCh)
		}()
	})
	select {
	case <-s.drainCh:
		return nil
	case <-ctx.Done():
		s.closeActiveConnections()
		return ctx.Err()
	}
}

func (s *Server) closeActiveConnections() {
	s.stateMu.Lock()
	connections := make([]net.Conn, 0, len(s.activeConns))
	for conn := range s.activeConns {
		connections = append(connections, conn)
	}
	s.stateMu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (s *Server) isShuttingDown() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.shuttingDown
}

func (s *Server) generationStopped() bool {
	select {
	case <-s.shutdownCtx.Done():
		return true
	default:
		return false
	}
}

// commitGenerationResult serializes pool publication with Shutdown. Once this
// generation is canceled, no completed dial or handshake may mutate pool state.
func (s *Server) commitGenerationResult(commit func()) (stopped bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.shuttingDown {
		return true
	}
	commit()
	return false
}

func (s *Server) registerActiveConnection(conn net.Conn) bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.shuttingDown {
		return false
	}
	if s.activeConns == nil {
		s.activeConns = make(map[net.Conn]struct{})
	}
	if _, exists := s.activeConns[conn]; exists {
		return true
	}
	s.activeConns[conn] = struct{}{}
	s.activeWG.Add(1)
	return true
}

func (s *Server) unregisterActiveConnection(conn net.Conn) {
	s.stateMu.Lock()
	if _, exists := s.activeConns[conn]; !exists {
		s.stateMu.Unlock()
		return
	}
	delete(s.activeConns, conn)
	s.stateMu.Unlock()
	s.activeWG.Done()
}

func (s *Server) relay(client, remote net.Conn) {
	if !s.registerActiveConnection(remote) {
		_ = remote.Close()
		return
	}
	defer s.unregisterActiveConnection(remote)
	relay(client, remote)
}

func (s *Server) serve(ln net.Listener) error {
	var temporaryDelay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				if temporaryDelay == 0 {
					temporaryDelay = socksAcceptRetryInitialDelay
				} else {
					temporaryDelay *= 2
					if temporaryDelay > socksAcceptRetryMaxDelay {
						temporaryDelay = socksAcceptRetryMaxDelay
					}
				}
				log.Printf("[server] temporary accept error: %v; retrying in %s", err, temporaryDelay)
				timer := time.NewTimer(temporaryDelay)
				select {
				case <-timer.C:
				case <-s.shutdownCtx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return net.ErrClosed
				}
				continue
			}
			return fmt.Errorf("accept failed: %w", err)
		}
		temporaryDelay = 0
		select {
		case s.connSlots <- struct{}{}:
			if !s.registerActiveConnection(conn) {
				<-s.connSlots
				_ = conn.Close()
				continue
			}
			go func() {
				defer s.unregisterActiveConnection(conn)
				defer func() { <-s.connSlots }()
				s.handleConn(conn)
			}()
		default:
			// Reject before authentication without allocating another goroutine.
			// Logging each rejection would itself become a denial-of-service vector.
			_ = conn.Close()
		}
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// Bound clients that connect but never finish the SOCKS greeting/request.
	// The deadline is cleared once the complete CONNECT frame has been read so
	// it cannot terminate a subsequently long-lived relay.
	if err := conn.SetDeadline(time.Now().Add(socks5HandshakeTimeout)); err != nil {
		log.Printf("[server] set handshake deadline failed: %v", err)
		return
	}

	// 1. SOCKS5 method negotiation. A TCP Read is not a message boundary, so
	// read the declared method list in full before selecting a method.
	if err := s.negotiate(conn); err != nil {
		return
	}

	// 2. Read one complete CONNECT request without consuming any application
	// data that may already follow it in the TCP stream.
	targetAddr, replyStatus, err := readConnectRequest(conn)
	if err != nil {
		if replyStatus != 0 {
			s.sendReply(conn, replyStatus)
		}
		return
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		log.Printf("[server] clear handshake deadline failed: %v", err)
		return
	}

	host, _, err := net.SplitHostPort(targetAddr)
	if err != nil {
		s.sendReply(conn, replyHostUnreachable)
		return
	}

	rules := s.store.Rules()
	groups := s.store.Groups()
	groupName := MatchGroup(rules, host)
	allowAnyFallback := true
	if policy := s.currentRoutePolicy(); policy != nil && policy.mode != ListenerModeRules {
		allowAnyFallback = false
		if policy.direct {
			groupName = GroupDirect
		} else {
			effectiveGroup := effectiveListenerGroup(policy, groups)
			groupName = effectiveGroup.Name
			groups = append(groups, effectiveGroup)
		}
	}

	if groupName == GroupDirect {
		remote, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(s.shutdownCtx, "tcp", targetAddr)
		if err != nil {
			if s.generationStopped() {
				return
			}
			log.Printf("[server] direct dial %s failed: %v", targetAddr, err)
			s.sendReply(conn, directDialErrorReply(err))
			return
		}
		if s.generationStopped() {
			_ = remote.Close()
			return
		}
		log.Printf("[route] %s -> DIRECT", host)
		s.sendReply(conn, replySucceeded, remote.LocalAddr())
		s.relay(conn, remote)
		return
	}

	// 3. Pick an upstream for the matched group, retrying against other
	// members on failure.
	exclude := make(map[string]bool)
	maxRetries := 3
	for i := 0; i < maxRetries; i++ {
		var upstream Proxy
		var ok bool
		if i == 0 {
			upstream, ok, _ = s.pool.Pick(groupName, groups)
		} else {
			upstream, ok, _ = s.pool.PickExcluding(groupName, groups, exclude)
		}
		if !ok {
			// The matched group currently has no usable member - most
			// commonly a country group ("COUNTRY:JP") with no live node in
			// that country this cycle. Fall back to ANY so the request still
			// succeeds instead of hard-failing, rather than blackholing it.
			if allowAnyFallback && groupName != GroupAny {
				log.Printf("[route] group %q has no available node, falling back to ANY", groupName)
				groupName = GroupAny
				continue
			}
			log.Printf("[server] no proxies available for group %q", groupName)
			s.sendReply(conn, replyGeneralFailure)
			return
		}
		if !allowAnyFallback && !upstream.Available {
			log.Printf("[server] listener group %q has no available proxy", groupName)
			s.sendReply(conn, replyGeneralFailure)
			return
		}

		dialStart := time.Now()
		healthGeneration := s.pool.HealthGeneration()
		remote, verified, err := DialUpstreamCredentialCandidatesContext(s.shutdownCtx, upstream, targetAddr, 10*time.Second)
		if err != nil {
			if s.commitGenerationResult(func() {
				log.Printf("[server] upstream %s (group %s) failed: %v, switching...", upstream.Addr(), groupName, err)
				if upstreamFailureAffectsHealth(err) {
					s.pool.RecordResult(upstream.Key(), false, 0)
					// A completed upstream connect/auth/protocol failure is terminal for
					// automatic routing. Target-specific refusals stay request-local.
					s.pool.ObserveHealthResultAtGeneration(upstream.Key(), false, 0, healthGeneration)
				}
			}) {
				return
			}
			exclude[upstream.Key()] = true
			continue
		}
		if s.commitGenerationResult(func() {
			s.pool.UpdateVerifiedCredentialsAtGeneration(upstream.Key(), verified, healthGeneration)
			s.pool.RecordResult(upstream.Key(), true, time.Since(dialStart).Milliseconds())
		}) {
			_ = remote.Close()
			return
		}
		upstream = verified

		// Success - log which upstream carried this target, so it's
		// visible which node each domain actually used.
		log.Printf("[route] %s -> group %s -> %s://%s", host, groupName, upstream.Protocol, upstream.Addr())
		s.sendReply(conn, replySucceeded, remote.LocalAddr())
		s.relay(conn, remote)
		return
	}

	s.sendReply(conn, replyGeneralFailure)
}

func directDialErrorReply(err error) byte {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return replyConnectionRefused
	}
	if errors.Is(err, syscall.ENETUNREACH) {
		return replyNetworkUnreachable
	}
	if errors.Is(err, syscall.EHOSTUNREACH) {
		return replyHostUnreachable
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.Timeout() {
			return replyTTLExpired
		}
		return replyHostUnreachable
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return replyTTLExpired
	}
	if errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT) {
		return replyAddressTypeNotSupported
	}
	return replyGeneralFailure
}

// requiresAuthentication deliberately treats a partially supplied pair as
// authentication-enabled too. Config.Validate rejects that configuration, and
// this defensive behavior avoids accidentally exposing an unauthenticated
// listener should Server be constructed directly with a malformed pair.
func (s *Server) requiresAuthentication() bool {
	return s.socksUser != "" || s.socksPass != ""
}

func (s *Server) negotiate(conn io.ReadWriter) error {
	if s.requiresAuthentication() {
		return negotiateUsernamePassword(conn, s.socksUser, s.socksPass)
	}
	return negotiateNoAuth(conn)
}

// negotiateNoAuth reads the complete RFC 1928 greeting and selects no-auth
// only when the client actually offered it. In particular, NMETHODS is a
// length byte, not a hint that the rest of the greeting arrived in the same
// TCP packet.
func negotiateNoAuth(conn io.ReadWriter) error {
	return negotiateMethod(conn, socks5NoAuth)
}

// negotiateUsernamePassword selects RFC 1929 username/password
// authentication only when the client offered method 0x02, then completes its
// sub-negotiation before allowing a CONNECT request. In particular, an offered
// no-auth method does not weaken a listener configured with credentials.
func negotiateUsernamePassword(conn io.ReadWriter, expectedUser, expectedPass string) error {
	if err := negotiateMethod(conn, socks5UsernamePassword); err != nil {
		return err
	}
	return authenticateUsernamePassword(conn, expectedUser, expectedPass)
}

func negotiateMethod(conn io.ReadWriter, requiredMethod byte) error {
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return fmt.Errorf("read greeting header: %w", err)
	}
	if header[0] != socks5Version {
		return fmt.Errorf("unsupported SOCKS version: %d", header[0])
	}

	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return fmt.Errorf("read greeting methods: %w", err)
	}

	selected := byte(socks5NoAcceptableMethods)
	for _, method := range methods {
		if method == requiredMethod {
			selected = requiredMethod
			break
		}
	}
	if err := writeFull(conn, []byte{socks5Version, selected}); err != nil {
		return fmt.Errorf("write method selection: %w", err)
	}
	if selected == socks5NoAcceptableMethods {
		return fmt.Errorf("client did not offer required SOCKS5 authentication method")
	}
	return nil
}

// authenticateUsernamePassword completes RFC 1929's VER/ULEN/UNAME/PLEN/PASSWD
// exchange. It never includes credential material in an error or log message.
func authenticateUsernamePassword(conn io.ReadWriter, expectedUser, expectedPass string) error {
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return fmt.Errorf("read username/password authentication header: %w", err)
	}
	if header[0] != socks5AuthVersion || header[1] == 0 {
		return writeAuthenticationFailure(conn, "invalid username/password authentication request")
	}

	username := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, username); err != nil {
		return writeAuthenticationFailure(conn, "read username/password authentication username")
	}

	var passwordLength [1]byte
	if _, err := io.ReadFull(conn, passwordLength[:]); err != nil {
		return writeAuthenticationFailure(conn, "read username/password authentication password length")
	}
	if passwordLength[0] == 0 {
		return writeAuthenticationFailure(conn, "invalid username/password authentication request")
	}

	password := make([]byte, int(passwordLength[0]))
	if _, err := io.ReadFull(conn, password); err != nil {
		return writeAuthenticationFailure(conn, "read username/password authentication password")
	}

	userMatches := constantTimeCredentialEqual(username, expectedUser)
	passwordMatches := constantTimeCredentialEqual(password, expectedPass)
	if userMatches&passwordMatches != 1 {
		return writeAuthenticationFailure(conn, "username/password authentication failed")
	}
	if err := writeAuthenticationReply(conn, socks5AuthSucceeded); err != nil {
		return fmt.Errorf("write username/password authentication success: %w", err)
	}
	return nil
}

func writeAuthenticationFailure(conn io.Writer, message string) error {
	if err := writeAuthenticationReply(conn, socks5AuthFailure); err != nil {
		return fmt.Errorf("%s: write authentication failure: %w", message, err)
	}
	return fmt.Errorf("%s", message)
}

func writeAuthenticationReply(conn io.Writer, status byte) error {
	return writeFull(conn, []byte{socks5AuthVersion, status})
}

// constantTimeCredentialEqual compares fixed-size digests so the comparison
// itself has no early exit for a different input length or a mismatched byte.
// Credential contents are intentionally not returned or logged anywhere.
func constantTimeCredentialEqual(got []byte, expected string) int {
	gotDigest := sha256.Sum256(got)
	expectedDigest := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(gotDigest[:], expectedDigest[:])
}

// readConnectRequest reads exactly one RFC 1928 CONNECT frame. On error it
// returns the reply status for a syntactically complete but invalid request;
// zero means the peer disconnected before a request header arrived.
func readConnectRequest(r io.Reader) (string, byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return "", 0, fmt.Errorf("read request header: %w", err)
	}
	if header[0] != socks5Version {
		return "", replyGeneralFailure, fmt.Errorf("unsupported SOCKS version: %d", header[0])
	}
	if header[2] != 0x00 {
		return "", replyGeneralFailure, fmt.Errorf("invalid reserved byte: %d", header[2])
	}
	if header[1] != cmdConnect {
		return "", replyCommandNotSupported, fmt.Errorf("unsupported command: %d", header[1])
	}

	request := append([]byte(nil), header[:]...)
	switch header[3] {
	case atypIPv4:
		payload := make([]byte, net.IPv4len+2)
		if _, err := io.ReadFull(r, payload); err != nil {
			return "", replyHostUnreachable, fmt.Errorf("read IPv4 target: %w", err)
		}
		request = append(request, payload...)
	case atypDomain:
		var length [1]byte
		if _, err := io.ReadFull(r, length[:]); err != nil {
			return "", replyHostUnreachable, fmt.Errorf("read domain length: %w", err)
		}
		if length[0] == 0 {
			return "", replyHostUnreachable, fmt.Errorf("empty domain")
		}
		payload := make([]byte, int(length[0])+2)
		if _, err := io.ReadFull(r, payload); err != nil {
			return "", replyHostUnreachable, fmt.Errorf("read domain target: %w", err)
		}
		request = append(request, length[0])
		request = append(request, payload...)
	case atypIPv6:
		payload := make([]byte, net.IPv6len+2)
		if _, err := io.ReadFull(r, payload); err != nil {
			return "", replyHostUnreachable, fmt.Errorf("read IPv6 target: %w", err)
		}
		request = append(request, payload...)
	default:
		return "", replyAddressTypeNotSupported, fmt.Errorf("unsupported address type: %d", header[3])
	}

	target, err := parseTarget(request)
	if err != nil {
		return "", replyHostUnreachable, err
	}
	return target, replySucceeded, nil
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (s *Server) sendReply(conn net.Conn, status byte, bound ...net.Addr) {
	reply := []byte{socks5Version, status, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0}
	if len(bound) > 0 && bound[0] != nil {
		if tcpAddr, ok := bound[0].(*net.TCPAddr); ok && tcpAddr.Port >= 0 && tcpAddr.Port <= 65535 {
			if ip4 := tcpAddr.IP.To4(); ip4 != nil {
				reply = append([]byte{socks5Version, status, 0x00, atypIPv4}, ip4...)
				reply = append(reply, byte(tcpAddr.Port>>8), byte(tcpAddr.Port))
			} else if ip6 := tcpAddr.IP.To16(); ip6 != nil {
				reply = append([]byte{socks5Version, status, 0x00, atypIPv6}, ip6...)
				reply = append(reply, byte(tcpAddr.Port>>8), byte(tcpAddr.Port))
			}
		}
	}
	_ = writeFull(conn, reply)
}

// parseTarget extracts the target address from a SOCKS5 connect request.
func parseTarget(buf []byte) (string, error) {
	if len(buf) < 4 {
		return "", fmt.Errorf("request too short")
	}
	if buf[0] != socks5Version {
		return "", fmt.Errorf("unsupported SOCKS version: %d", buf[0])
	}
	if buf[1] != cmdConnect {
		return "", fmt.Errorf("unsupported command: %d", buf[1])
	}
	if buf[2] != 0x00 {
		return "", fmt.Errorf("invalid reserved byte: %d", buf[2])
	}

	var host string
	var portOffset int

	switch buf[3] {
	case atypIPv4:
		if len(buf) < 10 {
			return "", fmt.Errorf("ipv4 request too short")
		}
		host = fmt.Sprintf("%d.%d.%d.%d", buf[4], buf[5], buf[6], buf[7])
		portOffset = 8
	case atypDomain:
		if len(buf) < 5 {
			return "", fmt.Errorf("domain request too short")
		}
		domainLen := int(buf[4])
		if domainLen == 0 {
			return "", fmt.Errorf("empty domain")
		}
		if len(buf) < 5+domainLen+2 {
			return "", fmt.Errorf("domain request too short")
		}
		host = string(buf[5 : 5+domainLen])
		if !validProxyHostname(host) {
			return "", fmt.Errorf("invalid domain target")
		}
		portOffset = 5 + domainLen
	case atypIPv6:
		if len(buf) < 22 {
			return "", fmt.Errorf("ipv6 request too short")
		}
		ip := net.IP(buf[4:20])
		host = ip.String()
		portOffset = 20
	default:
		return "", fmt.Errorf("unsupported address type: %d", buf[3])
	}

	port := int(buf[portOffset])<<8 | int(buf[portOffset+1])
	if port == 0 {
		return "", fmt.Errorf("target port must be between 1 and 65535")
	}
	// net.JoinHostPort brackets IPv6 literals (e.g. "[::1]:443"); plain
	// fmt.Sprintf("%s:%d", ...) does not, which made net.SplitHostPort
	// reject every IPv6 target downstream in handleConn ("too many colons
	// in address"), so ATYP=IPv6 CONNECT requests always failed.
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// relay copies data bidirectionally between two connections.
func relay(left, right net.Conn) {
	// A full close is deferred as the ultimate fail-safe.
	defer left.Close()
	defer right.Close()

	type copyResult struct {
		err error
	}
	done := make(chan copyResult, 2)

	cp := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		// io.Copy reports a read EOF as nil. Only that clean completion may propagate a
		// half-close; every other error must abort the relay so the peer worker cannot
		// remain blocked indefinitely.
		if err == nil || errors.Is(err, io.EOF) {
			if writer, ok := dst.(interface{ CloseWrite() error }); ok {
				_ = writer.CloseWrite()
			}
			if reader, ok := src.(interface{ CloseRead() error }); ok {
				_ = reader.CloseRead()
			}
		}
		done <- copyResult{err: err}
	}

	go cp(left, right)
	go cp(right, left)

	// Wait for the first direction to finish. A clean EOF preserves the other
	// direction for half-close protocols; net.ErrClosed is only expected after
	// our own abort, so it is anomalous when it arrives first.
	first := <-done
	if first.err != nil && !errors.Is(first.err, io.EOF) {
		_ = left.Close()
		_ = right.Close()
	}

	// Join the surviving worker after either the normal half-close or hard abort.
	<-done
}
