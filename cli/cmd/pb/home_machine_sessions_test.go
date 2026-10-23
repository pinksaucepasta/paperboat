package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

// The session menu lists the caller's sessions with one catalog request however many machines
// the account has; a request per machine made `pb session` hang on accounts with ~130 machines.
func TestMachineSessionMenuListsSessionsInOneCatalogRequest(t *testing.T) {
	catalog := 0
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/terminal-sessions" {
			t.Errorf("per-machine or unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		catalog++
		if query := r.URL.Query(); query.Get("owner") != "mine" || query.Get("live") != "false" {
			t.Errorf("catalog query %q, want owner=mine and live=false", r.URL.RawQuery)
		}
		row := func(id, name, machine string, active time.Time) map[string]any {
			return map[string]any{"id": id, "name": name, "state": "running", "is_default": name == "default", "last_active_at": active, "created_at": now.Add(-time.Hour), "updated_at": active,
				"target": map[string]any{"kind": "machine", "id": machine, "machine_id": machine}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"sessions": []any{
			row("pts_old", "build", "mch_a", now.Add(-time.Minute)),
			row("pts_new", "default", "mch_b", now),
			row("pts_x", "other", "mch_unlisted", now),
			row("pts_noterm", "x", "mch_noterm", now),
		}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 4, "next_offset": nil}}})
	}))
	defer server.Close()
	host := func(id, alias string, terminal bool) api.UserMachine {
		machine := api.UserMachine{ID: id, Alias: alias}
		machine.Capabilities.TerminalHost.Configured = terminal
		return machine
	}
	machines := []api.UserMachine{host("mch_a", "alpha", true), host("mch_b", "beta", true), host("mch_noterm", "gamma", false)}
	for i := 0; i < 130; i++ {
		machines = append(machines, host("mch_idle_"+string(rune('a'+i%26))+string(rune('a'+i/26)), "idle", true))
	}
	sessions, err := listMachineSessionsForMachines(context.Background(), api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client()), machines)
	if err != nil {
		t.Fatal(err)
	}
	if catalog != 1 || len(sessions) != 2 {
		t.Fatalf("catalog requests=%d sessions=%d, want 1 request and 2 sessions", catalog, len(sessions))
	}
	if first := sessions[0]; first.session.ID != "pts_new" || first.target.name != "beta" || !first.session.IsDefault || first.session.CreatedAt.IsZero() {
		t.Fatalf("most recent session first with its menu fields, got %+v", first)
	}
	if second := sessions[1]; second.session.ID != "pts_old" || second.target.id != "mch_a" {
		t.Fatalf("second session %+v", second)
	}
}
