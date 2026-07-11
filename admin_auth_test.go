package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatusServerAdminAuthIsOptionalForLegacyConstructor(t *testing.T) {
	server := NewStatusServer(NewProxyPool(), &ConfigStore{})
	if server.adminAuthEnabled {
		t.Fatal("legacy NewStatusServer unexpectedly enabled admin authentication")
	}

	recorder := httptest.NewRecorder()
	server.handler().ServeHTTP(recorder, localTestRequest(http.MethodGet, "/api/status", nil))
	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("legacy /api/status status = %d, want %d", got, want)
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("legacy /api/status challenge = %q, want none", got)
	}
}

func TestStatusServerAdminAuthProtectsDashboardAndAllAPIRoutes(t *testing.T) {
	server := NewStatusServerWithAdminCredentials(NewProxyPool(), &ConfigStore{}, "admin", "correct horse")
	handler := server.handler()

	for _, path := range []string{"/", "/api/status", "/api/nodes/export", "/api/refresh"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, path, nil))
			if got, want := recorder.Code, http.StatusUnauthorized; got != want {
				t.Fatalf("unauthenticated %s status = %d, want %d", path, got, want)
			}
			if got, want := recorder.Header().Get("WWW-Authenticate"), `Basic realm="socks5-pool", charset="UTF-8"`; got != want {
				t.Fatalf("unauthenticated %s challenge = %q, want %q", path, got, want)
			}
		})
	}

	for _, credentials := range []struct {
		name     string
		user     string
		password string
	}{
		{name: "wrong user", user: "other", password: "correct horse"},
		{name: "wrong password", user: "admin", password: "wrong"},
		{name: "missing basic scheme", user: "", password: ""},
	} {
		t.Run(credentials.name, func(t *testing.T) {
			req := localTestRequest(http.MethodGet, "/api/status", nil)
			if credentials.name != "missing basic scheme" {
				req.SetBasicAuth(credentials.user, credentials.password)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if got, want := recorder.Code, http.StatusUnauthorized; got != want {
				t.Fatalf("invalid credential status = %d, want %d", got, want)
			}
		})
	}

	req := localTestRequest(http.MethodGet, "/api/status", nil)
	req.SetBasicAuth("admin", "correct horse")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("authenticated /api/status status = %d, want %d", got, want)
	}

	// Authentication is evaluated before route-level method checks: a caller
	// with valid credentials sees the endpoint's normal 405, not a challenge.
	req = localTestRequest(http.MethodGet, "/api/refresh", nil)
	req.SetBasicAuth("admin", "correct horse")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if got, want := recorder.Code, http.StatusMethodNotAllowed; got != want {
		t.Fatalf("authenticated GET /api/refresh status = %d, want %d", got, want)
	}
}

func TestStatusServerHealthzIsTheOnlyUnauthenticatedPath(t *testing.T) {
	handler := NewStatusServerWithAdminCredentials(NewProxyPool(), &ConfigStore{}, "admin", "correct horse").handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/healthz", nil))
	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("GET /healthz status = %d, want %d", got, want)
	}
	if got, want := recorder.Body.String(), "ok\n"; got != want {
		t.Fatalf("GET /healthz body = %q, want %q", got, want)
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("GET /healthz challenge = %q, want none", got)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodHead, "/healthz", nil))
	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("HEAD /healthz status = %d, want %d", got, want)
	}
	if got := recorder.Body.Len(); got != 0 {
		t.Fatalf("HEAD /healthz body length = %d, want 0", got)
	}

	// A similar path still goes through the protected mux; the exact liveness
	// endpoint is the only authentication bypass.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodGet, "/healthz/", nil))
	if got, want := recorder.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("GET /healthz/ status = %d, want %d", got, want)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, localTestRequest(http.MethodPost, "/healthz", nil))
	if got, want := recorder.Code, http.StatusMethodNotAllowed; got != want {
		t.Fatalf("POST /healthz status = %d, want %d", got, want)
	}
}

func TestConfigValidateRequiresCompleteAdminCredentialPair(t *testing.T) {
	base := Config{
		ListenAddr:     "127.0.0.1:1080",
		StatusAddr:     "127.0.0.1:8080",
		DataDir:        ".",
		ScrapeInterval: time.Minute,
		CheckTimeout:   time.Second,
		MaxConcurrent:  1,
		MaxCandidates:  1,
	}
	for _, tt := range []struct {
		name     string
		user     string
		password string
		wantErr  bool
	}{
		{name: "neither"},
		{name: "both", user: "admin", password: "secret"},
		{name: "username only", user: "admin", wantErr: true},
		{name: "password only", password: "secret", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.AdminUser = tt.user
			cfg.AdminPass = tt.password
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
