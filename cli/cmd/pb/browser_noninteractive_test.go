package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// The rule every command that opens a browser shares: a person at a terminal gets a browser; --json and a run
// without a terminal (a script, an agent, CI) get only the printed link.
func TestOpenBrowserInteractiveOpensOnlyForAPersonAtATerminal(t *testing.T) {
	for _, c := range []struct {
		name           string
		json, terminal bool
		wantOpened     bool
	}{{"terminal", false, true, true}, {"json at a terminal", true, true, false}, {"no terminal", false, false, false}, {"json without terminal", true, false, false}} {
		t.Run(c.name, func(t *testing.T) {
			opened := 0
			ctx := withHostDependencies(t.Context(), func(d *hostDependencies) {
				d.isTerminal = func() bool { return c.terminal }
				d.openBrowser = func(string) error { opened++; return nil }
			})
			if err := openBrowserInteractive(ctx, c.json, "https://example.test/x"); err != nil || (opened == 1) != c.wantOpened {
				t.Fatalf("opened=%d err=%v, want opened=%v", opened, err, c.wantOpened)
			}
		})
	}
}

func browserCommandFixture(t *testing.T, terminal bool, opened *int) (context.Context, []string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if teamFixtureUpdatePolicy(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/client-configuration":
			_, _ = w.Write([]byte(`{"data":{"version":"1","machines_url":"https://console.example.test/machines","cli_verification_url":"https://console.example.test/cli","console_url":"https://console.example.test/","purchase_consent":{"version":"2026-11-19","text":"No refunds."}}}`))
		case r.Method == "POST" && r.URL.Path == "/v1/teams":
			_, _ = w.Write([]byte(`{"data":{"url":"https://checkout.example.test/c/pay_9"}}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/portal"):
			_, _ = w.Write([]byte(`{"data":{"url":"https://portal.example.test/p/abc"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, configPath, server.URL)
	ctx := withHostDependencies(t.Context(), func(d *hostDependencies) {
		d.isTerminal = func() bool { return terminal }
		d.openBrowser = func(string) error { *opened++; return nil }
	})
	return ctx, []string{"--server", server.URL, "--config", configPath}
}

func TestTeamCreateOpensTheCheckoutOnlyForAPersonAtATerminal(t *testing.T) {
	for _, c := range []struct {
		name           string
		extra          []string
		terminal, open bool
	}{{"terminal", nil, true, true}, {"--json at a terminal", []string{"--json"}, true, false}, {"no terminal", nil, false, false}, {"--json without terminal", []string{"--json"}, false, false}} {
		t.Run(c.name, func(t *testing.T) {
			opened := 0
			ctx, common := browserCommandFixture(t, c.terminal, &opened)
			args := append([]string{"team", "create", "acme", "--no-wait", "--accept-terms"}, append(c.extra, common...)...)
			var out bytes.Buffer
			if code := run(ctx, args, &out, &out); code != 0 || !strings.Contains(out.String(), "pay_9") || (opened == 1) != c.open {
				t.Fatalf("exit=%d opened=%d want opened=%v output=%s", code, opened, c.open, out.String())
			}
		})
	}
}

func TestSSOSetupOpensThePortalOnlyForAPersonAtATerminal(t *testing.T) {
	for _, c := range []struct {
		name     string
		terminal bool
	}{{"terminal", true}, {"no terminal", false}} {
		t.Run(c.name, func(t *testing.T) {
			opened := 0
			ctx, common := browserCommandFixture(t, c.terminal, &opened)
			var out bytes.Buffer
			if code := run(ctx, append([]string{"team", "sso", "setup", "acme"}, common...), &out, &out); code != 0 || !strings.Contains(out.String(), "portal.example.test/p/abc") || (opened == 1) != c.terminal {
				t.Fatalf("exit=%d opened=%d output=%s", code, opened, out.String())
			}
		})
	}
}
