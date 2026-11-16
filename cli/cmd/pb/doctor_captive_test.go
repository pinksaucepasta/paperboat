package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
)

// X-38 (Windows guest, 12 minutes after pairing): the access token had expired and could not refresh because a captive
// portal answered, so doctor printed "authentication is unavailable, check local credential storage" instead of naming the portal.
func TestDoctorServerFailureNamesACaptivePortalAndOnlyThen(t *testing.T) {
	base := doctorpkg.Check{Category: "control", Code: "authentication", Status: doctorpkg.StatusFail, Summary: "Paperboat authentication is unavailable.", Recovery: "Check local credential storage."}
	refreshFailure := &url.Error{Op: "Post", URL: "https://api.example.test/v1/auth/refresh", Err: errors.New("tls handshake timeout")}

	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://portal.example.test/login", http.StatusFound)
	}))
	defer portal.Close()
	check := doctorServerFailure(t.Context(), portal.URL, base, refreshFailure)
	if !strings.Contains(check.Summary, "captive portal") || !strings.Contains(check.Recovery, "network sign-in") || check.Validate() != nil {
		t.Fatalf("a portal in the server's place was not named: %q / %q", check.Summary, check.Recovery)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer server.Close()
	check = doctorServerFailure(t.Context(), server.URL, base, refreshFailure)
	if strings.Contains(check.Summary, "captive portal") || !strings.Contains(check.Recovery, "network access") {
		t.Fatalf("a plain outage was called a portal: %q / %q", check.Summary, check.Recovery)
	}
}
