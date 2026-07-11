package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type staticSourceResolver struct {
	addresses []net.IPAddr
	err       error
}

func (r staticSourceResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return r.addresses, r.err
}

func TestSourceIPPolicyRejectsPrivateAndReservedRanges(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "192.0.2.1", "192.168.1.1", "198.18.0.1", "198.51.100.1",
		"203.0.113.1", "224.0.0.1", "255.255.255.255", "::", "::1", "100::1",
		"2001:db8::1", "fc00::1", "fe80::1", "ff02::1",
	} {
		if ip := net.ParseIP(raw); ip == nil || !isDisallowedSourceIP(ip) {
			t.Errorf("address %q was not rejected", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if ip := net.ParseIP(raw); ip == nil || isDisallowedSourceIP(ip) {
			t.Errorf("public address %q was rejected", raw)
		}
	}
}

func TestValidateSourceURLRequiresExplicitPrivateEscapeHatch(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/feed",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/feed",
	} {
		if _, err := validateSourceURL(raw, false); err == nil || !strings.Contains(err.Error(), "allow_private=true") {
			t.Errorf("validateSourceURL(%q, false) error = %v", raw, err)
		}
		if _, err := validateSourceURL(raw, true); err != nil {
			t.Errorf("validateSourceURL(%q, true) error = %v", raw, err)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "http://example.com/feed#secret", "http://example.com:0/feed"} {
		if _, err := validateSourceURL(raw, true); err == nil {
			t.Errorf("validateSourceURL accepted invalid URL %q", raw)
		}
	}
}

func TestGuardedSourceDialRejectsDNSAnswersBeforeConnecting(t *testing.T) {
	resolver := staticSourceResolver{addresses: []net.IPAddr{
		{IP: net.ParseIP("8.8.8.8")},
		{IP: net.ParseIP("127.0.0.1")},
	}}
	dial := guardedSourceDialContext(resolver, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", "source.example:80")
	if conn != nil {
		conn.Close()
		t.Fatal("guarded dial unexpectedly connected")
	}
	if err == nil || !strings.Contains(err.Error(), "private or reserved") {
		t.Fatalf("guarded dial error = %v", err)
	}
}

func TestSourceRedirectPolicyRejectsPrivateTargetAndCapsChain(t *testing.T) {
	privateRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/internal", nil)
	if err := sourceRedirectPolicy(false)(privateRequest, nil); err == nil {
		t.Fatal("redirect policy accepted a private target")
	}
	if err := sourceRedirectPolicy(true)(privateRequest, nil); err != nil {
		t.Fatalf("explicit private redirect error = %v", err)
	}
	via := make([]*http.Request, 5)
	if err := sourceRedirectPolicy(true)(httptest.NewRequest(http.MethodGet, "https://example.com/feed", nil), via); err == nil {
		t.Fatal("redirect policy accepted an overlong chain")
	}
}

func TestProductionFetchBlocksPrivateURLUnlessExplicitlyAllowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("10.0.0.7:8080\n"))
	}))
	defer server.Close()

	source := testPlainListSource(server.URL)
	if _, err := FetchSource(source); err == nil {
		t.Fatal("FetchSource accepted a loopback source without allow_private")
	}
	source.AllowPrivate = true
	proxies, err := FetchSource(source)
	if err != nil {
		t.Fatalf("FetchSource explicit private source error = %v", err)
	}
	if len(proxies) != 1 || proxies[0].IP != "10.0.0.7" {
		t.Fatalf("private proxy advertised by trusted feed was removed: %#v", proxies)
	}
}

func TestProductionSourceFetchConcurrencyIsBounded(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 12)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte("8.8.8.8:8080\n"))
	}))
	defer server.Close()

	const requests = 8
	errorsCh := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			source := testPlainListSource(server.URL)
			source.Name = fmt.Sprintf("concurrency-%d", time.Now().UnixNano())
			source.AllowPrivate = true
			_, err := FetchSource(source)
			errorsCh <- err
		}()
	}

	deadline := time.After(3 * time.Second)
	for i := 0; i < maxConcurrentSourceFetches; i++ {
		select {
		case <-started:
		case <-deadline:
			close(release)
			t.Fatal("timed out waiting for bounded source fetches to start")
		}
	}
	// No fifth request may reach the server until one of the first four exits.
	select {
	case <-started:
		close(release)
		t.Fatalf("more than %d source requests ran concurrently", maxConcurrentSourceFetches)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Errorf("FetchSource error = %v", err)
		}
	}
	if got := maximum.Load(); got != maxConcurrentSourceFetches {
		t.Fatalf("maximum concurrent source fetches = %d, want %d", got, maxConcurrentSourceFetches)
	}
}
