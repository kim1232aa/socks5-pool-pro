package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"time"
)

const (
	socks5Version = 0x05
	cmdConnect    = 0x01
	atypIPv4      = 0x01
	atypDomain    = 0x03
	atypIPv6      = 0x04
)

// Server is the local SOCKS5 endpoint that clients connect to. Every
// incoming request is routed through the configured rules to a Group (or
// DIRECT), then forwarded via that group's chosen upstream.
type Server struct {
	listenAddr string
	pool       *ProxyPool
	store      *ConfigStore
}

func NewServer(listenAddr string, pool *ProxyPool, store *ConfigStore) *Server {
	return &Server{listenAddr: listenAddr, pool: pool, store: store}
}

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	log.Printf("[server] SOCKS5 proxy listening on %s", s.listenAddr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[server] accept error: %v", err)
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// 1. SOCKS5 handshake - read greeting. Sized to fit the largest legal
	// message reused below: a domain-based CONNECT request can be up to
	// 4+1+255+2=262 bytes (VER/CMD/RSV/ATYP + domain-length byte + a
	// maximal 255-byte domain + port).
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil || n < 2 || buf[0] != socks5Version {
		return
	}

	// Reply: no auth required
	conn.Write([]byte{socks5Version, 0x00})

	// 2. Read connect request
	n, err = conn.Read(buf)
	if err != nil || n < 7 || buf[1] != cmdConnect {
		s.sendReply(conn, 0x07) // command not supported
		return
	}

	targetAddr, err := parseTarget(buf[:n])
	if err != nil {
		s.sendReply(conn, 0x04) // host unreachable
		return
	}

	host, _, err := net.SplitHostPort(targetAddr)
	if err != nil {
		s.sendReply(conn, 0x04)
		return
	}

	rules := s.store.Rules()
	groups := s.store.Groups()
	groupName := MatchGroup(rules, host)

	if groupName == GroupDirect {
		remote, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
		if err != nil {
			log.Printf("[server] direct dial %s failed: %v", targetAddr, err)
			s.sendReply(conn, 0x01)
			return
		}
		log.Printf("[route] %s -> DIRECT", host)
		s.sendReply(conn, 0x00)
		relay(conn, remote)
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
			if groupName != GroupAny {
				log.Printf("[route] group %q has no available node, falling back to ANY", groupName)
				groupName = GroupAny
				continue
			}
			log.Printf("[server] no proxies available for group %q", groupName)
			s.sendReply(conn, 0x01) // general failure
			return
		}

		dialStart := time.Now()
		remote, err := DialUpstream(upstream, targetAddr, 10*time.Second)
		if err != nil {
			log.Printf("[server] upstream %s (group %s) failed: %v, switching...", upstream.Addr(), groupName, err)
			s.pool.RecordResult(upstream.Key(), false, 0)
			exclude[upstream.Key()] = true
			continue
		}
		s.pool.RecordResult(upstream.Key(), true, time.Since(dialStart).Milliseconds())

		// Success - log which upstream carried this target, so it's
		// visible which node each domain actually used.
		log.Printf("[route] %s -> group %s -> %s://%s", host, groupName, upstream.Protocol, upstream.Addr())
		s.sendReply(conn, 0x00)
		relay(conn, remote)
		return
	}

	s.sendReply(conn, 0x01) // general failure after retries
}

func (s *Server) sendReply(conn net.Conn, status byte) {
	// Minimal SOCKS5 reply: ver, status, rsv, atyp(ipv4), addr(0.0.0.0), port(0)
	conn.Write([]byte{socks5Version, status, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
}

// parseTarget extracts the target address from a SOCKS5 connect request.
func parseTarget(buf []byte) (string, error) {
	if len(buf) < 7 {
		return "", fmt.Errorf("request too short")
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
		domainLen := int(buf[4])
		if len(buf) < 5+domainLen+2 {
			return "", fmt.Errorf("domain request too short")
		}
		host = string(buf[5 : 5+domainLen])
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
	// net.JoinHostPort brackets IPv6 literals (e.g. "[::1]:443"); plain
	// fmt.Sprintf("%s:%d", ...) does not, which made net.SplitHostPort
	// reject every IPv6 target downstream in handleConn ("too many colons
	// in address"), so ATYP=IPv6 CONNECT requests always failed.
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// relay copies data bidirectionally between two connections.
func relay(left, right net.Conn) {
	defer left.Close()
	defer right.Close()

	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}

	go cp(left, right)
	go cp(right, left)
	// Wait for BOTH directions to finish before closing either connection -
	// waiting for only one (the previous behavior) meant whichever
	// direction happened to finish first (e.g. a client that half-closes
	// after sending its request) triggered an immediate full close of both
	// connections, truncating the other direction's still-in-flight data.
	<-done
	<-done
}
