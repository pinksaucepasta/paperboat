package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
)

// C066 on pbmac 2026-11-05: pb ssh reads the machine's host keys from the daemon's machine list (F5-07), so
// right after `pb ssh trust-host` it still offered the old keys and failed. trust-host refreshes that list
// after the promotion succeeds, and only then.
func TestSSHTrustHostRefreshesTheMachineListAfterPromotion(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	set := func(state, id, fingerprint string) string {
		return `{"data":{"type":"host_key_set","version":1,"set_id":"` + id + `","machine_id":"machine_1","machine_generation":1,"observation_generation":2,"keys":["ssh-ed25519 AAAA"],"fingerprint":"` + fingerprint + `","state":"` + state + `","reconciliation_version":3}}`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/promote"):
			calls = append(calls, "promote")
			_, _ = w.Write([]byte(set("active", "keyset_3f2b6c1e-8a4d-4c7e-9b1a-2d5e6f708192", "SHA256:new")))
		case r.URL.Query().Get("state") == "pending":
			_, _ = w.Write([]byte(set("pending", "keyset_3f2b6c1e-8a4d-4c7e-9b1a-2d5e6f708192", "SHA256:new")))
		default:
			_, _ = w.Write([]byte(set("active", "keyset_9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d", "SHA256:old")))
		}
	}))
	defer server.Close()
	old := refreshAfterSSHTrustHost
	t.Cleanup(func() { refreshAfterSSHTrustHost = old })
	refreshAfterSSHTrustHost = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, "refresh")
		return nil
	}
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	for name, test := range map[string]struct {
		fingerprint string
		want        string
	}{
		"trusted":           {"SHA256:new", "promote,refresh"},
		"wrong fingerprint": {"SHA256:other", ""},
	} {
		calls = nil
		command := &cobra.Command{}
		command.Flags().String("fingerprint", test.fingerprint, "")
		command.Flags().Bool("json", true, "")
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetContext(t.Context())
		_ = trustSSHHost(command, client, api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1})
		if got := strings.Join(calls, ","); got != test.want {
			t.Fatalf("%s: calls %q, want %q", name, got, test.want)
		}
	}
}
