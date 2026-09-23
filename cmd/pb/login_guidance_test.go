package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/preferences"
)

func TestLoginUsesServerDashboardURLWithoutAuthorization(t *testing.T) {
	oldBrowser := openBrowser
	openBrowser = func(string) error { t.Fatal("login opened a browser"); return nil }
	defer func() { openBrowser = oldBrowser }()
	for _, path := range []string{"login", "auth switch"} {
		for _, override := range []bool{false, true} {
			for _, jsonMode := range []bool{false, true} {
				t.Run(path+map[bool]string{false: "/config", true: "/override"}[override]+map[bool]string{false: "/text", true: "/json"}[jsonMode], func(t *testing.T) {
					dashboard := "https://deployment.example.test/custom/dashboard/machines"
					requests := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests++
						if r.Method != http.MethodGet || r.URL.Path != "/v1/client-configuration" || r.Header.Get("Authorization") != "" {
							t.Errorf("unexpected authenticated or non-metadata request: %s %s", r.Method, r.URL.Path)
							http.Error(w, "unexpected request", 400)
							return
						}
						writeAPIData(t, w, map[string]string{"version": "1", "machines_url": dashboard})
					}))
					defer server.Close()
					dir := t.TempDir()
					isolateCommandCredentialLocation(t, dir)
					cfg := filepath.Join(dir, "config.json")
					data := []byte(`{"server_url":` + quote(server.URL) + `}`)
					args := []string{"--config", cfg}
					if override {
						// An explicit server must not depend on valid local configuration.
						data = []byte("invalid config")
						args = append(args, "--server", server.URL)
					}
					if err := os.WriteFile(cfg, data, 0600); err != nil {
						t.Fatal(err)
					}
					prefPath, err := preferences.Path(cfg)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Dir(prefPath), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(prefPath, []byte("invalid preferences"), 0600); err != nil {
						t.Fatal(err)
					}
					args = append(args, strings.Fields(path)...)
					if jsonMode {
						args = append(args, "--json")
					}
					var out, errOut bytes.Buffer
					if code := run(context.Background(), args, &out, &errOut); code != 0 {
						t.Fatalf("exit=%d out=%s stderr=%s", code, &out, &errOut)
					}
					want := dashboardEnrollmentGuidance + "\n" + dashboard
					if jsonMode {
						var result struct {
							OK   bool `json:"ok"`
							Data struct {
								Message string `json:"message"`
							} `json:"data"`
						}
						if err := json.Unmarshal(out.Bytes(), &result); err != nil {
							t.Fatal(err)
						}
						if !result.OK || result.Data.Message != want {
							t.Fatalf("output=%s", &out)
						}
					} else if out.String() != want+"\n" {
						t.Fatalf("output=%q", out.String())
					}
					if requests != 1 || errOut.Len() != 0 {
						t.Fatalf("requests=%d stderr=%s", requests, &errOut)
					}
					after, err := os.ReadFile(cfg)
					if err != nil || !bytes.Equal(after, data) {
						t.Fatal("login changed configuration")
					}
					command, _, err := newRootCommand().Find(strings.Fields(path))
					if err != nil {
						t.Fatal(err)
					}
					if command.Flags().Lookup("recovery-key") != nil {
						t.Fatal("login exposes recovery workflow")
					}
				})
			}
		}
	}
}

func TestLoginDashboardLookupFailureKeepsGuidance(t *testing.T) {
	for _, scenario := range []string{"unavailable", "invalid-url", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/client-configuration" {
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				if scenario == "unavailable" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				writeAPIData(t, w, map[string]string{"version": "1", "machines_url": "not-a-url"})
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "canceled" {
				cancel()
			}
			var out, errOut bytes.Buffer
			command := newRootCommand()
			command.SetOut(&out)
			command.SetErr(&errOut)
			command.SetArgs([]string{"--server", server.URL, "login"})
			err := command.ExecuteContext(ctx)
			if err == nil || !strings.Contains(err.Error(), dashboardEnrollmentGuidance) || !strings.Contains(err.Error(), "Cannot retrieve the dashboard URL") {
				t.Fatalf("error=%v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("lookup failure printed successful guidance URL: %s", &out)
			}
		})
	}
}
