package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// speedTestURL is a long-standing plain-HTTP bandwidth test file mirrored
// on Cachefly's CDN, commonly used by CLI bandwidth-test scripts. Plain
// HTTP (no TLS) keeps the client side simple - DialUpstream's returned
// conn is handed straight to net/http with no TLS wrapping needed.
const speedTestURL = "http://cachefly.cachefly.net/10mb.test"

// speedTestMaxBytes bounds how much of the test file we actually read, so
// a single speed test finishes in bounded time even on a fast upstream.
const speedTestMaxBytes = 3 << 20 // 3MB

// SpeedTest measures approximate download throughput (in kbps) for a
// single upstream proxy by streaming part of a public test file through
// it and timing the transfer. It's triggered on demand (dashboard button
// or /api/nodes/speedtest), never automatically for the whole pool -
// downloading megabytes through every candidate on every refresh would be
// far too slow/expensive.
func SpeedTest(px Proxy, timeout time.Duration) (kbps float64, err error) {
	if px.Protocol != "socks5" && px.Protocol != "http" && px.Protocol != "https" {
		return 0, fmt.Errorf("protocol %q does not support forwarding", px.Protocol)
	}

	req, err := http.NewRequest(http.MethodGet, speedTestURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", speedTestMaxBytes-1))

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
		return 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	n, copyErr := io.Copy(io.Discard, io.LimitReader(resp.Body, speedTestMaxBytes))
	elapsed := time.Since(start)
	if n == 0 {
		if copyErr != nil {
			return 0, fmt.Errorf("read failed: %w", copyErr)
		}
		return 0, fmt.Errorf("no data received")
	}
	if elapsed <= 0 {
		return 0, fmt.Errorf("invalid elapsed time")
	}

	return float64(n) * 8 / 1024 / elapsed.Seconds(), nil
}
