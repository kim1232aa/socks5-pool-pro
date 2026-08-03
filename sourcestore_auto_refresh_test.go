package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRawSourceConfig(t *testing.T, dir, sourceFields string) string {
	t.Helper()
	path := filepath.Join(dir, "pool_config.json")
	data := []byte(`{"sources":[{` + sourceFields + `}],"rules":[],"groups":[]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const autoRefreshTestSource = `"id":"legacy","name":"legacy","url":"https://example.test/list","format":"plain-list","protocol":"http","enabled":true,"builtin":false`

func TestSourceAutoRefreshLegacyConfigDefaultsEnabledAndMigrates(t *testing.T) {
	dir := t.TempDir()
	path := writeRawSourceConfig(t, dir, autoRefreshTestSource)

	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := store.SourceByID("legacy")
	if !ok || !source.AutoRefreshEnabled || source.RefreshIntervalSeconds != 0 {
		t.Fatalf("migrated source = %#v, found = %v", source, ok)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"auto_refresh_enabled": true`) || !strings.Contains(string(data), `"refresh_interval_seconds": 0`) {
		t.Fatalf("migration was not persisted: %s", data)
	}
}

func TestSourceAutoRefreshExplicitFalseSurvivesLoad(t *testing.T) {
	dir := t.TempDir()
	writeRawSourceConfig(t, dir, autoRefreshTestSource+`,"auto_refresh_enabled":false,"refresh_interval_seconds":0`)

	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := store.SourceByID("legacy")
	if !ok || source.AutoRefreshEnabled {
		t.Fatalf("explicit false was lost: %#v, found = %v", source, ok)
	}
}

func TestSetSourceAutoRefreshPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := store.Sources()[0].ID
	if err := store.SetSourceAutoRefresh(id, false, 300); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := restarted.SourceByID(id)
	if !ok || source.AutoRefreshEnabled || source.RefreshIntervalSeconds != 300 {
		t.Fatalf("restarted source = %#v, found = %v", source, ok)
	}
}

func TestSetSourceAutoRefreshRejectsUnknownIDAndInvalidIntervals(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := store.Sources()[0].ID
	if err := store.SetSourceAutoRefresh("missing", false, 60); err == nil || !strings.Contains(err.Error(), "source not found") {
		t.Fatalf("unknown source error = %v", err)
	}
	for _, interval := range []int{-1, 1, 59, 604801} {
		if err := store.SetSourceAutoRefresh(id, false, interval); err == nil {
			t.Errorf("interval %d was accepted", interval)
		}
	}
	for _, interval := range []int{0, 60, 604800} {
		if err := store.SetSourceAutoRefresh(id, true, interval); err != nil {
			t.Errorf("interval %d rejected: %v", interval, err)
		}
	}
}

func TestScheduleIntervalsPersistFallbackAndValidate(t *testing.T) {
	dir := t.TempDir()
	store, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.SourceRefreshInterval(20 * time.Minute); got != 20*time.Minute {
		t.Fatalf("default source refresh interval = %s", got)
	}
	if got := store.FullRecheckInterval(30 * time.Minute); got != 30*time.Minute {
		t.Fatalf("default full recheck interval = %s", got)
	}

	if err := store.SetScheduleIntervals(90, 120); err != nil {
		t.Fatal(err)
	}
	if got := store.SourceRefreshInterval(time.Hour); got != 90*time.Second {
		t.Fatalf("source refresh override = %s", got)
	}
	if got := store.FullRecheckInterval(time.Hour); got != 2*time.Minute {
		t.Fatalf("full recheck override = %s", got)
	}

	restarted, err := NewConfigStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.SourceRefreshInterval(time.Hour) != 90*time.Second || restarted.FullRecheckInterval(time.Hour) != 2*time.Minute {
		t.Fatalf("restarted schedule = source %s full %s", restarted.SourceRefreshInterval(time.Hour), restarted.FullRecheckInterval(time.Hour))
	}

	for _, intervals := range [][2]int{{-1, 120}, {59, 120}, {604801, 120}, {90, -1}, {90, 59}, {90, 604801}} {
		if err := store.SetScheduleIntervals(intervals[0], intervals[1]); err == nil {
			t.Errorf("schedule intervals %v were accepted", intervals)
		}
	}
	if err := store.SetScheduleIntervals(0, 0); err != nil {
		t.Fatal(err)
	}
	if store.SourceRefreshInterval(20*time.Minute) != 20*time.Minute || store.FullRecheckInterval(30*time.Minute) != 30*time.Minute {
		t.Fatal("zero schedule overrides did not restore CLI fallbacks")
	}
}

func TestSetSourceAutoRefreshWriteFailureLeavesMemoryUnchanged(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := store.Sources()[0].ID
	before, _ := store.SourceByID(id)
	store.path = ""

	if err := store.SetSourceAutoRefresh(id, false, 120); err == nil {
		t.Fatal("SetSourceAutoRefresh succeeded despite unwritable path")
	}
	after, _ := store.SourceByID(id)
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(afterJSON) != string(beforeJSON) {
		t.Fatalf("memory changed after persistence failure: before=%s after=%s", beforeJSON, afterJSON)
	}
}

func TestDefaultSourcesEnableAutoRefresh(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range store.Sources() {
		if !source.AutoRefreshEnabled {
			t.Fatalf("default source %q has auto refresh disabled", source.ID)
		}
	}
}

func TestAdditionalDefaultProxySourcesContract(t *testing.T) {
	type expectedSource struct {
		url, format, protocol string
	}
	expected := map[string]expectedSource{
		"builtin-databay-http":          {"https://raw.githubusercontent.com/databay-labs/free-proxy-list/master/http.txt", FormatPlainList, "http"},
		"builtin-databay-socks5":        {"https://raw.githubusercontent.com/databay-labs/free-proxy-list/master/socks5.txt", FormatPlainList, "socks5"},
		"builtin-proxyscrape-http":      {"https://cdn.jsdelivr.net/gh/proxyscrape/free-proxy-list@main/proxies/protocols/http/data.txt", FormatTextRegex, ""},
		"builtin-proxyscrape-socks5":    {"https://cdn.jsdelivr.net/gh/proxyscrape/free-proxy-list@main/proxies/protocols/socks5/data.txt", FormatTextRegex, ""},
		"builtin-thespeedx-http":        {"https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/http.txt", FormatPlainList, "http"},
		"builtin-thespeedx-socks5":      {"https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/socks5.txt", FormatPlainList, "socks5"},
		"builtin-vpslab-http":           {"https://raw.githubusercontent.com/VPSLabCloud/VPSLab-Free-Proxy-List/main/http_all.txt", FormatPlainList, "http"},
		"builtin-vpslab-socks5":         {"https://raw.githubusercontent.com/VPSLabCloud/VPSLab-Free-Proxy-List/main/socks5_all.txt", FormatPlainList, "socks5"},
		"builtin-gproxynet-http":        {"https://raw.githubusercontent.com/gproxynet/free-proxy-list/main/http.txt", FormatPlainList, "http"},
		"builtin-gproxynet-socks5":      {"https://raw.githubusercontent.com/gproxynet/free-proxy-list/main/socks5.txt", FormatPlainList, "socks5"},
		"builtin-proxygenerator-http":   {"https://raw.githubusercontent.com/proxygenerator1/ProxyGenerator/main/Stable/http.txt", FormatPlainList, "http"},
		"builtin-proxygenerator-socks5": {"https://raw.githubusercontent.com/proxygenerator1/ProxyGenerator/main/MostStable/socks5.txt", FormatPlainList, "socks5"},
	}

	sources := defaultPoolConfig().Sources
	if len(sources) > maxConfiguredSources {
		t.Fatalf("default source count = %d, max = %d", len(sources), maxConfiguredSources)
	}
	seenIDs := make(map[string]bool, len(sources))
	seenURLs := make(map[string]string, len(sources))
	for _, source := range sources {
		if seenIDs[source.ID] {
			t.Fatalf("duplicate default source ID %q", source.ID)
		}
		seenIDs[source.ID] = true
		if other := seenURLs[source.URL]; other != "" {
			t.Fatalf("duplicate default source URL %q on %q and %q", source.URL, other, source.ID)
		}
		seenURLs[source.URL] = source.ID
		want, ok := expected[source.ID]
		if !ok {
			continue
		}
		if source.URL != want.url || source.Format != want.format || source.Protocol != want.protocol || !source.Builtin || !source.Enabled || !source.AutoRefreshEnabled {
			t.Fatalf("default source %q = %#v, want URL=%q format=%q protocol=%q enabled builtin auto-refresh", source.ID, source, want.url, want.format, want.protocol)
		}
		delete(expected, source.ID)
	}
	if len(expected) != 0 {
		t.Fatalf("missing additional default sources: %#v", expected)
	}
}

func TestAdditionalSourceFormatsRecognizeRepresentativeRows(t *testing.T) {
	plain, err := parsePlainList([]byte("# generated\n51.159.28.39:80\n\n[2001:4860:4860::8888]:1080\n"), "http")
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 2 || plain[0].Protocol != "http" || plain[0].Addr() != "51.159.28.39:80" || plain[1].Addr() != "[2001:4860:4860::8888]:1080" {
		t.Fatalf("plain-list parsed = %#v", plain)
	}

	withScheme, err := parseTextRegex([]byte("http://95.211.64.139:8888\nsocks5://46.146.216.44:1080\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(withScheme) != 2 || withScheme[0].Protocol != "http" || withScheme[1].Protocol != "socks5" {
		t.Fatalf("text-regex parsed = %#v", withScheme)
	}
}
