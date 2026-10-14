package selfhost

import (
	"context"
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
		if deployment["browser_access_enabled"] != false || deployment["browser_login_origin"] != "" {
			t.Fatal("enabled browser access without configured discovery and PSL isolation")
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
