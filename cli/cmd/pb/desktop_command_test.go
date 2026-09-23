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
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestDesktopLocalPreferenceBridgeCASAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	run := func(request string) cliJSONEnvelope {
		t.Helper()
		c := newRootCommand()
		var out bytes.Buffer
		c.SetContext(context.Background())
		c.SetIn(strings.NewReader(request))
		c.SetOut(&out)
		c.SetErr(&out)
		c.SetArgs([]string{"--config", path, "desktop", "request"})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		var result cliJSONEnvelope
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err, out.String())
		}
		return result
	}
	got := run(`{"action":"network.get","payload":{"scope":"local"}}`)
	if !got.OK {
		t.Fatalf("get: %+v", got.Error)
	}
	got = run(`{"action":"network.set","payload":{"scope":"local","expected_revision":"absent","device_suffix":"mydevices","device_loopback_cidr":"127.42.0.0/16"}}`)
	if !got.OK {
		t.Fatalf("save: %+v", got.Error)
	}
	data, _ := json.Marshal(got.Data)
	var pref config.NetworkPreferences
	if err := json.Unmarshal(data, &pref); err != nil {
		t.Fatal(err)
	}
	got = run(`{"action":"network.set","payload":{"scope":"local","expected_revision":"absent","device_suffix":null,"device_loopback_cidr":null}}`)
	if got.OK {
		t.Fatal("stale mutation succeeded")
	}
	request, _ := json.Marshal(map[string]any{"action": "network.set", "payload": map[string]any{"scope": "local", "expected_revision": pref.Revision, "device_suffix": nil, "device_loopback_cidr": nil}})
	if !run(string(request)).OK {
		t.Fatal("reset failed")
	}
}

func TestDesktopAuthUsesDashboardEnrollmentAndRejectsRevokedCredential(t *testing.T) {
	dashboard := "https://dashboard.paperboat.test/dashboard/machines"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/client-configuration":
			_, _ = w.Write([]byte(`{"data":{"version":"1","machines_url":"` + dashboard + `"}}`))
		case "/v1/me":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"Authentication is required."}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerURL = server.URL
	cfg.Auth.ProfileDir = filepath.Join(t.TempDir(), "profiles")
	cfg.Auth.AllowFileFallback = true
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	command := newRootCommand()
	command.SetContext(context.Background())
	if err := command.PersistentFlags().Set("config", path); err != nil {
		t.Fatal(err)
	}

	assertPending := func(value any) {
		t.Helper()
		data, _ := json.Marshal(value)
		var state map[string]any
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		if state["status"] != "pending" || state["verification_uri"] != dashboard || state["message"] != dashboardEnrollmentGuidance {
			t.Fatalf("state = %#v", state)
		}
	}
	value, err := handleDesktop(command, desktopRequest{Action: "auth.login"})
	if err != nil {
		t.Fatal(err)
	}
	assertPending(value)

	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if err := store.Save(config.Profile{Issuer: server.URL, CLIClientSessionID: "session_revoked", AccessExpiresAt: expires}, config.Credential{AccessToken: "revoked", RefreshToken: "refresh", TokenType: "Bearer", ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	value, err = handleDesktop(command, desktopRequest{Action: "auth.poll"})
	if err != nil {
		t.Fatal(err)
	}
	assertPending(value)
}

func TestDesktopStrictRequestRejectsCommandInjectionFields(t *testing.T) {
	for _, input := range []string{`{"action":"overview","command":"rm"}`, `{"action":"overview"} {}`, `{"action":4}`} {
		var value desktopRequest
		if decodeDesktop([]byte(input), &value) == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestDesktopEffectiveNetworkPreservesIndependentFieldSources(t *testing.T) {
	remote := api.EffectiveNetworkPreferences{Effective: api.NetworkPreferenceValues{DeviceSuffix: "teamnet", DeviceLoopbackCIDR: "127.55.0.0/16"}, Sources: api.NetworkPreferenceValues{DeviceSuffix: "team", DeviceLoopbackCIDR: "account"}}
	suffix := "localnet"
	values, sources := resolveDesktopNetwork(config.NetworkPreferences{DeviceSuffix: &suffix}, remote)
	if values.DeviceSuffix != "localnet" || sources.DeviceSuffix != "local" || values.DeviceLoopbackCIDR != "127.55.0.0/16" || sources.DeviceLoopbackCIDR != "account" {
		t.Fatal(values, sources)
	}
	values, sources = resolveDesktopNetwork(config.NetworkPreferences{}, remote)
	if values != remote.Effective || sources != remote.Sources {
		t.Fatal("reset failed to restore inheritance")
	}
}
