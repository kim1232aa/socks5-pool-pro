package main

import (
	_ "embed"
	"net/http"
)

// Keeping the template, presentation, and behavior in separate embedded
// resources makes the management console reviewable without adding a runtime
// filesystem dependency to the single-binary/container deployment.

//go:embed web/dashboard.html
var dashboardHTML string

//go:embed web/dashboard.css
var dashboardCSS []byte

//go:embed web/dashboard.js
var dashboardJS []byte

func embeddedDashboardAsset(contentType string, content []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(content)
		}
	}
}
