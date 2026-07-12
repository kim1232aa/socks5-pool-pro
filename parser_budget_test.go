package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchSourceTreatsZeroValidRecordsAsFailureUnlessAllowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("999.999.999.999:70000\nnot-an-address\n"))
	}))
	defer server.Close()

	source := testPlainListSource(server.URL)
	_, err := fetchSourceWithClient(source, server.Client(), testSourceFetchPolicy(1))
	if !errors.Is(err, ErrSourceEmpty) {
		t.Fatalf("empty invalid 200 error = %v, want ErrSourceEmpty", err)
	}

	source.AllowEmpty = true
	proxies, err := fetchSourceWithClient(source, server.Client(), testSourceFetchPolicy(1))
	if err != nil {
		t.Fatalf("explicit authoritative empty source failed: %v", err)
	}
	if len(proxies) != 0 {
		t.Fatalf("authoritative empty source returned %#v", proxies)
	}
}

func TestPlainAndJSONArraySupportHostnameAndBracketedIPv6(t *testing.T) {
	plain, err := parsePlainList([]byte("proxy.example.test:3128\n[2001:db8::7]:1080\n"), "socks5")
	if err != nil {
		t.Fatal(err)
	}
	jsonList, err := parseJSONArray([]byte(`["proxy.example.test:3128","[2001:db8::7]:1080"]`), "socks5")
	if err != nil {
		t.Fatal(err)
	}
	for name, proxies := range map[string][]Proxy{"plain": plain, "json": jsonList} {
		if len(proxies) != 2 {
			t.Fatalf("%s proxies = %#v, want hostname and IPv6", name, proxies)
		}
		if proxies[0].IP != "proxy.example.test" || proxies[1].IP != "2001:db8::7" {
			t.Fatalf("%s addresses = %#v", name, proxies)
		}
	}
}

func TestSourceParserKeepsCredentialVariantsAtSameAddress(t *testing.T) {
	body := []byte(strings.Join([]string{
		"socks5://alice:first@198.51.100.40:1080",
		"socks5://alice:second@198.51.100.40:1080",
		"socks5://alice:first@198.51.100.40:1080",
	}, "\n"))
	proxies, err := parseTextRegex(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 2 || proxies[0].Password != "first" || proxies[1].Password != "second" {
		t.Fatalf("credential variants were discarded or duplicated: %#v", proxies)
	}

	edt, err := parseEDTJSON([]byte(`[
		{"proxy":"socks5://alice:first@198.51.100.40:1080"},
		{"proxy":"socks5://alice:second@198.51.100.40:1080"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(edt) != 2 {
		t.Fatalf("EDT credential variants = %#v, want two", edt)
	}
}

func TestSourceParserRejectsFieldAndRecordBudgetExpansion(t *testing.T) {
	oversizedURL := "socks5://alice:" + strings.Repeat("x", maxSourceProxyURLBytes) + "@proxy.example.test:1080"
	if _, err := parseTextRegex([]byte(oversizedURL)); !errors.Is(err, ErrSourceBudgetExceeded) {
		t.Fatalf("oversized text field error = %v, want ErrSourceBudgetExceeded", err)
	}

	var body strings.Builder
	body.Grow(maxSourceParsedRecords*4 + 2)
	body.WriteByte('[')
	for i := 0; i <= maxSourceParsedRecords; i++ {
		if i != 0 {
			body.WriteByte(',')
		}
		body.WriteString(`"x"`)
	}
	body.WriteByte(']')
	if _, err := parseJSONArray([]byte(body.String()), "http"); !errors.Is(err, ErrSourceBudgetExceeded) {
		t.Fatalf("oversized record set error = %v, want ErrSourceBudgetExceeded", err)
	}
}

func TestProxyIPPortArrayHasPerRecordBudget(t *testing.T) {
	ports := make([]string, maxProxyIPPortsPerRecord+1)
	for i := range ports {
		ports[i] = "443"
	}
	body := []byte(`{"data":[{"ip":"1.1.1.1","port":[` + strings.Join(ports, ",") + `]}]}`)
	if _, err := parseProxyIPJSON(body); !errors.Is(err, ErrSourceBudgetExceeded) {
		t.Fatalf("oversized ProxyIP port list error = %v, want ErrSourceBudgetExceeded", err)
	}
}

func TestStreamingJSONParsersRejectTrailingValues(t *testing.T) {
	if _, err := parseJSONArray([]byte(`["proxy.example:80"] {}`), "http"); err == nil {
		t.Fatal("json-array parser accepted a trailing JSON value")
	}
	if _, err := parseEDTJSON([]byte(`[] null`)); err == nil {
		t.Fatal("EDT parser accepted a trailing JSON value")
	}
	if _, err := parseProxyIPJSON([]byte(`{"data":[]} []`)); err == nil {
		t.Fatal("ProxyIP parser accepted a trailing JSON value")
	}
}
