package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCandidateBatchCheckDefaultsAndClampsLimit(t *testing.T) {
	coordinator := newRefreshCoordinator()
	server := NewStatusServerWithCoordinator(NewProxyPool(), &ConfigStore{}, coordinator)
	server.SetCheckDefaults(time.Second, 2, 7, false)
	recorder := httptest.NewRecorder()
	server.handler().ServeHTTP(recorder, localTestRequest(http.MethodPost, "/api/candidates/batch-check", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("default candidate batch = %d %s", recorder.Code, recorder.Body.String())
	}
	coordinator.candidateCheckMu.RLock()
	request := coordinator.candidateCheckRequest
	coordinator.candidateCheckMu.RUnlock()
	if request == nil || request.limit != 7 || request.kind != candidateCheckOperationCandidateBatch {
		t.Fatalf("queued default candidate request = %+v", request)
	}
}

func TestCandidateBatchCheckRejectsInvalidLimit(t *testing.T) {
	for _, body := range []string{`{"limit":0}`, `{"limit":-1}`, `{"limit":6}`, ``, `{"limit":1,"unexpected":true}`} {
		coordinator := newRefreshCoordinator()
		server := NewStatusServerWithCoordinator(NewProxyPool(), &ConfigStore{}, coordinator)
		server.SetCheckDefaults(time.Second, 2, 5, false)
		recorder := httptest.NewRecorder()
		server.handler().ServeHTTP(recorder, localTestRequest(http.MethodPost, "/api/candidates/batch-check", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"invalid_candidate_check_request"`) {
			t.Errorf("candidate batch body %q = %d %s", body, recorder.Code, recorder.Body.String())
		}
		if status := coordinator.candidateCheckOperationStatus(); status.Status != "idle" {
			t.Errorf("invalid body %q queued operation: %+v", body, status)
		}
	}
}

func TestCandidateBatchCheckReturnsAcceptedOperation(t *testing.T) {
	coordinator := newRefreshCoordinator()
	server := NewStatusServerWithCoordinator(NewProxyPool(), &ConfigStore{}, coordinator)
	server.SetCheckDefaults(time.Second, 2, 5, false)
	recorder := httptest.NewRecorder()
	server.handler().ServeHTTP(recorder, localTestRequest(http.MethodPost, "/api/candidates/batch-check", strings.NewReader(`{"limit":3}`)))
	if recorder.Code != http.StatusAccepted || recorder.Header().Get("Location") != "/api/candidates/batch-check/status" {
		t.Fatalf("candidate batch accepted = %d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	var response candidateCheckStartResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID == "" || response.Kind != candidateCheckOperationCandidateBatch || response.Status != "queued" || !response.Accepted || response.StatusURL != "/api/candidates/batch-check/status" {
		t.Fatalf("candidate batch response = %+v", response)
	}
	statusRecorder := httptest.NewRecorder()
	server.handler().ServeHTTP(statusRecorder, localTestRequest(http.MethodGet, response.StatusURL, nil))
	var status CandidateCheckOperation
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if statusRecorder.Code != http.StatusOK || status.ID != response.ID || status.Status != "queued" {
		t.Fatalf("candidate status = %d %+v", statusRecorder.Code, status)
	}
}

func TestCandidateCheckRejectsSecondManualTaskWithConflict(t *testing.T) {
	failed := Proxy{IP: "198.51.100.90", Port: "8080", Protocol: "http"}
	pool := candidateOperationTestPool(nil, []Proxy{failed})
	coordinator := newRefreshCoordinator()
	server := NewStatusServerWithCoordinator(pool, &ConfigStore{}, coordinator)
	server.SetCheckDefaults(time.Second, 2, 5, false)
	handler := server.handler()

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, localTestRequest(http.MethodPost, "/api/candidates/batch-check", strings.NewReader(`{}`)))
	var firstResponse candidateCheckStartResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatal(err)
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, localTestRequest(http.MethodPost, "/api/failed-candidates/retry", strings.NewReader(`{"keys":["`+failed.Key()+`"]}`)))
	var secondResponse candidateCheckStartResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondResponse); err != nil {
		t.Fatal(err)
	}
	if second.Code != http.StatusConflict || secondResponse.Code != "candidate_check_busy" || secondResponse.ID != firstResponse.ID || secondResponse.Accepted {
		t.Fatalf("second manual candidate task = %d %+v", second.Code, secondResponse)
	}
}

func TestFailedRetryRejectsUnknownKeyBeforeStarting(t *testing.T) {
	failed := Proxy{IP: "198.51.100.91", Port: "1080", Protocol: "socks5"}
	pool := candidateOperationTestPool(nil, []Proxy{failed})
	coordinator := newRefreshCoordinator()
	server := NewStatusServerWithCoordinator(pool, &ConfigStore{}, coordinator)
	server.SetCheckDefaults(time.Second, 2, 5, false)

	for _, body := range []string{
		`{"keys":["http://198.51.100.99:8080"]}`,
		`{"keys":["SOCKS5://198.51.100.91:1080"]}`,
		`{"keys":["` + failed.Key() + `","` + failed.Key() + `"]}`,
		`{"keys":[]}`,
	} {
		recorder := httptest.NewRecorder()
		server.handler().ServeHTTP(recorder, localTestRequest(http.MethodPost, "/api/failed-candidates/retry", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("failed retry body %s = %d %s", body, recorder.Code, recorder.Body.String())
		}
		if status := coordinator.candidateCheckOperationStatus(); status.Status != "idle" {
			t.Fatalf("failed retry validation queued operation: %+v", status)
		}
	}

	unknown := httptest.NewRecorder()
	server.handler().ServeHTTP(unknown, localTestRequest(http.MethodPost, "/api/failed-candidates/retry", strings.NewReader(`{"keys":["http://198.51.100.99:8080"]}`)))
	if !strings.Contains(unknown.Body.String(), `"code":"failed_candidate_not_found"`) {
		t.Fatalf("unknown failed key error = %s", unknown.Body.String())
	}
}

func TestFailedRetryReturnsAcceptedOperation(t *testing.T) {
	failed := Proxy{IP: "198.51.100.92", Port: "1080", Protocol: "socks5"}
	pool := candidateOperationTestPool(nil, []Proxy{failed})
	coordinator := newRefreshCoordinator()
	server := NewStatusServerWithCoordinator(pool, &ConfigStore{}, coordinator)
	server.SetCheckDefaults(time.Second, 2, 5, false)
	recorder := httptest.NewRecorder()
	server.handler().ServeHTTP(recorder, localTestRequest(http.MethodPost, "/api/failed-candidates/retry", strings.NewReader(`{"keys":["`+failed.Key()+`"]}`)))
	var response candidateCheckStartResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusAccepted || recorder.Header().Get("Location") != "/api/failed-candidates/retry/status" || response.Kind != candidateCheckOperationFailedRetry || !response.Accepted || response.StatusURL != "/api/failed-candidates/retry/status" {
		t.Fatalf("failed retry accepted = %d headers=%v response=%+v", recorder.Code, recorder.Header(), response)
	}
}
