package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// speedTestMaxBytes is both the exact sample size and the upper bound read
	// from either endpoint.
	speedTestMaxBytes = 3_000_000 // 3 MB

	// The dashboard request stays below the status server's 45-second write
	// timeout while leaving enough time for a primary attempt and one fallback.
	speedTestOperationTimeout  = 42 * time.Second
	speedTestPrimaryMaxBudget  = 20 * time.Second
	speedTestFallbackMaxBudget = 25 * time.Second
)

// Cloudflare remains the preferred anycast target. Some otherwise healthy
// proxies intermittently truncate this response, so a fixed Range request to
// Hetzner provides an independent fallback rather than declaring the node
// unusable from one endpoint-specific failure.
var (
	speedTestURL         = fmt.Sprintf("https://speed.cloudflare.com/__down?bytes=%d", speedTestMaxBytes)
	speedTestFallbackURL = "https://nbg1-speed.hetzner.com/100MB.bin"
)

type SpeedTestResult struct {
	Kbps       float64
	Bytes      int64
	DurationMs int64
}

type speedTestEndpoint struct {
	Name         string
	URL          string
	RequireRange bool
}

type speedTestCombinedError struct {
	Primary  error
	Fallback error
}

func (e *speedTestCombinedError) Error() string {
	return fmt.Sprintf("测速未完成：Cloudflare 失败(%v)；Hetzner 备用失败(%v)。这通常只是测速站链路问题，不代表代理节点不可用，可稍后重试", e.Primary, e.Fallback)
}

func (e *speedTestCombinedError) Unwrap() []error {
	return []error{e.Primary, e.Fallback}
}

type speedTestStoppedError struct {
	Primary  error
	Fallback error
	Cause    error
}

func (e *speedTestStoppedError) Error() string {
	if e.Fallback == nil {
		return fmt.Sprintf("Cloudflare 测速失败(%v)后操作已取消或超时，未尝试 Hetzner 备用；这不代表代理节点不可用", e.Primary)
	}
	return fmt.Sprintf("Cloudflare 测速失败(%v)，Hetzner 备用测速被取消或超时(%v)；这不代表代理节点不可用", e.Primary, e.Fallback)
}

func (e *speedTestStoppedError) Unwrap() []error {
	errs := []error{e.Primary, e.Cause}
	if e.Fallback != nil {
		errs = append(errs, e.Fallback)
	}
	return errs
}

// SpeedTest measures approximate download throughput (in kbps) for a
// single upstream proxy by streaming a fixed-size download through it and
// timing the transfer. It's triggered on demand (dashboard button or
// /api/nodes/speedtest), never automatically for the whole pool -
// downloading megabytes through every candidate on every refresh would be
// far too slow/expensive.
//
// A result is recorded only after the entire fixed-size payload arrives.
// Partial downloads, redirects, non-2xx responses, and timeouts are errors.
// Cloudflare is attempted first; an endpoint-specific failure then falls back
// to a byte-range request against Hetzner within the same total deadline.
func SpeedTest(px Proxy, timeout time.Duration) (SpeedTestResult, error) {
	return SpeedTestContext(context.Background(), px, timeout)
}

// SpeedTestContext is the request-aware form used by the status API. Parent
// cancellation stops the active endpoint immediately and prevents a fallback
// from starting, so abandoned browser requests do not occupy a global slot for
// the rest of the 42-second operation budget.
func SpeedTestContext(parent context.Context, px Proxy, timeout time.Duration) (SpeedTestResult, error) {
	if px.Protocol != "socks5" && px.Protocol != "http" && px.Protocol != "https" {
		return SpeedTestResult{}, fmt.Errorf("protocol %q does not support forwarding", px.Protocol)
	}
	if timeout <= 0 {
		return SpeedTestResult{}, fmt.Errorf("测速超时预算必须大于 0")
	}

	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	primaryBudget := timeout * 45 / 100
	if primaryBudget <= 0 {
		primaryBudget = timeout
	}
	if primaryBudget > speedTestPrimaryMaxBudget {
		primaryBudget = speedTestPrimaryMaxBudget
	}
	primary := speedTestEndpoint{Name: "Cloudflare", URL: speedTestURL}
	result, primaryErr := runSpeedTestEndpoint(ctx, px, primary, primaryBudget)
	if primaryErr == nil {
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return SpeedTestResult{}, &speedTestStoppedError{Primary: primaryErr, Cause: ctxErr}
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return SpeedTestResult{}, &speedTestStoppedError{Primary: primaryErr, Cause: context.DeadlineExceeded}
	}
	fallbackBudget := remaining
	if fallbackBudget > speedTestFallbackMaxBudget {
		fallbackBudget = speedTestFallbackMaxBudget
	}
	fallback := speedTestEndpoint{Name: "Hetzner", URL: speedTestFallbackURL, RequireRange: true}
	result, fallbackErr := runSpeedTestEndpoint(ctx, px, fallback, fallbackBudget)
	if fallbackErr == nil {
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return SpeedTestResult{}, &speedTestStoppedError{Primary: primaryErr, Fallback: fallbackErr, Cause: ctxErr}
	}
	return SpeedTestResult{}, &speedTestCombinedError{Primary: primaryErr, Fallback: fallbackErr}
}

func runSpeedTestEndpoint(parent context.Context, px Proxy, endpoint speedTestEndpoint, budget time.Duration) (SpeedTestResult, error) {
	if budget <= 0 {
		return SpeedTestResult{}, fmt.Errorf("%s 测速预算已耗尽", endpoint.Name)
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.URL, nil)
	if err != nil {
		return SpeedTestResult{}, fmt.Errorf("%s 测速请求无效: %w", endpoint.Name, err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if endpoint.RequireRange {
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", speedTestMaxBytes-1))
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return DialUpstreamContext(ctx, px, addr, budget)
		},
		DisableKeepAlives:  true,
		DisableCompression: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   budget,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return SpeedTestResult{}, fmt.Errorf("%s 请求失败: %w", endpoint.Name, err)
	}
	defer resp.Body.Close()

	if endpoint.RequireRange {
		if resp.StatusCode != http.StatusPartialContent {
			return SpeedTestResult{}, fmt.Errorf("%s Range 测速站返回异常状态:%s", endpoint.Name, resp.Status)
		}
		if !validSpeedTestContentRange(resp.Header.Get("Content-Range")) {
			return SpeedTestResult{}, fmt.Errorf("%s Range 响应范围无效:%q", endpoint.Name, resp.Header.Get("Content-Range"))
		}
		if resp.ContentLength >= 0 && resp.ContentLength != speedTestMaxBytes {
			return SpeedTestResult{}, fmt.Errorf("%s Range 响应声明 %d 字节,需要 %d 字节", endpoint.Name, resp.ContentLength, speedTestMaxBytes)
		}
	} else {
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return SpeedTestResult{}, fmt.Errorf("%s 测速站返回异常状态:%s", endpoint.Name, resp.Status)
		}
		if resp.ContentLength >= 0 && resp.ContentLength < speedTestMaxBytes {
			return SpeedTestResult{}, fmt.Errorf("%s 测速站仅声明返回 %d 字节,不足 %d 字节", endpoint.Name, resp.ContentLength, speedTestMaxBytes)
		}
	}

	start := time.Now()
	n, copyErr := io.CopyN(io.Discard, resp.Body, speedTestMaxBytes)
	elapsed := time.Since(start)
	if copyErr != nil {
		return SpeedTestResult{}, fmt.Errorf("%s 测速下载不完整:%d/%d 字节: %w", endpoint.Name, n, speedTestMaxBytes, copyErr)
	}
	if n != speedTestMaxBytes {
		return SpeedTestResult{}, fmt.Errorf("%s 测速下载不完整:%d/%d 字节", endpoint.Name, n, speedTestMaxBytes)
	}
	if elapsed <= 0 {
		return SpeedTestResult{}, fmt.Errorf("%s 测速耗时无效", endpoint.Name)
	}

	durationMs := elapsed.Milliseconds()
	if durationMs < 1 {
		durationMs = 1
	}
	return SpeedTestResult{
		Kbps:       float64(n) * 8 / 1000 / elapsed.Seconds(),
		Bytes:      n,
		DurationMs: durationMs,
	}, nil
}

func validSpeedTestContentRange(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "bytes ") {
		return false
	}
	parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(value, "bytes ")), "/")
	if len(parts) != 2 {
		return false
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return false
	}
	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil || start != 0 {
		return false
	}
	end, err := strconv.ParseInt(bounds[1], 10, 64)
	if err != nil || end != speedTestMaxBytes-1 {
		return false
	}
	if parts[1] == "*" {
		return true
	}
	total, err := strconv.ParseInt(parts[1], 10, 64)
	return err == nil && total > end
}
