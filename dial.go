package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DialUpstream establishes a raw tunnel to target through the given
// upstream proxy, dispatching on px.Protocol. Once established, the
// returned conn carries bytes to/from target exactly as if dialed
// directly - callers don't need to know which protocol was used.
func DialUpstream(px Proxy, target string, timeout time.Duration) (net.Conn, error) {
	switch px.Protocol {
	case "socks5":
		return dialSOCKS5(px, target, timeout)
	case "http", "https":
		return dialHTTPConnect(px, target, timeout)
	default:
		return nil, fmt.Errorf("protocol %q cannot be used as a forwarding upstream", px.Protocol)
	}
}

func dialSOCKS5(px Proxy, target string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", px.Addr(), timeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(timeout))

	methods := []byte{0x00}
	if px.Username != "" {
		methods = append(methods, 0x02)
	}
	greeting := append([]byte{socks5Version, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		conn.Close()
		return nil, err
	}

	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		conn.Close()
		return nil, err
	}
	if buf[0] != socks5Version {
		conn.Close()
		return nil, fmt.Errorf("not socks5")
	}

	switch buf[1] {
	case 0x00:
		// no auth required
	case 0x02:
		if px.Username == "" {
			conn.Close()
			return nil, fmt.Errorf("upstream requires auth, none configured")
		}
		authReq := []byte{0x01, byte(len(px.Username))}
		authReq = append(authReq, []byte(px.Username)...)
		authReq = append(authReq, byte(len(px.Password)))
		authReq = append(authReq, []byte(px.Password)...)
		if _, err := conn.Write(authReq); err != nil {
			conn.Close()
			return nil, err
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			conn.Close()
			return nil, err
		}
		if authResp[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("upstream rejected auth")
		}
	case 0xFF:
		conn.Close()
		return nil, fmt.Errorf("upstream has no acceptable auth method")
	default:
		conn.Close()
		return nil, fmt.Errorf("unsupported auth method: %d", buf[1])
	}

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)

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

	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}

	resp := make([]byte, 256)
	n, err := conn.Read(resp)
	if err != nil || n < 2 || resp[1] != 0x00 {
		conn.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("upstream connect failed, status: %d", resp[1])
	}

	conn.SetDeadline(time.Time{})
	return conn, nil
}

// dialHTTPConnect tunnels to target via the HTTP CONNECT method. Used for
// both "http" and "https" tagged upstreams: the tag reflects what the
// source list advertised about the proxy's capability, not the wire
// protocol spoken to reach it (both use plain HTTP CONNECT).
func dialHTTPConnect(px Proxy, target string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", px.Addr(), timeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(timeout))

	var sb strings.Builder
	fmt.Fprintf(&sb, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if px.Username != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(px.Username + ":" + px.Password))
		fmt.Fprintf(&sb, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	sb.WriteString("\r\n")

	if _, err := conn.Write([]byte(sb.String())); err != nil {
		conn.Close()
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("upstream CONNECT failed: %s", resp.Status)
	}

	conn.SetDeadline(time.Time{})
	return conn, nil
}
