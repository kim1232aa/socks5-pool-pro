package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCandidateSpeedtestRouteValidatesEmptyAndBatchLimit(t *testing.T) {
	pool := NewProxyPool()
	server := NewStatusServer(pool, &ConfigStore{})
	handler := server.handler()

	method := httptest.NewRecorder()
	handler.ServeHTTP(method, localTestRequest(http.MethodGet, "/api/candidates/speedtest", nil))
	if method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET candidate speedtest = %d Allow=%q", method.Code, method.Header().Get("Allow"))
	}

	empty := httptest.NewRecorder()
	handler.ServeHTTP(empty, localTestRequest(http.MethodPost, "/api/candidates/speedtest", strings.NewReader(`{"keys":[]}`)))
	if empty.Code != http.StatusBadRequest || !strings.Contains(empty.Body.String(), `"code":"invalid_candidate_speedtest_request"`) {
		t.Fatalf("empty candidate speedtest = %d %s", empty.Code, empty.Body.String())
	}

	keys := make([]string, maxConcurrentNodeSpeedTests+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("http://198.51.100.%d:8080", i+1)
	}
	over := invokeCandidateSpeedtest(t, server, keys, context.Background())
	if over.status != http.StatusBadRequest || !strings.Contains(over.raw, `"code":"candidate_speedtest_batch_too_large"`) {
		t.Fatalf("oversized candidate speedtest = %d %s", over.status, over.raw)
	}
}

func TestCandidateSpeedtestDeduplicatesAndPassesProtocolCredentials(t *testing.T) {
	httpCandidate := Proxy{IP: "198.51.100.20", Port: "8080", Protocol: "http", Username: "http-user", Password: "http-secret"}
	socksCandidate := Proxy{IP: "198.51.100.20", Port: "8080", Protocol: "socks5", Username: "socks-user", Password: "socks-secret"}
	pool := NewProxyPool()
	seedCandidateSpeedCatalog(pool, []Proxy{httpCandidate, socksCandidate})
	server := NewStatusServer(pool, &ConfigStore{})

	var mu sync.Mutex
	seen := make(map[string]Proxy)
	installCandidateSpeedTest(t, func(_ context.Context, px Proxy, timeout time.Duration) (SpeedTestResult, Proxy, error) {
		if timeout != speedTestOperationTimeout {
			t.Errorf("candidate speed timeout = %s, want %s", timeout, speedTestOperationTimeout)
		}
		mu.Lock()
		seen[px.Key()] = px
		mu.Unlock()
		return SpeedTestResult{Kbps: 2048, Bytes: speedTestMaxBytes, DurationMs: 4000}, px, nil
	})

	result := invokeCandidateSpeedtest(t, server, []string{httpCandidate.Key(), httpCandidate.Key(), socksCandidate.Key()}, context.Background())
	if result.status != http.StatusOK || len(result.body.Results) != 2 {
		t.Fatalf("deduplicated candidate speedtest = %d %#v raw=%s", result.status, result.body, result.raw)
	}
	if result.body.Results[0].Key != httpCandidate.Key() || result.body.Results[1].Key != socksCandidate.Key() {
		t.Fatalf("deduplicated result order = %#v", result.body.Results)
	}
	mu.Lock()
	gotHTTP, gotSocks := seen[httpCandidate.Key()], seen[socksCandidate.Key()]
	mu.Unlock()
	if len(seen) != 2 || gotHTTP.Protocol != "http" || gotHTTP.Username != "http-user" || gotHTTP.Password != "http-secret" {
		t.Fatalf("HTTP candidate passed to speed test = %#v; all=%#v", gotHTTP, seen)
	}
	if gotSocks.Protocol != "socks5" || gotSocks.Username != "socks-user" || gotSocks.Password != "socks-secret" {
		t.Fatalf("SOCKS candidate passed to speed test = %#v", gotSocks)
	}
	if pool.Size() != 2 {
		t.Fatalf("successful candidate speedtest pool size = %d, want 2", pool.Size())
	}
	for _, candidate := range []Proxy{httpCandidate, socksCandidate} {
		promoted, ok := pool.Find(candidate.Key())
		if !ok || !promoted.Available || promoted.SpeedKbps != 2048 || promoted.SpeedBytes != speedTestMaxBytes || promoted.SpeedDurationMs != 4000 || promoted.SpeedTestedAt == 0 {
			t.Fatalf("promoted candidate %q = %#v, ok=%v", candidate.Key(), promoted, ok)
		}
	}
}
func TestCandidateSpeedtestRequiresExactProtocolAwareKey(t *testing.T) {
	candidate := Proxy{IP: "198.51.100.21", Port: "8080", Protocol: "http"}
	pool := NewProxyPool()
	seedCandidateSpeedCatalog(pool, []Proxy{candidate})
	server := NewStatusServer(pool, &ConfigStore{})
	var calls atomic.Int64
	installCandidateSpeedTest(t, func(_ context.Context, px Proxy, _ time.Duration) (SpeedTestResult, Proxy, error) {
		calls.Add(1)
		return SpeedTestResult{}, px, nil
	})

	result := invokeCandidateSpeedtest(t, server, []string{"HTTP://" + candidate.Addr()}, context.Background())
	if result.status != http.StatusOK || len(result.body.Results) != 1 || result.body.Results[0].Error == nil || result.body.Results[0].Error.Code != "candidate_not_found" {
		t.Fatalf("non-exact candidate key = %d %#v raw=%s", result.status, result.body, result.raw)
	}
	if calls.Load() != 0 {
		t.Fatalf("non-exact candidate key reached speed test %d time(s)", calls.Load())
	}
}

func TestCandidateSpeedtestRunsSixteenConcurrentlyAndNeverQueuesOverflow(t *testing.T) {
	candidates := make([]Proxy, maxConcurrentNodeSpeedTests+1)
	keys := make([]string, maxConcurrentNodeSpeedTests)
	for i := range candidates {
		candidates[i] = Proxy{IP: fmt.Sprintf("198.51.100.%d", i+1), Port: "8080", Protocol: "http"}
		if i < len(keys) {
			keys[i] = candidates[i].Key()
		}
	}
	pool := NewProxyPool()
	seedCandidateSpeedCatalog(pool, candidates)
	server := NewStatusServer(pool, &ConfigStore{})

	entered := make(chan struct{}, maxConcurrentNodeSpeedTests)
	release := make(chan struct{})
	var calls, active, maxActive atomic.Int64
	installCandidateSpeedTest(t, func(ctx context.Context, px Proxy, _ time.Duration) (SpeedTestResult, Proxy, error) {
		calls.Add(1)
		current := active.Add(1)
		for {
			observed := maxActive.Load()
			if current <= observed || maxActive.CompareAndSwap(observed, current) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			active.Add(-1)
			return SpeedTestResult{}, px, ctx.Err()
		}
		active.Add(-1)
		return SpeedTestResult{Kbps: 1024, Bytes: speedTestMaxBytes, DurationMs: 8000}, px, nil
	})

	firstDone := make(chan candidateSpeedtestInvocation, 1)
	go func() {
		firstDone <- invokeCandidateSpeedtest(t, server, keys, context.Background())
	}()
	for i := range maxConcurrentNodeSpeedTests {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatalf("only %d/%d candidate tests started concurrently", i, maxConcurrentNodeSpeedTests)
		}
	}
	if got := maxActive.Load(); got != maxConcurrentNodeSpeedTests {
		close(release)
		t.Fatalf("maximum active candidate speedtests = %d, want %d", got, maxConcurrentNodeSpeedTests)
	}

	overflowDone := make(chan candidateSpeedtestInvocation, 1)
	go func() {
		overflowDone <- invokeCandidateSpeedtest(t, server, []string{candidates[len(candidates)-1].Key()}, context.Background())
	}()
	select {
	case overflow := <-overflowDone:
		if overflow.status != http.StatusOK || len(overflow.body.Results) != 1 || overflow.body.Results[0].Error == nil || overflow.body.Results[0].Error.Code != "candidate_speedtest_busy" {
			close(release)
			t.Fatalf("overflow candidate speedtest = %d %#v raw=%s", overflow.status, overflow.body, overflow.raw)
		}
	case <-time.After(200 * time.Millisecond):
		close(release)
		t.Fatal("overflow candidate speedtest queued instead of returning immediately")
	}
	if got := calls.Load(); got != maxConcurrentNodeSpeedTests {
		close(release)
		t.Fatalf("speed test calls while slots full = %d, want %d", got, maxConcurrentNodeSpeedTests)
	}

	close(release)
	select {
	case first := <-firstDone:
		if first.status != http.StatusOK || len(first.body.Results) != maxConcurrentNodeSpeedTests {
			t.Fatalf("concurrent candidate speedtest = %d %#v raw=%s", first.status, first.body, first.raw)
		}
		for _, item := range first.body.Results {
			if !item.OK || item.Bytes != speedTestMaxBytes || item.Error != nil {
				t.Fatalf("concurrent candidate item = %#v", item)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent candidate speedtest did not complete after release")
	}
	if pool.Size() != maxConcurrentNodeSpeedTests {
		t.Fatalf("batch candidate speedtest added %d node(s), want %d", pool.Size(), maxConcurrentNodeSpeedTests)
	}
}

func TestCandidateSpeedtestReturnsPartialFailuresWithoutCancellingPeers(t *testing.T) {
	candidates := []Proxy{
		{IP: "198.51.100.31", Port: "8080", Protocol: "http"},
		{IP: "198.51.100.32", Port: "8080", Protocol: "http"},
		{IP: "198.51.100.33", Port: "8080", Protocol: "http"},
	}
	pool := NewProxyPool()
	seedCandidateSpeedCatalog(pool, candidates)
	server := NewStatusServer(pool, &ConfigStore{})
	var calls atomic.Int64
	installCandidateSpeedTest(t, func(_ context.Context, px Proxy, _ time.Duration) (SpeedTestResult, Proxy, error) {
		calls.Add(1)
		if px.Key() == candidates[1].Key() {
			return SpeedTestResult{}, px, errors.New("upstream refused CONNECT")
		}
		return SpeedTestResult{Kbps: 4096, Bytes: speedTestMaxBytes, DurationMs: 2000}, px, nil
	})

	keys := []string{candidates[0].Key(), candidates[1].Key(), "http://198.51.100.99:8080", candidates[2].Key()}
	result := invokeCandidateSpeedtest(t, server, keys, context.Background())
	if result.status != http.StatusOK || len(result.body.Results) != len(keys) || calls.Load() != 3 {
		t.Fatalf("partial candidate speedtest = %d calls=%d body=%#v raw=%s", result.status, calls.Load(), result.body, result.raw)
	}
	if !result.body.Results[0].OK || result.body.Results[0].Error != nil {
		t.Fatalf("first successful item = %#v", result.body.Results[0])
	}
	if result.body.Results[1].OK || result.body.Results[1].Error == nil || result.body.Results[1].Error.Code != "speedtest_failed" {
		t.Fatalf("failed item = %#v", result.body.Results[1])
	}
	if result.body.Results[2].OK || result.body.Results[2].Error == nil || result.body.Results[2].Error.Code != "candidate_not_found" {
		t.Fatalf("missing item = %#v", result.body.Results[2])
	}
	if !result.body.Results[3].OK {
		t.Fatalf("failure cancelled later peer: %#v", result.body.Results[3])
	}
	server.speedMu.Lock()
	_, failedRunning := server.speedRunning[candidates[1].Key()]
	_, failedCooldown := server.speedCooldownUntil[candidates[1].Key()]
	server.speedMu.Unlock()
	if failedRunning || failedCooldown || len(server.speedSlots) != 0 {
		t.Fatalf("failed candidate retained running=%v cooldown=%v slots=%d", failedRunning, failedCooldown, len(server.speedSlots))
	}
	retry := invokeCandidateSpeedtest(t, server, []string{candidates[1].Key()}, context.Background())
	if retry.status != http.StatusOK || len(retry.body.Results) != 1 || retry.body.Results[0].Error == nil || retry.body.Results[0].Error.Code != "speedtest_failed" {
		t.Fatalf("failed candidate immediate manual retry = %d %#v raw=%s", retry.status, retry.body, retry.raw)
	}
	if pool.Size() != 2 {
		t.Fatalf("partial candidate speedtest pool size = %d, want 2 successes", pool.Size())
	}
}

func TestCandidateSpeedtestCancellationStopsEveryWorker(t *testing.T) {
	candidates := []Proxy{
		{IP: "198.51.100.41", Port: "8080", Protocol: "http"},
		{IP: "198.51.100.42", Port: "8080", Protocol: "http"},
		{IP: "198.51.100.43", Port: "8080", Protocol: "http"},
	}
	pool := NewProxyPool()
	seedCandidateSpeedCatalog(pool, candidates)
	server := NewStatusServer(pool, &ConfigStore{})
	started := make(chan struct{}, len(candidates))
	var stopped atomic.Int64
	installCandidateSpeedTest(t, func(ctx context.Context, px Proxy, _ time.Duration) (SpeedTestResult, Proxy, error) {
		started <- struct{}{}
		<-ctx.Done()
		stopped.Add(1)
		return SpeedTestResult{}, px, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan candidateSpeedtestInvocation, 1)
	keys := []string{candidates[0].Key(), candidates[1].Key(), candidates[2].Key()}
	go func() {
		done <- invokeCandidateSpeedtest(t, server, keys, ctx)
	}()
	for range candidates {
		select {
		case <-started:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("candidate speedtest worker did not start")
		}
	}
	cancel()
	select {
	case result := <-done:
		if result.status != http.StatusRequestTimeout || !strings.Contains(result.raw, `"code":"request_cancelled"`) {
			t.Fatalf("cancelled candidate speedtest = %d %s", result.status, result.raw)
		}
	case <-time.After(time.Second):
		t.Fatal("candidate speedtest request did not stop after cancellation")
	}
	if stopped.Load() != int64(len(candidates)) {
		t.Fatalf("stopped candidate speedtest workers = %d, want %d", stopped.Load(), len(candidates))
	}
	if got := len(server.speedSlots); got != 0 {
		t.Fatalf("occupied speed slots after cancellation = %d", got)
	}
	server.speedMu.Lock()
	running := len(server.speedRunning)
	server.speedMu.Unlock()
	if running != 0 {
		t.Fatalf("running candidate speedtests after cancellation = %d", running)
	}
	for _, candidate := range candidates {
		if err := server.beginSpeedTest(candidate.Key()); err != nil {
			t.Fatalf("cancelled candidate %q retained cooldown or running state: %v", candidate.Key(), err)
		}
		server.endSpeedTest(candidate.Key(), false)
	}

}

func TestCandidateSpeedtestInternalTimeoutIsAnItemFailure(t *testing.T) {
	candidate := Proxy{IP: "198.51.100.49", Port: "8080", Protocol: "http"}
	pool := NewProxyPool()
	seedCandidateSpeedCatalog(pool, []Proxy{candidate})
	server := NewStatusServer(pool, &ConfigStore{})
	installCandidateSpeedTest(t, func(_ context.Context, px Proxy, _ time.Duration) (SpeedTestResult, Proxy, error) {
		return SpeedTestResult{}, px, context.DeadlineExceeded
	})

	result := invokeCandidateSpeedtest(t, server, []string{candidate.Key()}, context.Background())
	if result.status != http.StatusOK || len(result.body.Results) != 1 {
		t.Fatalf("internally timed-out candidate speedtest = %d %#v raw=%s", result.status, result.body, result.raw)
	}
	item := result.body.Results[0]
	if item.OK || item.Error == nil || item.Error.Code != "speedtest_failed" || !strings.Contains(item.Error.Message, context.DeadlineExceeded.Error()) {
		t.Fatalf("internally timed-out candidate item = %#v", item)
	}
	if err := server.beginCandidateSpeedTest(candidate.Key()); err != nil {
		t.Fatalf("manual candidate retry was cooled down: %v", err)
	}
	server.endSpeedTest(candidate.Key(), false)
}

func TestCandidateSpeedtestRejectsRunningButBypassesNodeCooldown(t *testing.T) {
	candidate := Proxy{IP: "198.51.100.50", Port: "8080", Protocol: "http"}
	pool := NewProxyPool()
	seedCandidateSpeedCatalog(pool, []Proxy{candidate})
	server := NewStatusServer(pool, &ConfigStore{})
	var calls atomic.Int64
	installCandidateSpeedTest(t, func(_ context.Context, px Proxy, _ time.Duration) (SpeedTestResult, Proxy, error) {
		calls.Add(1)
		return SpeedTestResult{Kbps: 1024, Bytes: speedTestMaxBytes, DurationMs: 3000}, px, nil
	})

	if err := server.beginSpeedTest(candidate.Key()); err != nil {
		t.Fatal(err)
	}
	busy := invokeCandidateSpeedtest(t, server, []string{candidate.Key()}, context.Background())
	if busy.status != http.StatusOK || len(busy.body.Results) != 1 || busy.body.Results[0].Error == nil || busy.body.Results[0].Error.Code != "candidate_speedtest_busy" {
		server.endSpeedTest(candidate.Key())
		t.Fatalf("running candidate speedtest = %d %#v raw=%s", busy.status, busy.body, busy.raw)
	}
	server.endSpeedTest(candidate.Key())

	retry := invokeCandidateSpeedtest(t, server, []string{candidate.Key()}, context.Background())
	if retry.status != http.StatusOK || len(retry.body.Results) != 1 || !retry.body.Results[0].OK || calls.Load() != 1 {
		t.Fatalf("candidate manual retry bypassing node cooldown = %d calls=%d %#v raw=%s", retry.status, calls.Load(), retry.body, retry.raw)
	}
}

type candidateSpeedtestInvocation struct {
	status int
	body   candidateSpeedtestResponse
	raw    string
}

func invokeCandidateSpeedtest(t *testing.T, server *StatusServer, keys []string, ctx context.Context) candidateSpeedtestInvocation {
	t.Helper()
	payload, err := json.Marshal(candidateSpeedtestRequest{Keys: keys})
	if err != nil {
		t.Fatalf("marshal candidate speedtest request: %v", err)
	}
	recorder := httptest.NewRecorder()
	request := localTestRequest(http.MethodPost, "/api/candidates/speedtest", bytes.NewReader(payload)).WithContext(ctx)
	server.handler().ServeHTTP(recorder, request)
	result := candidateSpeedtestInvocation{status: recorder.Code, raw: recorder.Body.String()}
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &result.body); err != nil {
			t.Fatalf("decode candidate speedtest response: %v; body=%s", err, recorder.Body.String())
		}
	}
	return result
}

func seedCandidateSpeedCatalog(pool *ProxyPool, candidates []Proxy) {
	refresh := pool.candidates.begin(candidates, nil, nil, 0)
	pool.candidates.complete(refresh, nil, nil, nil)
}

func installCandidateSpeedTest(t *testing.T, fn func(context.Context, Proxy, time.Duration) (SpeedTestResult, Proxy, error)) {
	t.Helper()
	previous := candidateSpeedTestContext
	candidateSpeedTestContext = fn
	t.Cleanup(func() { candidateSpeedTestContext = previous })
}
