package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckCandidateUsesCredentialExitGeoAndAnonymityChain(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	record := func(value string) {
		mu.Lock()
		calls = append(calls, value)
		mu.Unlock()
	}
	installCandidateCheckSeams(t,
		func(_ context.Context, px Proxy, testURL string, timeout time.Duration) (Proxy, bool, error) {
			record("credential")
			if testURL != "https://health.example/check" || timeout != time.Second {
				t.Fatalf("credential check options = %q, %s", testURL, timeout)
			}
			px.Username, px.Password = "verified", "secret"
			return px, true, nil
		},
		func(_ context.Context, px Proxy, timeout time.Duration) string {
			record("exit")
			if px.Username != "verified" || timeout != time.Second {
				t.Fatalf("exit probe proxy/timeout = %+v, %s", px, timeout)
			}
			return "203.0.113.20"
		},
		func(_ context.Context, ip string, timeout time.Duration) (string, string, string) {
			record("geo")
			if ip != "203.0.113.20" || timeout != time.Second {
				t.Fatalf("geo probe target/timeout = %q, %s", ip, timeout)
			}
			return "JP", "Tokyo", "AS"
		},
		func(_ context.Context, px Proxy, timeout time.Duration) string {
			record("anonymity")
			if px.ExitIP != "203.0.113.20" || px.Country != "JP" || timeout != time.Second {
				t.Fatalf("anonymity probe proxy/timeout = %+v, %s", px, timeout)
			}
			return "elite"
		},
	)

	px := Proxy{IP: "198.51.100.10", Port: "1080", Protocol: "socks5"}
	outcome := checkCandidateContext(context.Background(), px, candidateCheckOptions{
		Timeout: time.Second, RequireIPChange: true, TestURL: "https://health.example/check", BaselineIP: "198.51.100.1",
	})
	if outcome.Kind != candidateCheckAlive || outcome.Key != px.Key() {
		t.Fatalf("candidate outcome = %+v, want alive", outcome)
	}
	if got := outcome.Proxy; got.Username != "verified" || got.Password != "secret" || got.ExitIP != "203.0.113.20" || !got.IPChangeKnown || !got.IPChanged || got.Country != "JP" || got.City != "Tokyo" || got.Continent != "AS" || got.Anonymity != "elite" {
		t.Fatalf("checked proxy = %+v", got)
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	wantCalls := []string{"credential", "exit", "geo", "anonymity"}
	if fmt.Sprint(gotCalls) != fmt.Sprint(wantCalls) {
		t.Fatalf("check order = %v, want %v", gotCalls, wantCalls)
	}
}

func TestCheckCandidateClassifiesConnectionFailureWithSummary(t *testing.T) {
	installCandidateCheckSeams(t,
		func(context.Context, Proxy, string, time.Duration) (Proxy, bool, error) {
			return Proxy{}, false, errors.New("upstream refused CONNECT\nresponse body")
		}, nil, nil, nil,
	)
	px := Proxy{IP: "198.51.100.11", Port: "8080", Protocol: "http"}
	outcome := checkCandidateContext(context.Background(), px, candidateCheckOptions{Timeout: time.Second, TestURL: "https://health.example/check"})
	if outcome.Kind != candidateCheckUnreachable || outcome.Key != px.Key() {
		t.Fatalf("candidate outcome = %+v, want unreachable", outcome)
	}
	if outcome.Error != "upstream refused CONNECT response body" {
		t.Fatalf("failure summary = %q", outcome.Error)
	}
}

func TestCheckCandidateClassifiesIPChangePolicySeparately(t *testing.T) {
	installCandidateCheckSeams(t,
		func(_ context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			return px, true, nil
		},
		func(context.Context, Proxy, time.Duration) string { return "203.0.113.30" },
		nil,
		nil,
	)
	px := Proxy{IP: "198.51.100.12", Port: "8080", Protocol: "http"}
	outcome := checkCandidateContext(context.Background(), px, candidateCheckOptions{
		Timeout: time.Second, TestURL: "https://health.example/check", RequireIPChange: true, BaselineIP: "203.0.113.30",
	})
	if outcome.Kind != candidateCheckPolicyFiltered || !outcome.Proxy.IPChangeKnown || outcome.Proxy.IPChanged {
		t.Fatalf("candidate outcome = %+v, want policy filtered", outcome)
	}
	if outcome.Error == "" {
		t.Fatal("policy failure did not include an operator-facing summary")
	}
}

func TestCheckCandidateParentCancellationHasNoFailureOutcome(t *testing.T) {
	started := make(chan struct{})
	installCandidateCheckSeams(t,
		func(ctx context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			close(started)
			<-ctx.Done()
			return px, false, ctx.Err()
		}, nil, nil, nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan candidateCheckOutcome, 1)
	go func() {
		result <- checkCandidateContext(ctx, Proxy{IP: "198.51.100.13", Port: "8080", Protocol: "http"}, candidateCheckOptions{Timeout: time.Second, TestURL: "https://health.example/check"})
	}()
	<-started
	cancel()
	select {
	case outcome := <-result:
		if outcome.Kind != candidateCheckNoResult {
			t.Fatalf("cancelled candidate outcome = %+v, want no result", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled candidate check did not stop")
	}
}

func TestCheckCandidateDeadlineIsUnreachableWhenParentRemainsActive(t *testing.T) {
	installCandidateCheckSeams(t,
		func(ctx context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			<-ctx.Done()
			return px, false, ctx.Err()
		}, nil, nil, nil,
	)
	px := Proxy{IP: "198.51.100.14", Port: "8080", Protocol: "http"}
	outcome := checkCandidateContext(context.Background(), px, candidateCheckOptions{Timeout: 20 * time.Millisecond, TestURL: "https://health.example/check"})
	if outcome.Kind != candidateCheckUnreachable || !errors.Is(errors.New(outcome.Error), context.DeadlineExceeded) && outcome.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("deadline candidate outcome = %+v, want unreachable deadline", outcome)
	}
}

func TestCheckCandidateBatchNeverExceedsConfiguredConcurrency(t *testing.T) {
	var active, maximum, calls atomic.Int64
	installCandidateCheckSeams(t,
		func(ctx context.Context, px Proxy, _ string, _ time.Duration) (Proxy, bool, error) {
			calls.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}
			select {
			case <-time.After(15 * time.Millisecond):
				return px, true, nil
			case <-ctx.Done():
				return px, false, ctx.Err()
			}
		},
		func(context.Context, Proxy, time.Duration) string { return "" },
		func(context.Context, string, time.Duration) (string, string, string) { return "", "", "" },
		func(context.Context, Proxy, time.Duration) string { return "" },
	)
	proxies := make([]Proxy, 18)
	for i := range proxies {
		proxies[i] = Proxy{IP: fmt.Sprintf("198.51.100.%d", i+20), Port: "8080", Protocol: "http"}
	}
	outcomes := checkCandidateBatchContext(context.Background(), proxies, candidateCheckOptions{
		Timeout: time.Second, MaxConcurrent: 3, TestURL: "https://health.example/check",
	}, nil)
	if len(outcomes) != len(proxies) || calls.Load() != int64(len(proxies)) {
		t.Fatalf("batch outcomes/calls = %d/%d, want %d", len(outcomes), calls.Load(), len(proxies))
	}
	if maximum.Load() != 3 {
		t.Fatalf("maximum check concurrency = %d, want 3", maximum.Load())
	}
	for _, outcome := range outcomes {
		if outcome.Kind != candidateCheckAlive {
			t.Fatalf("batch outcome = %+v, want alive", outcome)
		}
	}
}

func installCandidateCheckSeams(
	t *testing.T,
	credential func(context.Context, Proxy, string, time.Duration) (Proxy, bool, error),
	exit func(context.Context, Proxy, time.Duration) string,
	geo func(context.Context, string, time.Duration) (string, string, string),
	anonymity func(context.Context, Proxy, time.Duration) string,
) {
	t.Helper()
	oldCredential := candidateCheckCredentialCandidates
	oldExit := candidateCheckProbeExitIP
	oldGeo := candidateCheckLookupGeo
	oldAnonymity := candidateCheckProbeAnonymity
	if credential != nil {
		candidateCheckCredentialCandidates = credential
	}
	if exit != nil {
		candidateCheckProbeExitIP = exit
	}
	if geo != nil {
		candidateCheckLookupGeo = geo
	}
	if anonymity != nil {
		candidateCheckProbeAnonymity = anonymity
	}
	t.Cleanup(func() {
		candidateCheckCredentialCandidates = oldCredential
		candidateCheckProbeExitIP = oldExit
		candidateCheckLookupGeo = oldGeo
		candidateCheckProbeAnonymity = oldAnonymity
	})
}
