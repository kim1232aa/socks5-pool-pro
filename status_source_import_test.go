package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sourceImportTestPart struct {
	name     string
	filename string
	value    []byte
}

func sourceImportTestRequest(t *testing.T, parts ...sourceImportTestPart) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, part := range parts {
		var (
			fieldWriter interface{ Write([]byte) (int, error) }
			err         error
		)
		if part.filename == "" {
			fieldWriter, err = writer.CreateFormField(part.name)
		} else {
			fieldWriter, err = writer.CreateFormFile(part.name, part.filename)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fieldWriter.Write(part.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := localTestRequest(http.MethodPost, "/api/sources/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestSourceImportCreatesUploadAndQueuesSafeRefresh(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewConfigStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newRefreshCoordinator()
	server := NewStatusServerWithCoordinator(NewProxyPool(), store, coordinator)
	const (
		originalFilename = "private-proxies-alice-secret.txt"
		proxyUsername    = "private-user"
		proxyPassword    = "private-password"
	)
	fileData := []byte(
		"socks5://" + proxyUsername + ":" + proxyPassword + "@8.8.8.8:1080\n" +
			"http://1.1.1.1:8080\n",
	)
	request := sourceImportTestRequest(t,
		sourceImportTestPart{name: "name", value: []byte("Local list")},
		sourceImportTestPart{name: "file", filename: originalFilename, value: fileData},
	)
	recorder := httptest.NewRecorder()
	server.handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("source import = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response sourceImportResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Source.ID == "" || response.Source.Name != "Local list" || response.Source.Kind != SourceKindUpload || response.Source.URL != "" || response.Source.Format != FormatTextRegex {
		t.Fatalf("imported source = %#v", response.Source)
	}
	if response.CandidateCount != 2 || !response.Accepted || response.Operation.SourceID != response.Source.ID || response.Operation.Status != "queued" || response.Operation.Trigger != "import" {
		t.Fatalf("import response = %#v", response)
	}
	for _, secret := range []string{originalFilename, dataDir, proxyUsername, proxyPassword, string(fileData)} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("source import response leaked %q: %s", secret, recorder.Body.String())
		}
	}
	stored, ok := store.SourceByID(response.Source.ID)
	if !ok || stored.Kind != SourceKindUpload || stored.URL != "" {
		t.Fatalf("stored source = %#v, found=%v", stored, ok)
	}
	select {
	case sourceID := <-coordinator.sourceRefreshChan:
		if sourceID != response.Source.ID {
			t.Fatalf("queued source = %q, want %q", sourceID, response.Source.ID)
		}
	default:
		t.Fatal("source import did not queue its refresh")
	}
}

func TestSourceImportRouteRequiresPost(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewStatusServer(NewProxyPool(), &ConfigStore{}).handler().ServeHTTP(
		recorder,
		localTestRequest(http.MethodGet, "/api/sources/import", nil),
	)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET source import = %d Allow=%q body=%s", recorder.Code, recorder.Header().Get("Allow"), recorder.Body.String())
	}
}

func TestSourceImportRejectsInvalidMultipartShape(t *testing.T) {
	tests := []struct {
		name    string
		request func(*testing.T) *http.Request
	}{
		{
			name: "wrong content type",
			request: func(*testing.T) *http.Request {
				request := localTestRequest(http.MethodPost, "/api/sources/import", strings.NewReader("not multipart"))
				request.Header.Set("Content-Type", "text/plain")
				return request
			},
		},
		{
			name: "missing name",
			request: func(t *testing.T) *http.Request {
				return sourceImportTestRequest(t, sourceImportTestPart{name: "file", filename: "list.txt", value: []byte("http://192.0.2.1:80")})
			},
		},
		{
			name: "missing file",
			request: func(t *testing.T) *http.Request {
				return sourceImportTestRequest(t, sourceImportTestPart{name: "name", value: []byte("List")})
			},
		},
		{
			name: "file is not an upload part",
			request: func(t *testing.T) *http.Request {
				return sourceImportTestRequest(t,
					sourceImportTestPart{name: "name", value: []byte("List")},
					sourceImportTestPart{name: "file", value: []byte("http://8.8.8.8:80")},
				)
			},
		},
		{
			name: "unknown field",
			request: func(t *testing.T) *http.Request {
				return sourceImportTestRequest(t,
					sourceImportTestPart{name: "name", value: []byte("List")},
					sourceImportTestPart{name: "metadata", value: []byte("private-filename.txt")},
					sourceImportTestPart{name: "file", filename: "list.txt", value: []byte("http://192.0.2.1:80")},
				)
			},
		},
		{
			name: "duplicate file",
			request: func(t *testing.T) *http.Request {
				return sourceImportTestRequest(t,
					sourceImportTestPart{name: "name", value: []byte("List")},
					sourceImportTestPart{name: "file", filename: "one.txt", value: []byte("http://192.0.2.1:80")},
					sourceImportTestPart{name: "file", filename: "two.txt", value: []byte("http://192.0.2.2:80")},
				)
			},
		},
		{
			name: "duplicate name",
			request: func(t *testing.T) *http.Request {
				return sourceImportTestRequest(t,
					sourceImportTestPart{name: "name", value: []byte("One")},
					sourceImportTestPart{name: "name", value: []byte("Two")},
					sourceImportTestPart{name: "file", filename: "list.txt", value: []byte("http://192.0.2.1:80")},
				)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewConfigStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			before := len(store.Sources())
			recorder := httptest.NewRecorder()
			NewStatusServerWithCoordinator(NewProxyPool(), store, newRefreshCoordinator()).handler().ServeHTTP(recorder, test.request(t))
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"invalid_source_import"`) {
				t.Fatalf("invalid import = %d: %s", recorder.Code, recorder.Body.String())
			}
			if len(store.Sources()) != before {
				t.Fatal("invalid multipart request created a source")
			}
			for _, leaked := range []string{"private-filename.txt", "one.txt", "two.txt", "list.txt"} {
				if strings.Contains(recorder.Body.String(), leaked) {
					t.Fatalf("invalid import response leaked filename %q: %s", leaked, recorder.Body.String())
				}
			}
		})
	}
}

func TestSourceImportEnforcesFileLimitWithoutLeakingFilename(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := len(store.Sources())
	request := sourceImportTestRequest(t,
		sourceImportTestPart{name: "name", value: []byte("Too large")},
		sourceImportTestPart{name: "file", filename: "secret-oversized-list.txt", value: bytes.Repeat([]byte("x"), maxFetchBytes+1)},
	)
	recorder := httptest.NewRecorder()
	NewStatusServerWithCoordinator(NewProxyPool(), store, newRefreshCoordinator()).handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), `"code":"source_import_too_large"`) {
		t.Fatalf("oversized import = %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "secret-oversized-list.txt") {
		t.Fatalf("oversized response leaked filename: %s", recorder.Body.String())
	}
	if len(store.Sources()) != before {
		t.Fatal("oversized import created a source")
	}
}

func TestSourceImportAcceptsFileAtExactFetchLimit(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("socks5://8.8.4.4:1080\n")
	fileData := make([]byte, maxFetchBytes)
	copy(fileData, prefix)
	for i := len(prefix); i < len(fileData); i++ {
		fileData[i] = ' '
	}
	request := sourceImportTestRequest(t,
		sourceImportTestPart{name: "name", value: []byte("Boundary")},
		sourceImportTestPart{name: "file", filename: "boundary.txt", value: fileData},
	)
	recorder := httptest.NewRecorder()
	NewStatusServerWithCoordinator(NewProxyPool(), store, newRefreshCoordinator()).handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("exact-limit import = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response sourceImportResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.CandidateCount != 1 {
		t.Fatalf("exact-limit candidate count = %d, want 1", response.CandidateCount)
	}
}

func TestSourceImportDoesNotLeakRejectedFileContents(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const secret = "private-user:private-password@not-a-proxy"
	request := sourceImportTestRequest(t,
		sourceImportTestPart{name: "name", value: []byte("Invalid local list")},
		sourceImportTestPart{name: "file", filename: "credentials.txt", value: []byte(secret)},
	)
	recorder := httptest.NewRecorder()
	NewStatusServerWithCoordinator(NewProxyPool(), store, newRefreshCoordinator()).handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid file import = %d: %s", recorder.Code, recorder.Body.String())
	}
	for _, leaked := range []string{secret, "private-user", "private-password", "credentials.txt"} {
		if strings.Contains(recorder.Body.String(), leaked) {
			t.Fatalf("invalid file response leaked %q: %s", leaked, recorder.Body.String())
		}
	}
}

func TestSourceImportStorageFailureIsSafeInternalError(t *testing.T) {
	badParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &ConfigStore{
		path: filepath.Join(badParent, "pool_config.json"),
		cfg:  defaultPoolConfig(),
	}
	request := sourceImportTestRequest(t,
		sourceImportTestPart{name: "name", value: []byte("Storage failure")},
		sourceImportTestPart{name: "file", filename: "private-list.txt", value: []byte("socks5://private-user:private-password@8.8.8.8:1080")},
	)
	recorder := httptest.NewRecorder()
	NewStatusServerWithCoordinator(NewProxyPool(), store, newRefreshCoordinator()).handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), `"code":"source_import_storage_failed"`) {
		t.Fatalf("storage failure import = %d: %s", recorder.Code, recorder.Body.String())
	}
	for _, leaked := range []string{badParent, "private-list.txt", "private-user", "private-password"} {
		if strings.Contains(recorder.Body.String(), leaked) {
			t.Fatalf("storage failure response leaked %q: %s", leaked, recorder.Body.String())
		}
	}
}

func TestSourceImportEnforcesOverallMultipartLimit(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	privateFilename := strings.Repeat("private-filename-", 5_000) + ".txt"
	request := sourceImportTestRequest(t,
		sourceImportTestPart{name: "name", value: []byte("Oversized multipart")},
		sourceImportTestPart{name: "file", filename: privateFilename, value: bytes.Repeat([]byte("x"), maxFetchBytes)},
	)
	recorder := httptest.NewRecorder()
	NewStatusServerWithCoordinator(NewProxyPool(), store, newRefreshCoordinator()).handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), `"code":"source_import_too_large"`) {
		t.Fatalf("oversized multipart = %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "private-filename") {
		t.Fatalf("oversized multipart response leaked filename: %s", recorder.Body.String())
	}
}
