package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFetchSourceContextCancelsBlockedRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(requestStarted)
		<-req.Context().Done()
		close(requestCanceled)
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := fetchSourceWithClientContext(ctx, testPlainListSource("http://source.test/list"), client, sourceFetchPolicy{
			Attempts:     3,
			TotalTimeout: time.Minute,
			RetryDelay:   time.Minute,
		})
		result <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("source request did not start")
	}
	cancel()
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("source request did not observe cancellation")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("fetch error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("source fetch did not exit after cancellation")
	}
}

func TestRefreshBaselineContextCancelsBlockedRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(requestStarted)
		<-serverDone
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan bool, 1)
	go func() {
		success, _ := refreshBaselineExitWithURLChangeContext(ctx, server.URL, time.Minute)
		result <- success
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("baseline request did not start")
	}
	cancel()
	select {
	case success := <-result:
		if success {
			t.Fatal("canceled baseline request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("baseline request did not exit after cancellation")
	}
	close(serverDone)
}

func TestRefreshCoordinatorShutdownRejectsNewOperationsAndFinalizesActiveWork(t *testing.T) {
	coordinator := newRefreshCoordinator()
	pool := NewProxyPool()
	pool.SetHealthCriterion("http://health.test/check")

	refresh, accepted := coordinator.requestRefresh()
	if !accepted {
		t.Fatal("refresh was not accepted before shutdown")
	}
	if got := coordinator.beginRefreshOperation(); got != refresh.ID {
		t.Fatalf("active refresh ID = %q, want %q", got, refresh.ID)
	}
	source := Source{ID: "source-a", Name: "A", Enabled: true}
	if _, accepted := coordinator.requestSourceRefresh(source, "manual"); !accepted {
		t.Fatal("source refresh was not accepted before shutdown")
	}
	if _, ok := coordinator.beginSourceRefresh(source.ID); !ok {
		t.Fatal("source refresh did not become active")
	}
	health, accepted := coordinator.triggerFullRecheck(pool)
	if !accepted {
		t.Fatal("health recheck was not accepted before shutdown")
	}
	coordinator.beginHealthRecheckOperation(pool, 1)

	coordinator.shutdown()

	if operation, accepted := coordinator.requestRefresh(); accepted || operation.Status != "cancelled" {
		t.Fatalf("post-shutdown refresh = (%+v, %v), want cancelled rejection", operation, accepted)
	}
	if operation, accepted := coordinator.requestSourceRefresh(source, "manual"); accepted || operation.Status != "cancelled" {
		t.Fatalf("post-shutdown source refresh = (%+v, %v), want cancelled rejection", operation, accepted)
	}
	if operation, accepted := coordinator.triggerFullRecheck(pool); accepted || operation.Status != "cancelled" {
		t.Fatalf("post-shutdown health recheck = (%+v, %v), want cancelled rejection", operation, accepted)
	}
	coordinator.triggerRecheck()
	if coordinator.drainRecheckSignalForTest() {
		t.Fatal("post-shutdown bounded recheck was queued")
	}

	refreshStatus := coordinator.refreshOperationStatus()
	if refreshStatus.State != "idle" || refreshStatus.Active != nil || refreshStatus.Last == nil || refreshStatus.Last.ID != refresh.ID || refreshStatus.Last.Status != "cancelled" {
		t.Fatalf("refresh status after shutdown = %+v", refreshStatus)
	}
	healthStatus := coordinator.healthRecheckOperationStatus()
	if healthStatus.State != "idle" || healthStatus.Active != nil || healthStatus.Last == nil || healthStatus.Last.ID != health.ID || healthStatus.Last.Status != "cancelled" {
		t.Fatalf("health status after shutdown = %+v", healthStatus)
	}
	coordinator.sourceRefreshMu.Lock()
	activeSources := len(coordinator.sourceRefreshActive)
	pendingSources := len(coordinator.sourceRefreshPending)
	coordinator.sourceRefreshMu.Unlock()
	if activeSources != 0 || pendingSources != 0 {
		t.Fatalf("source operations remain after shutdown: active=%d pending=%d", activeSources, pendingSources)
	}
}

func TestShutdownApplicationJoinsBackgroundBeforeFinalFlush(t *testing.T) {
	lifecycle := newBackgroundLifecycle(context.Background())
	coordinator := newRefreshCoordinator()
	workerStarted := make(chan struct{})
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	lifecycle.Go(func(ctx context.Context) {
		close(workerStarted)
		<-ctx.Done()
		record("worker joined")
	})
	<-workerStarted

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := shutdownApplication(ctx, coordinator, lifecycle, func(context.Context) error {
		if !coordinator.shuttingDown.Load() {
			t.Fatal("coordinator still admitted work while admission was stopping")
		}
		if lifecycle.Context().Err() != nil {
			t.Fatal("background cancellation started before admission stopped")
		}
		record("admission stopped")
		return nil
	}, func() error {
		record("cache flushed")
		return nil
	})
	if err != nil {
		t.Fatalf("shutdownApplication() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"admission stopped", "worker joined", "cache flushed"}
	if len(events) != len(want) {
		t.Fatalf("shutdown events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("shutdown events = %v, want %v", events, want)
		}
	}
}

func TestShutdownApplicationSkipsFlushWhenBackgroundJoinTimesOut(t *testing.T) {
	lifecycle := newBackgroundLifecycle(context.Background())
	release := make(chan struct{})
	lifecycle.Go(func(context.Context) { <-release })
	defer close(release)
	flushed := false
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := shutdownApplication(ctx, newRefreshCoordinator(), lifecycle, func(context.Context) error { return nil }, func() error {
		flushed = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdownApplication() error = %v, want deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "background workers did not stop before shutdown deadline") {
		t.Fatalf("shutdownApplication() error = %v, want observable join timeout", err)
	}
	if flushed {
		t.Fatal("cache flushed while a background worker could still mutate it")
	}
}

func TestWaitForShutdownReturnsAuxiliaryListenerFatalEvent(t *testing.T) {
	signalDone := make(chan struct{})
	serverErrors := make(chan error)
	fatalEvents := make(chan ListenerFatalEvent, 1)
	fatalErr := errors.New("accept failed")
	fatalEvents <- ListenerFatalEvent{ID: "listener-a", Addr: "127.0.0.1:1081", Err: fatalErr}

	err := waitForShutdown(signalDone, serverErrors, fatalEvents)
	if !errors.Is(err, fatalErr) || !strings.Contains(err.Error(), "listener-a") || !strings.Contains(err.Error(), "127.0.0.1:1081") {
		t.Fatalf("waitForShutdown() error = %v", err)
	}
}

func TestWaitForShutdownIgnoresAbsentExpectedStopEvent(t *testing.T) {
	signalDone := make(chan struct{})
	close(signalDone)
	if err := waitForShutdown(signalDone, make(chan error), make(chan ListenerFatalEvent)); err != nil {
		t.Fatalf("waitForShutdown() error = %v, want signal shutdown", err)
	}
}
