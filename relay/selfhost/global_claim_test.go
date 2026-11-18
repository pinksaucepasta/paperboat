package selfhost

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestActualTLSGlobalClaimExportsManagedRuntimeAndReappliesOnRestart(t *testing.T) {
	dir, secret, client, server := testInstallation(t, true)
	claim := testClaim(true)
	claim.ScopeType, claim.ScopeID = "global", "global-native"
	status, _ := post(t, client, server.URL+"/v1/selfhost/claim", secret, claim)
	if status != 200 {
		t.Fatalf("global claim status=%d", status)
	}
	path := filepath.Join(dir, "tunnel", "deployment.json")
	read := func() map[string]any {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var deployment map[string]any
		if err := json.Unmarshal(body, &deployment); err != nil {
			t.Fatal(err)
		}
		return deployment
	}
	check := func() {
		t.Helper()
		deployment := read()
		if deployment["self_hosted"] != false {
			t.Fatal("authenticated global claim exported a customer-hosted runtime")
		}
		if deployment["browser_access_enabled"] != false {
			t.Fatal("enabled browser access without being asked")
		}
		if deployment["infrastructure_tls_cert_file"] != filepath.Join(dir, "tls.crt") || deployment["infrastructure_tls_key_file"] != filepath.Join(dir, "tls.key") {
			t.Fatal("global ownership changed claimed carrier identity")
		}
	}
	check()
	// A previously claimed installation already has files produced by the old
	// exporter. Restart must converge those files without another ownership claim.
	old := read()
	old["self_hosted"] = true
	if err := writeJSON(path, old); err != nil {
		t.Fatal(err)
	}
	state, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	state.Config.Listen = "127.0.0.1:0"
	if err := save(dir, state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := Serve(ctx, dir, cancel, nil); err != nil {
		t.Fatal(err)
	}
	check()
	updated, err := load(dir)
	if err != nil || updated.Receipt == nil || VerifyReceipt(*updated.Receipt) != nil || updated.Receipt.ScopeType != "global" || updated.PrivateKey != state.PrivateKey {
		t.Fatal("restart replaced or lost authenticated global ownership")
	}
}

func TestBrowserAccessFlagEnablesBrowserAccessInExportedDeployment(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		dir := t.TempDir()
		setup := Setup{ScopeType: "global", Capability: "tunnel", ControlURL: "https://api.example.test", BrowserAccess: enabled}
		registration := Registration{NodeID: "node_x", UsageKeyID: "key", EdgePool: "pool", RuntimeCredential: "c"}
		_, usage, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := exportRuntime(context.Background(), dir, setup, registration, usage, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(dir, "deployment.json"))
		if err != nil {
			t.Fatal(err)
		}
		var deployment map[string]any
		if err := json.Unmarshal(body, &deployment); err != nil {
			t.Fatal(err)
		}
		if deployment["browser_access_enabled"] != enabled {
			t.Fatalf("browser access %v exported %v", enabled, deployment["browser_access_enabled"])
		}
		if _, ok := deployment["browser_login_origin"]; ok {
			t.Fatal("exported a login origin: the server names the console")
		}
	}
}

// A 2026.11.16.2 install (testdata written by that release's Initialize) recorded browser access as a login
// origin: it loads with browser access on, and the edge deployment regenerated from it still carries the origin
// that release's edge image requires, so a failed upgrade that rolls the image back leaves it a config it reads.
func TestPreviousReleaseInstallationKeepsBrowserAccessAndItsOrigin(t *testing.T) {
	raw, err := os.ReadFile("testdata/selfhost-2026.11.16.2.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "selfhost.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(dir)
	if err != nil || !s.Config.BrowserAccess || s.Config.BrowserLoginOrigin != "https://console.example.test" {
		t.Fatalf("previous release's installation = %+v, %v", s.Config, err)
	}
	setup := Setup{ScopeType: "account", Capability: "tunnel", ControlURL: "https://api.example.test", BrowserAccess: s.Config.BrowserAccess, BrowserLoginOrigin: s.Config.BrowserLoginOrigin}
	_, usage, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := exportRuntime(context.Background(), out, setup, Registration{NodeID: "node_x", UsageKeyID: "key", EdgePool: "pool", RuntimeCredential: "c"}, usage, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(out, "deployment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var deployment map[string]any
	if err := json.Unmarshal(body, &deployment); err != nil || deployment["browser_access_enabled"] != true || deployment["browser_login_origin"] != "https://console.example.test" {
		t.Fatalf("deployment = %v, %v", deployment, err)
	}
}
