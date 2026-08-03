package main

import (
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parseConfigArgsForTest(t *testing.T, args ...string) *Config {
	t.Helper()
	previousFlags := flag.CommandLine
	previousArgs := os.Args
	flag.CommandLine = flag.NewFlagSet("socks5-pool-test", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"socks5-pool-test"}, args...)
	defer func() {
		flag.CommandLine = previousFlags
		os.Args = previousArgs
	}()
	return ParseConfig()
}

func TestParseConfigScheduleDefaultsAndOverrides(t *testing.T) {
	unsetCredentialTestEnv(t,
		"SOCKS_USER", "SOCKS_USER_FILE", "SOCKS_PASS", "SOCKS_PASS_FILE",
		"ADMIN_USER", "ADMIN_USER_FILE", "ADMIN_PASS", "ADMIN_PASS_FILE", "PORT",
	)

	defaults := parseConfigArgsForTest(t)
	if defaults.ScrapeInterval != 20*time.Minute || defaults.FullRecheckInterval != 30*time.Minute {
		t.Fatalf("schedule defaults = source %s full %s", defaults.ScrapeInterval, defaults.FullRecheckInterval)
	}

	overrides := parseConfigArgsForTest(t, "-scrape-interval=45m", "-full-recheck-interval=2h")
	if overrides.ScrapeInterval != 45*time.Minute || overrides.FullRecheckInterval != 2*time.Hour {
		t.Fatalf("schedule overrides = source %s full %s", overrides.ScrapeInterval, overrides.FullRecheckInterval)
	}
}

func unsetCredentialTestEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		old, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, old)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

func TestCredentialDefaultSupportsValueAndSecretFile(t *testing.T) {
	const valueEnv = "SOCKS5_POOL_TEST_SECRET"
	const fileEnv = "SOCKS5_POOL_TEST_SECRET_FILE"
	unsetCredentialTestEnv(t, valueEnv, fileEnv)

	if err := os.Setenv(valueEnv, "direct value"); err != nil {
		t.Fatal(err)
	}
	got, err := credentialDefault(valueEnv, fileEnv)
	if err != nil || got != "direct value" {
		t.Fatalf("direct credential = %q, %v", got, err)
	}
	if err := os.Unsetenv(valueEnv); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "admin-pass")
	if err := os.WriteFile(path, []byte("  mounted secret  \r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv(fileEnv, path); err != nil {
		t.Fatal(err)
	}
	got, err = credentialDefault(valueEnv, fileEnv)
	if err != nil || got != "  mounted secret  " {
		t.Fatalf("file credential = %q, %v", got, err)
	}
}

func TestCredentialDefaultRejectsAmbiguityAndOversizeFile(t *testing.T) {
	const valueEnv = "SOCKS5_POOL_TEST_CONFLICT"
	const fileEnv = "SOCKS5_POOL_TEST_CONFLICT_FILE"
	unsetCredentialTestEnv(t, valueEnv, fileEnv)

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxCredentialFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv(fileEnv, path); err != nil {
		t.Fatal(err)
	}
	if _, err := credentialDefault(valueEnv, fileEnv); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize secret error = %v", err)
	}
	if err := os.Setenv(valueEnv, "direct"); err != nil {
		t.Fatal(err)
	}
	if _, err := credentialDefault(valueEnv, fileEnv); err == nil || !strings.Contains(err.Error(), "cannot both be set") {
		t.Fatalf("ambiguous secret error = %v", err)
	}
}

func TestConfigValidateSurfacesCredentialLoadFailure(t *testing.T) {
	cfg := Config{credentialLoadErr: os.ErrPermission}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "load credentials") {
		t.Fatalf("Validate credential error = %v", err)
	}
}

func TestTrustedManagementProxyFlagAcceptsExactIPsAndIsTransactional(t *testing.T) {
	values := []net.IP{net.ParseIP("192.0.2.10")}
	flagValue := &trustedManagementProxyFlag{target: &values}
	if err := flagValue.Set("198.51.100.3, 2001:db8::1"); err != nil {
		t.Fatalf("Set valid values: %v", err)
	}
	if err := flagValue.Set("198.51.100.3"); err != nil {
		t.Fatalf("Set duplicate value: %v", err)
	}
	want := []string{"192.0.2.10", "198.51.100.3", "2001:db8::1"}
	if got := strings.Split(flagValue.String(), ","); len(got) != len(want) {
		t.Fatalf("trusted proxies = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("trusted proxies = %v, want %v", got, want)
			}
		}
	}

	before := flagValue.String()
	if err := flagValue.Set("203.0.113.9,proxy.example"); err == nil {
		t.Fatal("mixed valid/invalid flag value was accepted")
	}
	if got := flagValue.String(); got != before {
		t.Fatalf("failed Set partially mutated values: got %q, want %q", got, before)
	}
}

func TestTrustedManagementProxyFlagRejectsNonExactAddresses(t *testing.T) {
	for _, value := range []string{"", "192.0.2.0/24", "proxy.example", "fe80::1%eth0", "192.0.2.1,"} {
		t.Run(value, func(t *testing.T) {
			var values []net.IP
			if err := (&trustedManagementProxyFlag{target: &values}).Set(value); err == nil {
				t.Fatalf("Set(%q) unexpectedly succeeded with %v", value, values)
			}
			if len(values) != 0 {
				t.Fatalf("Set(%q) retained partial values %v", value, values)
			}
		})
	}
}

func TestConfigValidateConnectionLimitAndTrustedProxyIPs(t *testing.T) {
	base := Config{
		ListenAddr:          "127.0.0.1:1080",
		StatusAddr:          "127.0.0.1:8080",
		DataDir:             ".",
		ScrapeInterval:      time.Minute,
		FullRecheckInterval: time.Minute,
		CheckTimeout:        time.Second,
		MaxConcurrent:       1,
		MaxCandidates:       1,
	}

	valid := base
	valid.MaxClientConnections = 32
	valid.TrustedManagementProxies = []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("2001:db8::1")}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "negative connection limit", mutate: func(c *Config) { c.MaxClientConnections = -1 }},
		{name: "invalid trusted IP", mutate: func(c *Config) { c.TrustedManagementProxies = []net.IP{{1, 2, 3}} }},
		{name: "duplicate trusted IP", mutate: func(c *Config) {
			c.TrustedManagementProxies = []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("::ffff:192.0.2.1")}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}
