package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

// buildBenchPool is the shared pool builder used by both benchmark and the
// allocation regression test. It is deliberately allocation-light at setup so
// measured allocations reflect handler cost, not pool construction.
func buildBenchPool(n int) (*StatusServer, http.Handler) {
	proxies := make([]Proxy, 0, n)
	for i := 0; i < n; i++ {
		proto := "socks5"
		if i%7 == 0 {
			proto = "http"
		}
		px := Proxy{
			Protocol:  proto,
			IP:        fmt.Sprintf("203.0.%d.%d", (i>>8)&0xff, i&0xff),
			Port:      "1080",
			Country:   []string{"US", "JP", "DE", "FR", "GB"}[i%5],
			City:      fmt.Sprintf("city-%d", i),
			ExitIP:    fmt.Sprintf("198.51.%d.%d", (i>>8)&0xff, i&0xff),
			LatencyMs: int64(10 + (i % 500)),
			SpeedKbps: float64(5000 + (i%1000)*10),
		}
		px.Available = true
		proxies = append(proxies, px)
	}
	pool := NewProxyPool()
	pool.Prime(proxies, nil)
	server := NewStatusServer(pool, &ConfigStore{})
	return server, server.handler()
}

func makePageBenchPool(b *testing.B, n int) (*StatusServer, http.Handler) {
	b.Helper()
	return buildBenchPool(n)
}

func BenchmarkNodePage1PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 1)
	benchmarkNodePage(b, h, 1, 1)
}

func BenchmarkNodePage100PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 100)
	benchmarkNodePage(b, h, 100, 1)
}

func BenchmarkNodePage1000PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 1000)
	benchmarkNodePage(b, h, 1000, 1)
}

func BenchmarkNodePage10000PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 10000)
	benchmarkNodePage(b, h, 10000, 1)
}

func BenchmarkNodePage10000PageSize20(b *testing.B) {
	_, h := makePageBenchPool(b, 10000)
	benchmarkNodePage(b, h, 10000, 20)
}

func BenchmarkNodePageMaxInt100PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 100)
	benchmarkNodeMaxIntPage(b, h, 100)
}

func BenchmarkNodePageMaxInt1000PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 1000)
	benchmarkNodeMaxIntPage(b, h, 1000)
}

func BenchmarkNodePageMaxInt10000PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 10000)
	benchmarkNodeMaxIntPage(b, h, 10000)
}

func BenchmarkV1ProxyPage100PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 100)
	benchmarkV1ProxyPage(b, h, 1)
}

func BenchmarkV1ProxyPage1000PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 1000)
	benchmarkV1ProxyPage(b, h, 1)
}

func BenchmarkV1ProxyPage10000PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 10000)
	benchmarkV1ProxyPage(b, h, 1)
}

func BenchmarkV1ProxyPage10000PageSize20(b *testing.B) {
	_, h := makePageBenchPool(b, 10000)
	benchmarkV1ProxyPage(b, h, 20)
}

func BenchmarkV1ProxyPageMaxInt100PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 100)
	benchmarkV1ProxyMaxIntPage(b, h, 100)
}

func BenchmarkV1ProxyPageMaxInt1000PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 1000)
	benchmarkV1ProxyMaxIntPage(b, h, 1000)
}

func BenchmarkV1ProxyPageMaxInt10000PageSize1(b *testing.B) {
	_, h := makePageBenchPool(b, 10000)
	benchmarkV1ProxyMaxIntPage(b, h, 10000)
}

func benchmarkNodePage(b *testing.B, h http.Handler, n int, pageSize int) {
	b.Helper()
	path := fmt.Sprintf("/api/nodes/page?page=1&page_size=%d&sort=score", pageSize)
	req := localTestRequest(http.MethodGet, path, nil)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("node page status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(n), "pool_size")
}

func benchmarkV1ProxyPage(b *testing.B, h http.Handler, pageSize int) {
	b.Helper()
	path := fmt.Sprintf("/api/v1/proxies?page=1&page_size=%d", pageSize)
	benchmarkPagePath(b, h, path, http.StatusOK)
}

func benchmarkNodeMaxIntPage(b *testing.B, h http.Handler, n int) {
	b.Helper()
	maxInt := int(^uint(0) >> 1)
	path := fmt.Sprintf("/api/nodes/page?page=%d&page_size=1&sort=score", maxInt)
	benchmarkPagePath(b, h, path, http.StatusOK)
	b.ReportMetric(float64(n), "pool_size")
}

func benchmarkV1ProxyMaxIntPage(b *testing.B, h http.Handler, n int) {
	b.Helper()
	maxInt := int(^uint(0) >> 1)
	path := fmt.Sprintf("/api/v1/proxies?page=%d&page_size=1", maxInt)
	benchmarkPagePath(b, h, path, http.StatusBadRequest)
	b.ReportMetric(float64(n), "pool_size")
}

func benchmarkPagePath(b *testing.B, h http.Handler, path string, wantStatus int) {
	b.Helper()
	req := localTestRequest(http.MethodGet, path, nil)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != wantStatus {
			b.Fatalf("GET %s status = %d, want %d; body=%s", path, w.Code, wantStatus, w.Body.String())
		}
	}
}

// TestStatusPageCollectorsStayBounded is the allocation regression guard for
// first-page and huge-page requests. The window assertions exercise the exact
// collector capacities; this test additionally catches accidental restoration
// of full NodeView/V1ProxyView slices at handler level.
func TestStatusPageCollectorsStayBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skip allocation regression test in short mode")
	}
	maxInt := int(^uint(0) >> 1)
	for _, n := range []int{100, 1000, 10000} {
		_, h := buildBenchPool(n)
		paths := []struct {
			name       string
			path       string
			wantStatus int
		}{
			{name: "node_first", path: "/api/nodes/page?page=1&page_size=1&sort=score", wantStatus: http.StatusOK},
			{name: "node_max_int", path: fmt.Sprintf("/api/nodes/page?page=%d&page_size=1&sort=score", maxInt), wantStatus: http.StatusOK},
			{name: "v1_first", path: "/api/v1/proxies?page=1&page_size=1", wantStatus: http.StatusOK},
			{name: "v1_max_int", path: fmt.Sprintf("/api/v1/proxies?page=%d&page_size=1", maxInt), wantStatus: http.StatusBadRequest},
		}
		for _, test := range paths {
			t.Run(fmt.Sprintf("%s/pool_%d", test.name, n), func(t *testing.T) {
				req := localTestRequest(http.MethodGet, test.path, nil)
				var before, after runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				const iterations = 5
				for i := 0; i < iterations; i++ {
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)
					if w.Code != test.wantStatus {
						t.Fatalf("status %d, want %d: %s", w.Code, test.wantStatus, w.Body.String())
					}
				}
				runtime.ReadMemStats(&after)
				perRequest := int64(after.TotalAlloc-before.TotalAlloc) / iterations
				t.Logf("total_alloc/req=%dB allocs/req~=%.0f", perRequest, float64(after.Mallocs-before.Mallocs)/iterations)
				if n == 10000 {
					const fullViewBaseline = 26 * 1024 * 1024
					if perRequest >= fullViewBaseline/10 {
						t.Errorf("allocated %dB/req, expected < %dB (full-view collector regression)", perRequest, fullViewBaseline/10)
					}
				}
			})
		}
	}
}

// TestV1ProxySnapshotIDIsStableAndMatchesHeader verifies that identical pool
// states produce the same non-empty snapshot token and that the response header
// carries the same token as the response body.
func TestV1ProxySnapshotIDIsStableAndMatchesHeader(t *testing.T) {
	_, h := buildBenchPool(50)
	fetch := func() string {
		req := localTestRequest(http.MethodGet, "/api/v1/proxies?page=1&page_size=1", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		var page V1ProxyPage
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return page.SnapshotID
	}
	first := fetch()
	if first == "" {
		t.Fatal("empty snapshot id")
	}
	// Identical state yields identical token within process.
	if got := fetch(); got != first {
		t.Fatalf("snapshot id drifted: first=%q second=%q", first, got)
	}
	// Header and body must agree.
	req := localTestRequest(http.MethodGet, "/api/v1/proxies?page=1&page_size=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("X-Snapshot-ID"); got != first {
		t.Fatalf("header X-Snapshot-ID %q != body %q", got, first)
	}
}
