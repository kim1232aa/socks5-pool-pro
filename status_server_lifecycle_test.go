package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestStatusServerShutdownMakesNormalClosureSuccessful(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	server := NewStatusServer(NewProxyPool(), &ConfigStore{})
	startResult := make(chan error, 1)
	go func() { startResult <- server.Start(addr) }()

	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, requestErr := client.Get("http://" + addr + "/healthz")
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("status server did not become ready at %s: %v", addr, requestErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case err := <-startResult:
		if err != nil {
			t.Fatalf("Start() after graceful shutdown = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after Shutdown()")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
}

func TestStatusServerStartReturnsSynchronousBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	server := NewStatusServer(NewProxyPool(), &ConfigStore{})
	if err := server.Start(listener.Addr().String()); err == nil {
		t.Fatal("Start() unexpectedly succeeded on an address that is already bound")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() after bind failure = %v", err)
	}
}
