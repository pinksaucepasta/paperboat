package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestMachineAddJSONCarriesOneTokenForBothInstallers(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/client-configuration":
			writeAPIData(t, w, map[string]string{"version": "1", "machines_url": "https://console.example.test/machines", "installer_base_url": "https://install.example.test"})
		case "/v1/machine-enrollments":
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input) != 0 {
				t.Errorf("enrollment body = %v (%v), want empty: the token has no platform", input, err)
			}
			creates++
			writeAPIData(t, w, map[string]string{"id": "ume_one", "bootstrap_token": "ABCDEFGHIJKLMNOPQRSTUVWXYZ"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, path, server.URL)
	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", path, "machine", "add", "--json"}, &output, &output); code != 0 {
		t.Fatalf("exit=%d output=%q", code, output.String())
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	result := envelope.Data
	if creates != 1 ||
		result["linux_macos_command"] != "curl -fsSL 'https://install.example.test/install?p=ABCDEFGHIJKLMNOPQRSTUVWXYZ' | bash" ||
		result["windows_command"] != `powershell -c "iex (irm 'https://install.example.test/install.ps1?p=ABCDEFGHIJKLMNOPQRSTUVWXYZ')"` {
		t.Fatalf("creates=%d result=%v", creates, result)
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

func TestMachineAddWorkspaceFlagMakesTheTokenForThatWorkspace(t *testing.T) {
	var got map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/client-configuration":
			writeAPIData(t, w, map[string]string{"version": "1", "machines_url": "https://console.example.test/machines", "installer_base_url": "https://install.example.test"})
		case "/v1/machine-enrollments":
			got = map[string]string{}
			_ = json.NewDecoder(r.Body).Decode(&got)
			writeAPIData(t, w, map[string]string{"id": "ume_team", "bootstrap_token": "ABCDEFGHIJKLMNOPQRSTUVWXYZ"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, path, server.URL)
	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", path, "--workspace", "myteam", "machine", "add", "--json"}, &output, &output); code != 0 {
		t.Fatalf("exit=%d output=%q", code, output.String())
	}
	if got["workspace"] != "myteam" {
		t.Fatalf("enrollment body = %v, want workspace myteam", got)
	}
}
