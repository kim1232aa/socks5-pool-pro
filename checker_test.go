package main

import (
	"net"
	"testing"
	"time"
)

func TestCheckProxiesIgnoresNonForwardingProxyIPResources(t *testing.T) {
	resource := Proxy{IP: "127.0.0.1", Port: "1", Protocol: "proxyip"}
	alive, unreachable := CheckProxies([]Proxy{resource}, 50*time.Millisecond, 1, false, "http://example.invalid/")
	if len(alive) != 0 || len(unreachable) != 0 {
		t.Fatalf("ProxyIP resource entered forwarding health result: alive=%#v unreachable=%#v", alive, unreachable)
	}
}

func TestCheckProxiesReportsFailuresByProtocolAwareKey(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	httpProxy := Proxy{IP: host, Port: port, Protocol: "http"}
	socksProxy := Proxy{IP: host, Port: port, Protocol: "socks5"}

	alive, failed := CheckProxies(
		[]Proxy{httpProxy, socksProxy},
		500*time.Millisecond,
		2,
		false,
		"http://health.test/check",
	)
	if len(alive) != 0 {
		t.Fatalf("abruptly-closed proxy endpoints reported alive: %#v", alive)
	}
	if len(failed) != 2 {
		t.Fatalf("failed keys = %#v, want one key per protocol", failed)
	}
	for _, px := range []Proxy{httpProxy, socksProxy} {
		if !failed[px.Key()] {
			t.Errorf("missing protocol-aware failed key %q in %#v", px.Key(), failed)
		}
	}
	if failed[httpProxy.Addr()] {
		t.Errorf("failure map unexpectedly contains protocol-agnostic address key %q", httpProxy.Addr())
	}
}
