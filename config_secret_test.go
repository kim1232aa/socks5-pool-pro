package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
