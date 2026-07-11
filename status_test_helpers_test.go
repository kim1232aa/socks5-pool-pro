package main

import (
	"io"
	"net/http"
	"net/http/httptest"
)

// localTestRequest reflects the production default: unauthenticated
// management access is permitted only through a loopback Host.
func localTestRequest(method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Host = "localhost"
	return request
}
