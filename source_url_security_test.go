package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSafeSourceURLRedactsUserinfoAndSensitiveQueryValues(t *testing.T) {
	raw := "https://api-user:api-password@example.test/list?" +
		"token=token-value&api_key=key-value&client-secret=secret-value&" +
		"password=password-value&auth=auth-value&X-Amz-Signature=signature-value&" +
		"credential=credential-value&user=query-user&proxy_pass=proxy-pass-value&" +
		"subscription-key=subscription-key-value&plain=visible&monkey=banana&compass=north"
	safe := safeSourceURL(raw)

	for _, secret := range []string{
		"api-user", "api-password", "token-value", "key-value", "secret-value",
		"password-value", "auth-value", "signature-value", "credential-value", "query-user",
		"proxy-pass-value", "subscription-key-value",
	} {
		if strings.Contains(safe, secret) {
			t.Errorf("safe URL leaked %q: %s", secret, safe)
		}
	}
	u, err := url.Parse(safe)
	if err != nil {
		t.Fatal(err)
	}
	if u.User == nil || u.User.Username() != redactedURLValue {
		t.Fatalf("safe userinfo = %#v, want redacted marker", u.User)
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		t.Fatal("safe URL unexpectedly retains a password component")
	}
	query := u.Query()
	for _, key := range []string{"token", "api_key", "client-secret", "password", "auth", "X-Amz-Signature", "credential", "user", "proxy_pass", "subscription-key"} {
		if got := query.Get(key); got != redactedURLValue {
			t.Errorf("query %q = %q, want redacted marker", key, got)
		}
	}
	for _, key := range []string{"plain", "monkey", "compass"} {
		if got := query.Get(key); got != redactedURLValue {
			t.Errorf("custom query %q = %q, want redacted marker", key, got)
		}
	}
}

func TestSafeSourceURLFailsClosedForMalformedOrInvalidURL(t *testing.T) {
	malformed := safeSourceURL("https://example.test/list?token=do-not-leak;plain=value")
	if strings.Contains(malformed, "do-not-leak") || strings.Contains(malformed, "plain=value") {
		t.Fatalf("malformed query was echoed: %q", malformed)
	}
	if got := safeSourceURL("://not a URL?token=do-not-leak"); got != "[invalid source URL]" {
		t.Fatalf("invalid URL = %q", got)
	}
}

func TestSourcesManagementOutputIsRedactedWithoutMutatingStore(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rawURL := "https://management-user:management-pass@example.test/list?token=management-token&plain=visible"
	added, err := store.AddSource(Source{
		Name: "private-source", URL: rawURL, Format: FormatPlainList, Protocol: "http",
	})
	if err != nil {
		t.Fatal(err)
	}

	handler := NewStatusServer(NewProxyPool(), store).handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/sources", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/sources status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, secret := range []string{"management-user", "management-pass", "management-token"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Errorf("GET /api/sources leaked %q: %s", secret, recorder.Body.String())
		}
	}
	var sources []Source
	if err := jsonUnmarshalTest(recorder.Body.Bytes(), &sources); err != nil {
		t.Fatal(err)
	}
	var displayed Source
	for _, source := range sources {
		if source.ID == added.ID {
			displayed = source
			break
		}
	}
	if displayed.ID == "" {
		t.Fatalf("custom source missing from response: %#v", sources)
	}
	displayURL, err := url.Parse(displayed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if displayURL.User == nil || displayURL.User.Username() != redactedURLValue || displayURL.Query().Get("token") != redactedURLValue || displayURL.Query().Get("plain") != redactedURLValue {
		t.Fatalf("display URL not correctly redacted: %q", displayed.URL)
	}

	stored := store.Sources()
	for _, source := range stored {
		if source.ID == added.ID && source.URL != rawURL {
			t.Fatalf("stored fetch URL was mutated: %q, want %q", source.URL, rawURL)
		}
	}

	// The HTML dashboard uses the same management-safe projection.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET / status = %d", recorder.Code)
	}
	for _, secret := range []string{"management-user", "management-pass", "management-token"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Errorf("dashboard leaked %q", secret)
		}
	}
}

func jsonUnmarshalTest(data []byte, dst interface{}) error {
	return json.Unmarshal(data, dst)
}

func captureDefaultLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	buffer := &bytes.Buffer{}
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(buffer)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})
	return buffer
}

func TestFetchSourceUsesOriginalURLButLogsOnlyRedactedURL(t *testing.T) {
	const (
		username = "fetch-user-sensitive"
		password = "fetch-pass-sensitive"
		token    = "fetch-token-sensitive"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPassword, ok := r.BasicAuth()
		if !ok || gotUser != username || gotPassword != password {
			t.Errorf("fetch BasicAuth = %q/%q/%v", gotUser, gotPassword, ok)
		}
		if got := r.URL.Query().Get("token"); got != token {
			t.Errorf("fetch token = %q, want %q", got, token)
		}
		_, _ = w.Write([]byte("198.51.100.44:8080\n"))
	}))
	defer server.Close()

	u, err := url.Parse(server.URL + "/list")
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(username, password)
	query := u.Query()
	query.Set("token", token)
	query.Set("plain", "visible")
	u.RawQuery = query.Encode()

	logs := captureDefaultLogger(t)
	proxies, err := fetchSourceWithClient(testPlainListSource(u.String()), server.Client(), testSourceFetchPolicy(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 {
		t.Fatalf("proxies = %#v, want one", proxies)
	}
	for _, secret := range []string{username, password, token} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("fetch log leaked %q: %s", secret, logs.String())
		}
	}
	if strings.Contains(logs.String(), "plain=visible") || !strings.Contains(logs.String(), "plain="+redactedURLValue) || !strings.Contains(logs.String(), "token="+redactedURLValue) {
		t.Fatalf("fetch log lacks useful redacted URL: %s", logs.String())
	}
}

func TestFetchSourceRetryLogsAndReturnedErrorRedactURL(t *testing.T) {
	const (
		username = "retry-user-sensitive"
		password = "retry-pass-sensitive"
		token    = "retry-token-sensitive"
	)
	sentinel := errors.New("temporary network failure")
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("request to %s failed: %w", req.URL.String(), sentinel)
	})}
	rawURL := "https://" + username + ":" + password + "@source.example.test/list?token=" + token + "&plain=visible"
	logs := captureDefaultLogger(t)

	_, err := fetchSourceWithClient(testPlainListSource(rawURL), client, testSourceFetchPolicy(2))
	if err == nil {
		t.Fatal("fetchSourceWithClient error = nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("returned error lost original cause: %v", err)
	}
	combined := logs.String() + "\n" + err.Error()
	for _, secret := range []string{username, password, token} {
		if strings.Contains(combined, secret) {
			t.Errorf("retry log/error leaked %q: %s", secret, combined)
		}
	}
	if strings.Contains(combined, "plain=visible") || !strings.Contains(combined, "plain="+redactedURLValue) || !strings.Contains(combined, "token="+redactedURLValue) || !strings.Contains(combined, "temporary network failure") {
		t.Fatalf("redacted retry diagnostics missing: %s", combined)
	}
}

func TestSafeSourceURLRedactsFragmentAndUnknownQueryNames(t *testing.T) {
	safe := safeSourceURL("https://example.test/feed?code=unknown-secret&subscription=another-secret#fragment-secret")
	for _, secret := range []string{"unknown-secret", "another-secret", "fragment-secret"} {
		if strings.Contains(safe, secret) {
			t.Fatalf("safe URL leaked %q: %s", secret, safe)
		}
	}
	if !strings.Contains(safe, "code="+redactedURLValue) || !strings.Contains(safe, "subscription="+redactedURLValue) || !strings.HasSuffix(safe, "#"+redactedURLValue) {
		t.Fatalf("safe URL did not preserve useful redacted shape: %s", safe)
	}
}

func TestSafeLogLabelNeutralizesLegacyControlCharacters(t *testing.T) {
	got := safeLogLabel("feed\n[error] forged\tentry")
	if strings.ContainsAny(got, "\r\n\t") || !strings.Contains(got, "feed") || !strings.Contains(got, "forged") {
		t.Fatalf("safeLogLabel() = %q", got)
	}
}

func TestEnabledSourcesSanitizesLegacyLabelCopyOnly(t *testing.T) {
	raw := "legacy\n[error] forged"
	store := &ConfigStore{cfg: PoolConfig{Sources: []Source{{Name: raw, Enabled: true}}}}
	got := store.EnabledSources()
	if len(got) != 1 || strings.ContainsAny(got[0].Name, "\r\n\t") {
		t.Fatalf("EnabledSources label = %#v", got)
	}
	if stored := store.Sources()[0].Name; stored != raw {
		t.Fatalf("EnabledSources mutated persisted label = %q", stored)
	}
}
