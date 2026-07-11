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
		_, _ = w.Write([]byte("198.51.100.10:8080\n"))
	}))
	defer server.Close()

	proxies, err := fetchSourceWithClient(testPlainListSource(server.URL), server.Client(), testSourceFetchPolicy(3))
	if err != nil {
		t.Fatalf("fetchSourceWithClient() error = %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if len(proxies) != 1 || proxies[0].Key() != "http://198.51.100.10:8080" {
		t.Fatalf("proxies = %#v, want the successful retry result", proxies)
	}
}

func TestFetchSourceDoesNotRetryPermanent4xx(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	_, err := fetchSourceWithClient(testPlainListSource(server.URL), server.Client(), testSourceFetchPolicy(5))
	if err == nil {
		t.Fatal("fetchSourceWithClient() error = nil, want permanent 404 failure")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want no retry for 404", got)
	}
	if !strings.Contains(err.Error(), "unexpected status: 404") {
		t.Fatalf("error = %q, want 404 status", err)
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
			body = "198.51.100.12:8080\n"
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
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if !isRetryableSourceStatus(status) {
			t.Errorf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity} {
		if isRetryableSourceStatus(status) {
			t.Errorf("status %d should not be retryable", status)
		}
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
