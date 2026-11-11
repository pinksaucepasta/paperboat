package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
)

// Edge abuse plan 3.5: a list given by flags replaces only that list, the other is kept; a bad entry is
// refused before any request; --clear sends two empty lists.
func TestTunnelIPRulesCommandReplacesOnlyTheGivenList(t *testing.T) {
	var puts []api.IPRules
	stored := api.IPRules{Allow: []string{"198.51.100.0/24"}, Deny: []string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tunnels/tun_1/ip-rules" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Method == http.MethodPut {
			var body api.IPRules
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			puts = append(puts, body)
			stored = body
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": stored})
	}))
	defer server.Close()
	oldClient, oldResolve := tunnelClientForCommand, resolveTunnelSelectorForCommand
	t.Cleanup(func() { tunnelClientForCommand, resolveTunnelSelectorForCommand = oldClient, oldResolve })
	tunnelClientForCommand = func(*cobra.Command) (*api.Client, error) {
		return api.New(server.URL, config.Credential{AccessToken: "client-session"}, server.Client()), nil
	}
	resolveTunnelSelectorForCommand = func(context.Context, *api.Client, string) (string, error) { return "tun_1", nil }
	run := func(args ...string) (string, error) {
		command := ipRulesCommand("tunnel")
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetArgs(args)
		command.SetContext(context.Background())
		err := command.Execute()
		return out.String(), err
	}
	if out, err := run("tun_1", "--deny", "203.0.113.0/24"); err != nil || !strings.Contains(out, "Allow: 198.51.100.0/24") || !strings.Contains(out, "Deny:  203.0.113.0/24") {
		t.Fatalf("deny only: %q %v", out, err)
	}
	if len(puts) != 1 || strings.Join(puts[0].Allow, ",") != "198.51.100.0/24" || strings.Join(puts[0].Deny, ",") != "203.0.113.0/24" {
		t.Fatalf("put = %+v, want the allow list kept", puts)
	}
	if _, err := run("tun_1", "--allow", "not-an-address"); err == nil || len(puts) != 1 {
		t.Fatalf("bad entry: err=%v puts=%d", err, len(puts))
	}
	if out, err := run("tun_1", "--clear"); err != nil || !strings.Contains(out, "No IP rules") || len(puts[1].Allow) != 0 || len(puts[1].Deny) != 0 {
		t.Fatalf("clear: %q %v %+v", out, err, puts)
	}
	if _, err := run("tun_1"); err != nil || len(puts) != 2 {
		t.Fatalf("show sent a PUT: %v %d", err, len(puts))
	}
}
