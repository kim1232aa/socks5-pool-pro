package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpeedTestDownloadsCompletePayloadThroughHTTPConnect(t *testing.T) {
	payload := strings.Repeat("x", speedTestMaxBytes)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		// Keep the transfer long enough that DurationMs and the relationship
		// between duration and throughput can be asserted without relying on a
		// sub-millisecond localhost request.
		const chunkSize = 64 * 1024
		for offset := 0; offset < len(payload); offset += chunkSize {
			end := offset + chunkSize
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := io.WriteString(w, payload[offset:end]); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(time.Millisecond)
		}
	}))
	t.Cleanup(target.Close)

	proxy := newSpeedTestConnectProxy(t)
	restoreSpeedTestURL(t, target.URL)

	result, err := SpeedTest(proxy, 5*time.Second)
	if err != nil {
		t.Fatalf("SpeedTest() error = %v", err)
	}
	if result.Bytes != speedTestMaxBytes {
		t.Fatalf("Bytes = %d, want %d", result.Bytes, speedTestMaxBytes)
	}
	if result.DurationMs <= 0 || result.DurationMs >= 5_000 {
		t.Fatalf("DurationMs = %d, want a positive duration below timeout", result.DurationMs)
	}
	if math.IsNaN(result.Kbps) || math.IsInf(result.Kbps, 0) || result.Kbps <= 0 {
		t.Fatalf("Kbps = %v, want a finite positive throughput", result.Kbps)
	}

	// DurationMs is elapsed time rounded down to milliseconds, so the
	// throughput reconstructed from it is a small upper bound for Kbps.
	fromRoundedDuration := float64(result.Bytes) * 8 / float64(result.DurationMs)
	if result.Kbps > fromRoundedDuration || result.Kbps < fromRoundedDuration*0.90 {
		t.Fatalf("Kbps = %.2f is inconsistent with %d bytes in %dms (rounded estimate %.2f)",
			result.Kbps, result.Bytes, result.DurationMs, fromRoundedDuration)
	}
}

func TestSpeedTestFallsBackToCompleteHetznerRange(t *testing.T) {
	var primaryCalls atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		http.Error(w, "primary unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(primary.Close)

	payload := strings.Repeat("h", speedTestMaxBytes)
	var fallbackCalls atomic.Int64
	var rangeMatched atomic.Bool
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		if r.Header.Get("Range") == fmt.Sprintf("bytes=0-%d", speedTestMaxBytes-1) {
			rangeMatched.Store(true)
		}
		w.Header().Set("Content-Length", fmt.Sprint(speedTestMaxBytes))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/100000000", speedTestMaxBytes-1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(fallback.Close)

	restoreSpeedTestURLs(t, primary.URL, fallback.URL)
	result, err := SpeedTest(newSpeedTestConnectProxy(t), 5*time.Second)
	if err != nil {
		t.Fatalf("SpeedTest() fallback error = %v", err)
	}
	if result.Bytes != speedTestMaxBytes || result.Kbps <= 0 || result.DurationMs < 1 {
		t.Fatalf("fallback result = %+v, want a complete %d-byte sample", result, speedTestMaxBytes)
	}
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 || !rangeMatched.Load() {
		t.Fatalf("calls primary/fallback=%d/%d rangeMatched=%v", primaryCalls.Load(), fallbackCalls.Load(), rangeMatched.Load())
	}
}

func TestSpeedTestContextCancellationSkipsFallback(t *testing.T) {
	started := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(primary.Close)
	var fallbackCalls atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		http.Error(w, "must not be reached", http.StatusInternalServerError)
	}))
	t.Cleanup(fallback.Close)

	restoreSpeedTestURLs(t, primary.URL, fallback.URL)
	proxy := newSpeedTestConnectProxy(t)
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		result SpeedTestResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := SpeedTestContext(ctx, proxy, 5*time.Second)
		done <- outcome{result: result, err: err}
	}()
	<-started
	canceledAt := time.Now()
	cancel()
	select {
	case got := <-done:
		if got.result != (SpeedTestResult{}) {
			t.Fatalf("canceled result = %+v, want zero", got.result)
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled error = %v, want errors.Is(context.Canceled)", got.err)
		}
		if strings.Contains(got.err.Error(), "Hetzner 备用失败") {
			t.Fatalf("canceled error falsely reports unattempted fallback: %v", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("SpeedTestContext did not stop promptly after parent cancellation")
	}
	if elapsed := time.Since(canceledAt); elapsed > time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback calls after parent cancellation = %d, want 0", fallbackCalls.Load())
	}
}

func TestValidSpeedTestContentRange(t *testing.T) {
	last := speedTestMaxBytes - 1
	tests := []struct {
		value string
		valid bool
	}{
		{fmt.Sprintf("bytes 0-%d/100000000", last), true},
		{fmt.Sprintf("bytes 0-%d/%d", last, speedTestMaxBytes), true},
		{fmt.Sprintf("bytes 0-%d/*", last), true},
		{fmt.Sprintf("bytes 1-%d/100000000", last), false},
		{fmt.Sprintf("bytes 0-%d/100000000", last-1), false},
		{fmt.Sprintf("bytes 0-%d/%d", last, last), false},
		{fmt.Sprintf("bytes 0-%d/1", last), false},
		{fmt.Sprintf("bytes 0-%d/garbage", last), false},
		{fmt.Sprintf("items 0-%d/100000000", last), false},
		{"", false},
	}
	for _, tt := range tests {
		if got := validSpeedTestContentRange(tt.value); got != tt.valid {
			t.Errorf("validSpeedTestContentRange(%q) = %v, want %v", tt.value, got, tt.valid)
		}
	}
}

func TestSpeedTestFallbackRejectsTruncatedPartialContent(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "primary unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(primary.Close)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(speedTestMaxBytes))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/100000000", speedTestMaxBytes-1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, strings.Repeat("x", speedTestMaxBytes/4))
	}))
	t.Cleanup(fallback.Close)

	restoreSpeedTestURLs(t, primary.URL, fallback.URL)
	_, err := SpeedTest(newSpeedTestConnectProxy(t), 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "Hetzner 测速下载不完整") {
		t.Fatalf("SpeedTest() error = %v, want truncated fallback rejection", err)
	}
}

func TestSpeedTestSummarizesBothEndpointFailuresWithoutCondemningNode(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "primary unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(primary.Close)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fallback unavailable", http.StatusBadGateway)
	}))
	t.Cleanup(fallback.Close)

	restoreSpeedTestURLs(t, primary.URL, fallback.URL)
	_, err := SpeedTest(newSpeedTestConnectProxy(t), 3*time.Second)
	if err == nil {
		t.Fatal("SpeedTest() error = nil, want both-endpoint failure")
	}
	message := err.Error()
	for _, want := range []string{"Cloudflare", "503", "Hetzner", "502", "不代表代理节点不可用", "可稍后重试"} {
		if !strings.Contains(message, want) {
			t.Fatalf("combined error %q lacks %q", message, want)
		}
	}
}

func TestSpeedTestTotalTimeoutBoundsPrimaryAndFallback(t *testing.T) {
	var primaryCalls atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(primary.Close)
	var fallbackCalls atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(fallback.Close)

	restoreSpeedTestURLs(t, primary.URL, fallback.URL)
	started := time.Now()
	_, err := SpeedTest(newSpeedTestConnectProxy(t), 400*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("SpeedTest() error = nil, want timeout")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("SpeedTest() took %s, want bounded near 400ms total budget", elapsed)
	}
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("timeout attempts primary/fallback=%d/%d, want 1/1", primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestSpeedTestRejectsNon2xxResponse(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(target.Close)

	restoreSpeedTestURL(t, target.URL)
	_, err := SpeedTest(newSpeedTestConnectProxy(t), 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("SpeedTest() error = %v, want non-2xx status rejection", err)
	}
}

func TestSpeedTestRejectsShortDeclaredContentLength(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "128")
		_, _ = io.WriteString(w, strings.Repeat("x", 128))
	}))
	t.Cleanup(target.Close)

	restoreSpeedTestURL(t, target.URL)
	_, err := SpeedTest(newSpeedTestConnectProxy(t), 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "不足") {
		t.Fatalf("SpeedTest() error = %v, want short Content-Length rejection", err)
	}
}

func TestSpeedTestRejectsTruncatedBody(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(speedTestMaxBytes))
		_, _ = io.WriteString(w, strings.Repeat("x", speedTestMaxBytes/4))
	}))
	t.Cleanup(target.Close)

	restoreSpeedTestURL(t, target.URL)
	_, err := SpeedTest(newSpeedTestConnectProxy(t), 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "下载不完整") {
		t.Fatalf("SpeedTest() error = %v, want truncated download rejection", err)
	}
}

func TestSpeedTestDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			followed.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(speedTestMaxBytes))
			_, _ = io.WriteString(w, strings.Repeat("x", speedTestMaxBytes))
			return
		}
		http.Redirect(w, r, "/download", http.StatusFound)
	}))
	t.Cleanup(target.Close)

	restoreSpeedTestURL(t, target.URL+"/redirect")
	_, err := SpeedTest(newSpeedTestConnectProxy(t), 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("SpeedTest() error = %v, want redirect rejection", err)
	}
	if got := followed.Load(); got != 0 {
		t.Fatalf("redirect target was fetched %d time(s), want 0", got)
	}
}

func TestSpeedTestRejectsUnsupportedProtocol(t *testing.T) {
	_, err := SpeedTest(Proxy{IP: "127.0.0.1", Port: "1", Protocol: "proxyip"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "does not support forwarding") {
		t.Fatalf("SpeedTest() error = %v, want unsupported protocol error", err)
	}
}

func restoreSpeedTestURL(t *testing.T, testURL string) {
	restoreSpeedTestURLs(t, testURL, testURL)
}

func restoreSpeedTestURLs(t *testing.T, primaryURL, fallbackURL string) {
	t.Helper()
	originalPrimary := speedTestURL
	originalFallback := speedTestFallbackURL
	speedTestURL = primaryURL
	speedTestFallbackURL = fallbackURL
	t.Cleanup(func() {
		speedTestURL = originalPrimary
		speedTestFallbackURL = originalFallback
	})
}

// newSpeedTestConnectProxy returns a local HTTP CONNECT relay. It exercises
// the production dialHTTPConnect path while keeping every byte on localhost.
func newSpeedTestConnectProxy(t *testing.T) Proxy {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}

		upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			http.Error(w, "target unavailable", http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		client, buffered, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		defer client.Close()
		defer upstream.Close()

		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err := buffered.Flush(); err != nil {
			return
		}

		go func() {
			_, _ = io.Copy(upstream, buffered)
			if tcp, ok := upstream.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
		}()
		_, _ = io.Copy(client, upstream)
	}))
	t.Cleanup(server.Close)

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split proxy address: %v", err)
	}
	return Proxy{IP: host, Port: port, Protocol: "http"}
}
