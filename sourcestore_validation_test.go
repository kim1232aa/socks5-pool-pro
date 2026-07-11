package main

import (
	"strings"
	"testing"
)

func TestAddSourceValidatesURLAndProtocol(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.AddSource(Source{
		Name: "bad-url", URL: "file:///etc/passwd", Format: FormatPlainList, Protocol: "http",
	}); err == nil {
		t.Fatal("AddSource accepted a non-HTTP source URL")
	}
	if _, err := store.AddSource(Source{
		Name: "bad-protocol", URL: "https://example.test/list", Format: FormatPlainList, Protocol: "proxyip",
	}); err == nil {
		t.Fatal("AddSource accepted proxyip as a forwarding source protocol")
	}
	added, err := store.AddSource(Source{
		Name: " good ", URL: " https://example.test/list ", Format: FormatPlainList, Protocol: "HTTP",
	})
	if err != nil {
		t.Fatalf("AddSource valid input error = %v", err)
	}
	if added.Name != "good" || added.URL != "https://example.test/list" || added.Protocol != "http" {
		t.Fatalf("AddSource did not normalize input: %#v", added)
	}
}

func TestAddSourcePrivateURLRequiresExplicitOptIn(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := Source{
		Name: "lan-feed", URL: "http://192.168.10.5/list", Format: FormatPlainList, Protocol: "http",
	}
	if _, err := store.AddSource(source); err == nil || !strings.Contains(err.Error(), "allow_private=true") {
		t.Fatalf("AddSource private URL error = %v", err)
	}
	source.AllowPrivate = true
	added, err := store.AddSource(source)
	if err != nil {
		t.Fatalf("AddSource explicit private URL error = %v", err)
	}
	if !added.AllowPrivate {
		t.Fatalf("AddSource lost allow_private: %#v", added)
	}
}

func TestAddSourceRejectsControlCharactersAndCapsNewSourceCount(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddSource(Source{
		Name: "feed\nforged-log", URL: "https://example.test/list", Format: FormatPlainList, Protocol: "http",
	}); err == nil {
		t.Fatal("AddSource accepted a control character in source name")
	}

	store.mu.Lock()
	store.cfg.Sources = make([]Source, maxConfiguredSources)
	store.mu.Unlock()
	if _, err := store.AddSource(Source{
		Name: "one-too-many", URL: "https://example.test/extra", Format: FormatPlainList, Protocol: "http",
	}); err == nil || !strings.Contains(err.Error(), "source limit") {
		t.Fatalf("AddSource over limit error = %v", err)
	}
	if got := len(store.Sources()); got != maxConfiguredSources {
		t.Fatalf("source limit failure mutated store: %d", got)
	}
}

func TestSetCheckURLRejectsEmbeddedCredentialsAndFragments(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"https://user:secret@example.test/health",
		"https://example.test/health#secret",
	} {
		if err := store.SetCheckURL(raw); err == nil {
			t.Errorf("SetCheckURL accepted %q", raw)
		}
	}
	if err := store.SetCheckURL("https://example.test/health?tenant=public"); err != nil {
		t.Fatalf("SetCheckURL rejected compatible query URL: %v", err)
	}
}
