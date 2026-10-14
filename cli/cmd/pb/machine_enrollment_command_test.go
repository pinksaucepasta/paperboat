package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestMachineAddCancelsUnusedPosixEnrollmentWhenWindowsCreationFails(t *testing.T) {
	var shells []string
	cancelled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/client-configuration":
			writeAPIData(t, w, map[string]string{"version": "1", "machines_url": "https://console.example.test/machines", "installer_base_url": "https://install.example.test"})
		case "/v1/machine-enrollments":
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			shells = append(shells, input["shell"])
			if input["shell"] == "powershell" {
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "unavailable", "message": "Enrollment unavailable"}})
				return
			}
			writeAPIData(t, w, map[string]string{"id": "ume_posix", "bootstrap_token": "test-private-bootstrap-token"})
		case "/v1/machine-enrollments/ume_posix/cancel":
			if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") == "" {
				t.Error("cleanup lost resource or operation binding")
			}
			cancelled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, path, server.URL)
	var output bytes.Buffer
	code := run(context.Background(), []string{"--config", path, "machine", "add", "--json"}, &output, &output)
	if code == 0 || !cancelled || strings.Join(shells, ",") != "posix,powershell" {
		t.Fatalf("partial enrollment did not retire: code=%d cancelled=%v shells=%v", code, cancelled, shells)
	}
	if strings.Contains(output.String(), "test-private-bootstrap-token") {
		t.Fatal("partial commands exposed an unused credential")
	}
}

func TestMachineAddUnavailableDiscoveryCreatesNoEnrollment(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/client-configuration" {
			writeAPIData(t, w, map[string]string{"version": "1", "machines_url": "https://console.example.test/machines"})
			return
		}
		if r.URL.Path == "/v1/machine-enrollments" {
			creates++
			writeAPIData(t, w, map[string]string{"id": "ume_unused", "bootstrap_token": "test-only-token"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, path, server.URL)
	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", path, "machine", "add"}, &output, &output); code == 0 || creates != 0 {
		t.Fatalf("unconfigured installer created enrollment: exit=%d creates=%d", code, creates)
	}
}
