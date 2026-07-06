package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// speedTestMaxBytes bounds how much we download per test, so it finishes in
// bounded time even on a fast upstream.
const speedTestMaxBytes = 3_000_000 // 3 MB

// speedTestURL is Cloudflare's anycast download endpoint, which returns
// exactly the requested number of bytes and is reachably fast from almost
// anywhere. It's HTTPS - net/http performs the TLS handshake over the raw
// conn our DialContext returns, so the tunnel still works. (The old
// cachefly.net/10mb.test file went stale and returns ~24 bytes now, which
// made every speed test either fail or measure nothing.)
var speedTestURL = fmt.Sprintf("https://speed.cloudflare.com/__down?bytes=%d", speedTestMaxBytes)

// SpeedTest measures approximate download throughput (in kbps) for a
// single upstream proxy by streaming a fixed-size download through it and
// timing the transfer. It's triggered on demand (dashboard button or
// /api/nodes/speedtest), never automatically for the whole pool -
// downloading megabytes through every candidate on every refresh would be
// far too slow/expensive.
//
// A slow proxy that only manages part of the download before the timeout
// still yields a (low) measurement rather than an error, as long as any
// bytes arrived - only a total failure to get a response is reported as an
// error.
func SpeedTest(px Proxy, timeout time.Duration) (kbps float64, err error) {
	if px.Protocol != "socks5" && px.Protocol != "http" && px.Protocol != "https" {
		return 0, fmt.Errorf("protocol %q does not support forwarding", px.Protocol)
	}

	req, err := http.NewRequest(http.MethodGet, speedTestURL, nil)
	if err != nil {
		return 0, err
	}

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
				return DialUpstream(px, addr, timeout)
			},
		},
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// Almost always a dead/slow node or one that can't reach the test
		// host (e.g. an HTTP-only proxy that won't do the HTTPS CONNECT).
		// Keep the message concise for the dashboard alert.
		return 0, fmt.Errorf("节点无响应(太慢/不可用,%s 内未连通)", timeout)
	}
	defer resp.Body.Close()

	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, speedTestMaxBytes))
	elapsed := time.Since(start)
	if n == 0 {
		return 0, fmt.Errorf("节点未返回数据(太慢或屏蔽了测速站)")
	}
	if elapsed <= 0 {
		return 0, fmt.Errorf("invalid elapsed time")
	}

	return float64(n) * 8 / 1024 / elapsed.Seconds(), nil
}
