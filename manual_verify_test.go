package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type manualVerifyResponse struct {
	Reachable           bool   `json:"reachable"`
	Available           bool   `json:"available"`
	Attempts            int    `json:"attempts"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LatencyMs           int64  `json:"latency_ms"`
	Country             string `json:"country"`
	LabelMatchKnown     bool   `json:"label_match_known"`
	LabelMatched        bool   `json:"label_matched"`
	Code                string `json:"code"`
	Error               string `json:"error"`
	PolicyExcluded      bool   `json:"policy_excluded"`
}

func TestManualNodeVerifyHonorsRequireIPChangePolicy(t *testing.T) {
	px := Proxy{IP: "192.0.2.69", Port: "1080", Protocol: "socks5", Available: false}
	pool := NewProxyPool()
	pool.SetRequireIPChangePolicy(true)
	pool.Prime([]Proxy{px}, nil)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})
	server.nodeVerifyOps = manualNodeVerifyOperations{
		checkURL: func(context.Context, Proxy, string, time.Duration) (bool, time.Duration) {
			return true, 10 * time.Millisecond
		},
		probeExitIP: func(context.Context, Proxy, time.Duration) string { return "203.0.113.69" },
		lookupGeo:   func(context.Context, string, time.Duration) (string, string, string) { return "US", "", "NA" },
	}
	baselineExitMu.Lock()
	previous := baselineExitIP
	baselineExitIP = "203.0.113.69"
	baselineExitMu.Unlock()
	t.Cleanup(func() {
		baselineExitMu.Lock()
		baselineExitIP = previous
		baselineExitMu.Unlock()
	})

	recorder := httptest.NewRecorder()
	server.handleNodeVerify(recorder, httptest.NewRequest(http.MethodPost, "/api/nodes/verify", bytes.NewBufferString(`{"key":"`+px.Key()+`"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("manual policy verify = %d %s", recorder.Code, recorder.Body.String())
	}
	var response manualVerifyResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Reachable || response.Available || !response.PolicyExcluded {
		t.Fatalf("manual policy response = %#v", response)
	}
	if selected, ok, _ := pool.Pick(GroupAny, nil); ok {
		t.Fatalf("manual verification revived transparent node: %+v", selected)
	}
}

func TestManualNodeVerifyPromotesWorkingCredentialCandidate(t *testing.T) {
	px := Proxy{
		IP: "192.0.2.68", Port: "1080", Protocol: "socks5", Available: false,
		Username: "old", Password: "wrong",
		CredentialAlternates: []ProxyCredential{{Username: "working", Password: "secret"}},
	}
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})
	var probeCredential ProxyCredential
	server.nodeVerifyOps = manualNodeVerifyOperations{
		checkURLCredentials: func(_ context.Context, got Proxy, target string, _ time.Duration) (Proxy, bool, time.Duration, error) {
			if target != "http://health.test/check" || got.Username != "old" {
				t.Fatalf("manual credential check input = %+v target=%q", got, target)
			}
			working := got
			working.Username, working.Password = "working", "secret"
			return got.promoteCredential(working), true, 7 * time.Millisecond, nil
		},
		probeExitIP: func(_ context.Context, got Proxy, _ time.Duration) string {
			probeCredential = ProxyCredential{Username: got.Username, Password: got.Password}
			return ""
		},
		lookupGeo: func(context.Context, string, time.Duration) (string, string, string) { return "", "", "" },
	}

	recorder := httptest.NewRecorder()
	server.handleNodeVerify(recorder, httptest.NewRequest(http.MethodPost, "/api/nodes/verify", bytes.NewBufferString(`{"key":"`+px.Key()+`"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("manual credential verify = %d %s", recorder.Code, recorder.Body.String())
	}
	if probeCredential != (ProxyCredential{Username: "working", Password: "secret"}) {
		t.Fatalf("exit probe credential = %+v", probeCredential)
	}
	got, ok := pool.Find(px.Key())
	if !ok || got.Username != "working" || got.Password != "secret" || !got.Available {
		t.Fatalf("pool credential after manual verify = %+v, found=%v", got, ok)
	}
	if len(got.CredentialAlternates) != 1 || got.CredentialAlternates[0].Username != "old" {
		t.Fatalf("pool alternate credentials after promotion = %#v", got.CredentialAlternates)
	}
}

func TestManualNodeVerifyRejectsDuplicateAndOverCapacityWork(t *testing.T) {
	nodes := make([]Proxy, maxManualNodeVerifyConcurrent+1)
	for i := range nodes {
		nodes[i] = Proxy{IP: "192.0.2." + strconv.Itoa(i+1), Port: "1080", Protocol: "socks5", Available: true}
	}
	pool := NewProxyPool()
	pool.Prime(nodes, nil)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})
	entered := make(chan struct{}, maxManualNodeVerifyConcurrent)
	release := make(chan struct{})
	server.nodeVerifyOps = manualNodeVerifyOperations{
		checkURL: func(ctx context.Context, _ Proxy, _ string, _ time.Duration) (bool, time.Duration) {
			entered <- struct{}{}
			select {
			case <-release:
				return true, time.Millisecond
			case <-ctx.Done():
				return false, 0
			}
		},
		probeExitIP: func(context.Context, Proxy, time.Duration) string { return "" },
		lookupGeo:   func(context.Context, string, time.Duration) (string, string, string) { return "", "", "" },
	}

	var workers sync.WaitGroup
	results := make(chan int, maxManualNodeVerifyConcurrent)
	for i := 0; i < maxManualNodeVerifyConcurrent; i++ {
		key := nodes[i].Key()
		workers.Add(1)
		go func() {
			defer workers.Done()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/nodes/verify", bytes.NewBufferString(`{"key":"`+key+`"}`))
			server.handleNodeVerify(recorder, request)
			results <- recorder.Code
		}()
	}
	for i := 0; i < maxManualNodeVerifyConcurrent; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("manual verification slots did not fill")
		}
	}

	for _, key := range []string{nodes[0].Key(), nodes[maxManualNodeVerifyConcurrent].Key()} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/nodes/verify", bytes.NewBufferString(`{"key":"`+key+`"}`))
		server.handleNodeVerify(recorder, request)
		if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" {
			close(release)
			t.Fatalf("busy manual verify %q = %d headers=%v body=%s", key, recorder.Code, recorder.Header(), recorder.Body.String())
		}
	}

	close(release)
	workers.Wait()
	close(results)
	for status := range results {
		if status != http.StatusOK {
			t.Fatalf("admitted manual verify status = %d", status)
		}
	}
	if got := len(server.nodeVerifySlots); got != 0 {
		t.Fatalf("manual verify slots after completion = %d", got)
	}
	server.nodeVerifyMu.Lock()
	defer server.nodeVerifyMu.Unlock()
	if got := len(server.nodeVerifyRunning); got != 0 {
		t.Fatalf("manual verify running keys after completion = %d", got)
	}
}

func TestManualNodeVerifyRetriesUntilSuccessAndUsesSuccessfulAttemptLatency(t *testing.T) {
	px := Proxy{IP: "192.0.2.70", Port: "1080", Protocol: "socks5", Available: false, LatencyMs: 999}
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	// A prior failure streak must be reset by the later successful manual
	// observation, even though its first transport attempt fails.
	pool.ObserveHealthResult(px.Key(), false, 0)
	pool.ObserveHealthResult(px.Key(), false, 0)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})
	var checks, exits, geos atomic.Int64
	var attemptTimeouts []time.Duration
	server.nodeVerifyOps = manualNodeVerifyOperations{
		checkURL: func(ctx context.Context, got Proxy, target string, timeout time.Duration) (bool, time.Duration) {
			attempt := checks.Add(1)
			attemptTimeouts = append(attemptTimeouts, timeout)
			if got.Key() != px.Key() || target != "http://health.test/check" {
				t.Errorf("check args proxy=%q target=%q timeout=%s", got.Key(), target, timeout)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("manual check attempt has no deadline")
			}
			if attempt == 1 {
				return false, 850 * time.Millisecond
			}
			return true, 137 * time.Millisecond
		},
		probeExitIP: func(_ context.Context, _ Proxy, timeout time.Duration) string {
			exits.Add(1)
			if timeout != manualNodeVerifyExitTimeout {
				t.Errorf("exit timeout = %s", timeout)
			}
			return ""
		},
		lookupGeo: func(context.Context, string, time.Duration) (string, string, string) {
			geos.Add(1)
			return "", "", ""
		},
	}

	body, status := invokeManualNodeVerify(t, server, px.Key(), context.Background())
	if status != http.StatusOK || !body.Reachable || !body.Available || body.Attempts != 2 || body.LatencyMs != 137 || body.ConsecutiveFailures != 0 {
		t.Fatalf("retry-success response status=%d body=%#v", status, body)
	}
	if checks.Load() != 2 || exits.Load() != 1 || geos.Load() != 0 {
		t.Fatalf("operation calls check/exit/geo = %d/%d/%d", checks.Load(), exits.Load(), geos.Load())
	}
	if len(attemptTimeouts) != 2 || attemptTimeouts[0] != 10*time.Second || attemptTimeouts[1] != 8*time.Second {
		t.Fatalf("attempt timeout sequence = %v, want [10s 8s]", attemptTimeouts)
	}
	updated, ok := pool.Find(px.Key())
	if !ok || !updated.Available || updated.LatencyMs != 137 {
		t.Fatalf("successful retry pool state = found=%v proxy=%#v", ok, updated)
	}
	if successes, failures := pool.StatsOf(px.Key()); successes != 1 || failures != 2 {
		t.Fatalf("successful retry stats = %d/%d, want 1/2", successes, failures)
	}
}

func TestManualNodeVerifyAllAttemptsFailAsOneHealthObservation(t *testing.T) {
	px := Proxy{IP: "192.0.2.71", Port: "8080", Protocol: "http", Available: true, LatencyMs: 321}
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})
	var checks, exits atomic.Int64
	var attemptTimeouts []time.Duration
	server.nodeVerifyOps = manualNodeVerifyOperations{
		checkURL: func(_ context.Context, _ Proxy, _ string, timeout time.Duration) (bool, time.Duration) {
			checks.Add(1)
			attemptTimeouts = append(attemptTimeouts, timeout)
			return false, 9 * time.Second
		},
		probeExitIP: func(context.Context, Proxy, time.Duration) string {
			exits.Add(1)
			return "203.0.113.1"
		},
		lookupGeo: func(context.Context, string, time.Duration) (string, string, string) {
			return "JP", "Tokyo", "AS"
		},
	}

	body, status := invokeManualNodeVerify(t, server, px.Key(), context.Background())
	if status != http.StatusOK || body.Reachable || !body.Available || body.Attempts != 3 || body.LatencyMs != 0 || body.ConsecutiveFailures != 1 {
		t.Fatalf("all-failed response status=%d body=%#v", status, body)
	}
	if checks.Load() != 3 || exits.Load() != 0 {
		t.Fatalf("all-failed calls check/exit = %d/%d", checks.Load(), exits.Load())
	}
	if len(attemptTimeouts) != 3 || attemptTimeouts[0] != 10*time.Second || attemptTimeouts[1] != 8*time.Second || attemptTimeouts[2] != 8*time.Second {
		t.Fatalf("attempt timeout sequence = %v, want [10s 8s 8s]", attemptTimeouts)
	}
	updated, ok := pool.Find(px.Key())
	if !ok || !updated.Available || updated.LatencyMs != 321 {
		t.Fatalf("debounced failed pool state = found=%v proxy=%#v", ok, updated)
	}
	if successes, failures := pool.StatsOf(px.Key()); successes != 0 || failures != 1 {
		t.Fatalf("all attempts counted as %d/%d observations, want 0/1", successes, failures)
	}
}

func TestManualNodeVerifyCancellationStopsRetriesWithoutHealthObservation(t *testing.T) {
	px := Proxy{IP: "192.0.2.72", Port: "1080", Protocol: "socks5", Available: true, LatencyMs: 222}
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})
	started := make(chan struct{})
	var checks atomic.Int64
	server.nodeVerifyOps = manualNodeVerifyOperations{
		checkURL: func(ctx context.Context, _ Proxy, _ string, _ time.Duration) (bool, time.Duration) {
			checks.Add(1)
			close(started)
			<-ctx.Done()
			return false, 0
		},
		probeExitIP: probeExitIPContext,
		lookupGeo:   LookupGeoContext,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		body   manualVerifyResponse
		status int
	}, 1)
	go func() {
		body, status := invokeManualNodeVerify(t, server, px.Key(), ctx)
		result <- struct {
			body   manualVerifyResponse
			status int
		}{body, status}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("manual verification did not start")
	}
	cancel()
	select {
	case got := <-result:
		if got.status != http.StatusRequestTimeout || got.body.Attempts != 1 || got.body.Error == "" {
			t.Fatalf("canceled response status=%d body=%#v", got.status, got.body)
		}
	case <-time.After(time.Second):
		t.Fatal("manual verification did not stop after cancellation")
	}
	if checks.Load() != 1 {
		t.Fatalf("checks after cancellation = %d, want 1", checks.Load())
	}
	updated, _ := pool.Find(px.Key())
	if !updated.Available || updated.LatencyMs != 222 {
		t.Fatalf("cancellation changed pool state: %#v", updated)
	}
	if successes, failures := pool.StatsOf(px.Key()); successes != 0 || failures != 0 {
		t.Fatalf("cancellation recorded health observation %d/%d", successes, failures)
	}
}

func TestManualNodeVerifyCancellationDuringRetryBackoffStopsNextAttempt(t *testing.T) {
	px := Proxy{IP: "192.0.2.74", Port: "8080", Protocol: "http", Available: true, LatencyMs: 333}
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})
	firstFinished := make(chan struct{})
	var checks atomic.Int64
	server.nodeVerifyOps = manualNodeVerifyOperations{
		checkURL: func(context.Context, Proxy, string, time.Duration) (bool, time.Duration) {
			if checks.Add(1) == 1 {
				close(firstFinished)
			}
			return false, time.Millisecond
		},
		probeExitIP: probeExitIPContext,
		lookupGeo:   LookupGeoContext,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		body   manualVerifyResponse
		status int
	}, 1)
	go func() {
		body, status := invokeManualNodeVerify(t, server, px.Key(), ctx)
		result <- struct {
			body   manualVerifyResponse
			status int
		}{body, status}
	}()
	select {
	case <-firstFinished:
	case <-time.After(time.Second):
		t.Fatal("first manual attempt did not finish")
	}
	// The first retry backoff is 200ms. Cancel well inside it and verify a
	// second transport attempt is never started.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case got := <-result:
		if got.status != http.StatusRequestTimeout || got.body.Attempts != 1 {
			t.Fatalf("backoff-canceled response status=%d body=%#v", got.status, got.body)
		}
	case <-time.After(time.Second):
		t.Fatal("retry backoff did not stop after cancellation")
	}
	if checks.Load() != 1 {
		t.Fatalf("attempts started after backoff cancellation = %d, want 1", checks.Load())
	}
	updated, _ := pool.Find(px.Key())
	if !updated.Available || updated.LatencyMs != 333 {
		t.Fatalf("backoff cancellation changed pool: %#v", updated)
	}
	if successes, failures := pool.StatsOf(px.Key()); successes != 0 || failures != 0 {
		t.Fatalf("backoff cancellation recorded observation %d/%d", successes, failures)
	}
}

func TestManualNodeVerifyRejectsResultFromPreviousHealthCriterion(t *testing.T) {
	px := Proxy{IP: "192.0.2.76", Port: "1080", Protocol: "socks5", Available: true, LatencyMs: 222}
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/old"}})
	started := make(chan struct{})
	release := make(chan struct{})
	server.nodeVerifyOps = manualNodeVerifyOperations{
		checkURL: func(ctx context.Context, _ Proxy, target string, _ time.Duration) (bool, time.Duration) {
			if target != "http://health.test/old" {
				t.Errorf("manual verification target = %q", target)
			}
			close(started)
			select {
			case <-release:
				return true, 17 * time.Millisecond
			case <-ctx.Done():
				return false, 0
			}
		},
		probeExitIP: func(context.Context, Proxy, time.Duration) string { return "" },
		lookupGeo:   func(context.Context, string, time.Duration) (string, string, string) { return "", "", "" },
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/nodes/verify", bytes.NewBufferString(`{"key":"`+px.Key()+`"}`))
	done := make(chan struct{})
	go func() {
		server.handleNodeVerify(recorder, request)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("manual verification did not start")
	}
	pool.InvalidateHealth()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stale manual verification did not return")
	}

	var response manualVerifyResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode stale manual verification: %v; body=%s", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusConflict || response.Code != "health_criterion_changed" || response.Error != "检测标准已改变，结果未应用" {
		t.Fatalf("stale manual verification = %d %#v", recorder.Code, response)
	}
	updated, ok := pool.Find(px.Key())
	if !ok || updated.Available || updated.LatencyMs != 222 {
		t.Fatalf("stale manual verification changed invalidated node: %#v", updated)
	}
	if successes, failures := pool.StatsOf(px.Key()); successes != 0 || failures != 0 {
		t.Fatalf("stale manual verification recorded observation %d/%d", successes, failures)
	}
}

func TestManualNodeVerifyCancellationDuringExitProbeDoesNotCommitSuccess(t *testing.T) {
	px := Proxy{IP: "192.0.2.73", Port: "1080", Protocol: "socks5", Available: false, LatencyMs: 444}
	pool := NewProxyPool()
	pool.Prime([]Proxy{px}, nil)
	server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})
	exitStarted := make(chan struct{})
	server.nodeVerifyOps = manualNodeVerifyOperations{
		checkURL: func(context.Context, Proxy, string, time.Duration) (bool, time.Duration) {
			return true, 31 * time.Millisecond
		},
		probeExitIP: func(ctx context.Context, _ Proxy, _ time.Duration) string {
			close(exitStarted)
			<-ctx.Done()
			return ""
		},
		lookupGeo: LookupGeoContext,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan int, 1)
	go func() {
		_, status := invokeManualNodeVerify(t, server, px.Key(), ctx)
		result <- status
	}()
	<-exitStarted
	cancel()
	select {
	case status := <-result:
		if status != http.StatusRequestTimeout {
			t.Fatalf("exit-canceled status = %d, want 408", status)
		}
	case <-time.After(time.Second):
		t.Fatal("exit probe did not stop after cancellation")
	}
	updated, _ := pool.Find(px.Key())
	if updated.Available || updated.LatencyMs != 444 {
		t.Fatalf("canceled exit probe committed success: %#v", updated)
	}
	if successes, failures := pool.StatsOf(px.Key()); successes != 0 || failures != 0 {
		t.Fatalf("canceled exit probe recorded %d/%d", successes, failures)
	}
}

func TestManualNodeVerifyUnknownGeoPreservesLabelsAndReportsUnknownMatch(t *testing.T) {
	tests := []struct {
		name                string
		previousCountry     string
		lookupCountry       string
		wantPoolCountry     string
		wantPoolCity        string
		wantPoolContinent   string
		wantResponseCountry string
	}{
		{
			name: "literal Unknown geo", previousCountry: "US", lookupCountry: "Unknown",
			wantPoolCountry: "US", wantPoolCity: "Existing City", wantPoolContinent: "NA",
		},
		{
			name: "empty geo", previousCountry: "US", lookupCountry: "",
			wantPoolCountry: "US", wantPoolCity: "Existing City", wantPoolContinent: "NA",
		},
		{
			name: "empty previous country", previousCountry: "", lookupCountry: "JP",
			wantPoolCountry: "JP", wantPoolCity: "Tokyo", wantPoolContinent: "AS", wantResponseCountry: "JP",
		},
		{
			name: "legacy Unknown previous country", previousCountry: "Unknown", lookupCountry: "JP",
			wantPoolCountry: "JP", wantPoolCity: "Tokyo", wantPoolContinent: "AS", wantResponseCountry: "JP",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			px := Proxy{
				IP: "192.0.2.75", Port: "1080", Protocol: "socks5", Available: true,
				ExitIP: "203.0.113.10", Country: test.previousCountry, City: "Existing City", Continent: "NA",
			}
			pool := NewProxyPool()
			pool.Prime([]Proxy{px}, nil)
			server := NewStatusServer(pool, &ConfigStore{cfg: PoolConfig{CheckURL: "http://health.test/check"}})
			server.nodeVerifyOps = manualNodeVerifyOperations{
				checkURL: func(context.Context, Proxy, string, time.Duration) (bool, time.Duration) {
					return true, 42 * time.Millisecond
				},
				probeExitIP: func(context.Context, Proxy, time.Duration) string {
					return "203.0.113.20"
				},
				lookupGeo: func(context.Context, string, time.Duration) (string, string, string) {
					return test.lookupCountry, "Tokyo", "AS"
				},
			}

			body, status := invokeManualNodeVerify(t, server, px.Key(), context.Background())
			if status != http.StatusOK || !body.Reachable || body.LabelMatchKnown || !body.LabelMatched || body.Country != test.wantResponseCountry {
				t.Fatalf("unknown-label response status=%d body=%#v", status, body)
			}
			updated, ok := pool.Find(px.Key())
			if !ok || updated.ExitIP != "203.0.113.20" || updated.Country != test.wantPoolCountry || updated.City != test.wantPoolCity || updated.Continent != test.wantPoolContinent {
				t.Fatalf("unknown geo overwrote/preserved wrong labels: found=%v proxy=%#v", ok, updated)
			}
		})
	}
}

func invokeManualNodeVerify(t *testing.T, server *StatusServer, key string, ctx context.Context) (manualVerifyResponse, int) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/nodes/verify", bytes.NewBufferString(`{"key":"`+key+`"}`)).WithContext(ctx)
	server.handleNodeVerify(recorder, request)
	var body manualVerifyResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode manual verify response: %v; body=%s", err, recorder.Body.String())
	}
	return body, recorder.Code
}
