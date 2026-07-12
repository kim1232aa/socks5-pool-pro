package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type trackingReadCloser struct {
	io.Reader
	closed *atomic.Int32
}

func (b *trackingReadCloser) Close() error {
	b.closed.Add(1)
	return nil
}

func testSourceFetchPolicy(attempts int) sourceFetchPolicy {
	return sourceFetchPolicy{
		Attempts:     attempts,
		TotalTimeout: 2 * time.Second,
		RetryDelay:   0,
	}
}

func testPlainListSource(sourceURL string) Source {
	return Source{
		Name:     "retry-test",
		URL:      sourceURL,
		Format:   FormatPlainList,
		Protocol: "http",
	}
}

func TestFetchSourceRetriesTemporaryFailureThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "temporary upstream failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("8.8.8.8:8080\n"))
	}))
	defer server.Close()

	proxies, err := fetchSourceWithClient(testPlainListSource(server.URL), server.Client(), testSourceFetchPolicy(3))
	if err != nil {
		t.Fatalf("fetchSourceWithClient() error = %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if len(proxies) != 1 || proxies[0].Key() != "http://8.8.8.8:8080" {
		t.Fatalf("proxies = %#v, want the successful retry result", proxies)
	}
}

func TestFetchSourceDoesNotRetryPermanent4xx(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	_, err := fetchSourceWithClient(testPlainListSource(server.URL), server.Client(), testSourceFetchPolicy(5))
	if err == nil {
		t.Fatal("fetchSourceWithClient() error = nil, want permanent 403 failure")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want no retry for 403", got)
	}
	if !strings.Contains(err.Error(), "unexpected status: 403") {
		t.Fatalf("error = %q, want 403 status", err)
	}
}

func TestFetchSourceRetriesTransientFyvriArchive404ThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	const fyvriArchivePath = "/fyvri/fresh-proxy-list/archive/storage/classic/http.json"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != fyvriArchivePath {
			http.Error(w, "unexpected archive path", http.StatusBadRequest)
			return
		}
		if attempts.Add(1) == 1 {
			http.Error(w, "archive edge has not propagated", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("1.1.1.1:8080\n"))
	}))
	defer server.Close()

	proxies, err := fetchSourceWithClient(testPlainListSource(server.URL+fyvriArchivePath), server.Client(), testSourceFetchPolicy(3))
	if err != nil {
		t.Fatalf("fetchSourceWithClient() error = %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want one bounded retry", got)
	}
	if len(proxies) != 1 || proxies[0].Addr() != "1.1.1.1:8080" {
		t.Fatalf("proxies = %#v, want successful retry result", proxies)
	}
}

func TestFetchSourceReturnsFinalRetryFailure(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "still unavailable", http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := fetchSourceWithClient(testPlainListSource(server.URL), server.Client(), testSourceFetchPolicy(3))
	if err == nil {
		t.Fatal("fetchSourceWithClient() error = nil, want final retry failure")
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if !strings.Contains(err.Error(), "after 3 attempt(s)") || !strings.Contains(err.Error(), "unexpected status: 502") {
		t.Fatalf("error = %q, want attempt count and final status", err)
	}
}

func TestFetchSourceClosesEveryRetryResponseBody(t *testing.T) {
	var attempts atomic.Int32
	var closed atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempt := attempts.Add(1)
		status := http.StatusServiceUnavailable
		body := "temporary failure"
		if attempt == 3 {
			status = http.StatusOK
			body = "8.8.4.4:8080\n"
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       &trackingReadCloser{Reader: strings.NewReader(body), closed: &closed},
			Request:    req,
		}, nil
	})}

	proxies, err := fetchSourceWithClient(testPlainListSource("http://source.test/list"), client, testSourceFetchPolicy(3))
	if err != nil {
		t.Fatalf("fetchSourceWithClient() error = %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("proxies = %#v, want one", proxies)
	}
	if got := closed.Load(); got != 3 {
		t.Fatalf("closed response bodies = %d, want 3", got)
	}
}

func TestRetryableSourceStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if !isRetryableSourceStatus(status) {
			t.Errorf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity} {
		if isRetryableSourceStatus(status) {
			t.Errorf("status %d should not be retryable", status)
		}
	}
}

func TestProductionSourceFetchBudgetsCoverSlowArchiveBodies(t *testing.T) {
	if sourceFetchAttemptTimeout < 45*time.Second {
		t.Fatalf("attempt timeout = %s, want at least 45s for multi-MiB archive bodies", sourceFetchAttemptTimeout)
	}
	retryBudget := time.Duration(0)
	for attempt := 1; attempt < sourceFetchAttempts; attempt++ {
		retryBudget += sourceFetchRetryDelay * time.Duration(1<<(attempt-1))
	}
	minimumTotal := time.Duration(sourceFetchAttempts)*sourceFetchAttemptTimeout + retryBudget
	if sourceFetchTotalTimeout < minimumTotal {
		t.Fatalf("total timeout = %s, cannot cover three %s attempts plus retry backoff", sourceFetchTotalTimeout, sourceFetchAttemptTimeout)
	}
	if sourceFetchQueueTimeout < 5*time.Minute {
		t.Fatalf("queue timeout = %s, want at least 5m behind four bounded slots", sourceFetchQueueTimeout)
	}
	if maxConcurrentSourceFetches != 4 || maxFetchBytes != 64<<20 {
		t.Fatalf("resource bounds changed: concurrency=%d max-bytes=%d", maxConcurrentSourceFetches, maxFetchBytes)
	}
}

func TestFetchSourceAllowsSlowResponseBodyWithinAttemptBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(80 * time.Millisecond)
		_, _ = w.Write([]byte("9.9.9.9:8080\n"))
	}))
	defer server.Close()

	client := server.Client()
	client.Timeout = 500 * time.Millisecond
	proxies, err := fetchSourceWithClient(testPlainListSource(server.URL), client, sourceFetchPolicy{
		Attempts:     1,
		TotalTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("slow response body should complete within the attempt budget: %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("proxies = %#v, want one parsed response", proxies)
	}
}

func TestRetryableNetworkErrorIncludesDNSErrors(t *testing.T) {
	err := &url.Error{
		Op:  "Get",
		URL: "https://raw.githubusercontent.com/example/list.txt",
		Err: &net.DNSError{Err: "temporary failure in name resolution", Name: "raw.githubusercontent.com", IsTemporary: true},
	}
	if !isRetryableNetworkError(err) {
		t.Fatalf("DNS error should be retryable: %v", err)
	}
}
