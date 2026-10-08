package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func TestEdgeListUsesAccountInventoryAndPaginates(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") == "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/selfhost/installations":
			_, _ = w.Write([]byte(selfhostTestInventoryJSON(`[]`)))
			return
		case "/v1/selfhost/pools/tunnel":
			_, _ = w.Write([]byte(`{"data":{"mode":"mixed","installation_ids":[]}}`))
			return
		case "/v1/edges":
			requests++
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("after") == "" {
			_, _ = w.Write([]byte(`{"data":{"items":[{"id":"edge_a","region":"fsn1","status":"ready"}],"next_cursor":"edge_a","observed_at":"2026-09-23T00:00:00Z"}}`))
		} else if r.URL.Query().Get("after") == "edge_a" {
			_, _ = w.Write([]byte(`{"data":{"items":[{"id":"edge_b","region":"hel1","status":"draining"}],"observed_at":"2026-09-23T00:00:01Z"}}`))
		} else {
			http.Error(w, "bad cursor", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	writeTestProfile(t, root, configPath, server.URL)
	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "edge", "list", "--json"}, &output, &output); code != 0 {
		t.Fatalf("code=%d output=%s", code, output.String())
	}
	var document struct {
		Schema string `json:"schema"`
		Edges  []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil || document.Schema != "paperboat.edge-list/v1" || len(document.Edges) != 2 || document.Edges[0].ID != "edge_a" || document.Edges[1].Status != "draining" || requests != 2 {
		t.Fatalf("document=%+v requests=%d error=%v", document, requests, err)
	}
	output.Reset()
	if code := run(context.Background(), []string{"--config", configPath, "edge", "list"}, &output, &output); code != 0 || !strings.Contains(output.String(), "edge_a") || !strings.Contains(output.String(), "hel1") {
		t.Fatalf("human code=%d output=%s", code, output.String())
	}
}

func TestEdgeListRespectsPoolAndShowsHostedWhenNoSelfhostSelected(t *testing.T) {
	for _, tc := range []struct {
		name, mode, installations, selected    string
		wantHosted, wantSelfhost, wantFallback bool
	}{
		{"mixed", "mixed", `[{"installation_id":"install_1","node_id":"node_1","name":"My edge","capability":"tunnel","scope_kind":"account","scope_id":"account_1","enrollment_state":"enrolled","ready":true}]`, `["install_1"]`, true, true, false},
		{"selfhost_only", "self-hosted-only", `[{"installation_id":"install_1","node_id":"node_1","name":"My edge","capability":"tunnel","scope_kind":"account","scope_id":"account_1","enrollment_state":"enrolled","ready":false}]`, `["install_1"]`, false, true, false},
		{"unselected_private", "mixed", `[{"installation_id":"install_1","node_id":"node_1","name":"Shared edge","capability":"tunnel","scope_kind":"team","scope_id":"team_1","enrollment_state":"enrolled","ready":true}]`, `[]`, true, false, false},
		{"pending_selected", "self-hosted-only", `[{"installation_id":"install_1","node_id":"node_1","name":"Pending edge","capability":"tunnel","scope_kind":"account","scope_id":"account_1","enrollment_state":"pending","ready":false}]`, `["install_1"]`, true, false, true},
		{"empty_fallback", "self-hosted-only", `[]`, `[]`, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostedRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/selfhost/installations":
					_, _ = w.Write([]byte(selfhostTestInventoryJSON(tc.installations)))
				case "/v1/selfhost/pools/tunnel":
					_, _ = w.Write([]byte(`{"data":{"mode":"` + tc.mode + `","installation_ids":` + tc.selected + `}}`))
				case "/v1/edges":
					hostedRequests++
					_, _ = w.Write([]byte(`{"data":{"items":[{"id":"hosted_1","region":"fsn1","status":"ready"}],"observed_at":"2026-09-23T00:00:00Z"}}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			root := t.TempDir()
			configPath := filepath.Join(root, "config.json")
			writeTestProfile(t, root, configPath, server.URL)
			var output bytes.Buffer
			if code := run(context.Background(), []string{"--config", configPath, "edge", "list", "--json"}, &output, &output); code != 0 {
				t.Fatalf("code=%d output=%s", code, output.String())
			}
			var result struct {
				Edges []struct {
					Source string `json:"source"`
				} `json:"edges"`
				HostedFallbackDisplayOnly bool `json:"hosted_fallback_display_only"`
			}
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			hosted, selfhost := false, false
			for _, edge := range result.Edges {
				hosted = hosted || edge.Source == "paperboat"
				selfhost = selfhost || edge.Source == "self-hosted"
			}
			if hosted != tc.wantHosted || selfhost != tc.wantSelfhost || result.HostedFallbackDisplayOnly != tc.wantFallback || (hostedRequests > 0) != tc.wantHosted {
				t.Fatalf("hosted=%v selfhost=%v fallback=%v hostedRequests=%d", hosted, selfhost, result.HostedFallbackDisplayOnly, hostedRequests)
			}
		})
	}
}

func TestRelayListResultsUseNativeAuthorityAndSelectedPool(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	inventory := localapi.RelayInventory{Schema: localapi.RelayInventorySchemaV1, Candidates: []localapi.RelayCandidate{
		{NodeID: "hosted_1", Region: "fsn1", State: "ready", ObservedAt: now.Unix() - 2, ExpiresAt: now.Unix() + 30, Roles: []string{"relay"}, Transports: []string{"derp_quic"}},
		{NodeID: "node_1", Region: "hel1", State: "ready", ObservedAt: now.Unix() - 3, ExpiresAt: now.Unix() + 30, Roles: []string{"relay"}, Transports: []string{"derp_quic"}},
		{NodeID: "edge_1", Region: "fsn1", State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Unix() + 30, Roles: []string{"edge"}, Transports: []string{"http3"}},
	}}
	selected := []api.SelfhostInstallation{{InstallationID: "install_1", NodeID: "node_1", Name: "My relay", ScopeKind: "account", ScopeID: "account_1", EnrollmentState: "enrolled", Selected: true}, {InstallationID: "install_2", NodeID: "node_2", Name: "Offline relay", ScopeKind: "team", ScopeID: "team_1", EnrollmentState: "enrolled", Selected: true}}
	mixed := relayListResults(inventory, selected, "mixed", now)
	if len(mixed) != 3 || mixed[0].RelayID != "hosted_1" || mixed[0].Status != "reported_ready" || mixed[1].Name != "My relay" || mixed[1].Source != "self-hosted" || mixed[2].RelayID != "node_2" || mixed[2].Status != "unavailable" {
		t.Fatalf("mixed relays = %+v", mixed)
	}
	strict := relayListResults(inventory, selected, "self-hosted-only", now)
	if len(strict) != 2 || strict[0].RelayID != "node_1" || strict[1].RelayID != "node_2" {
		t.Fatalf("strict relays = %+v", strict)
	}
	stale := relayListResults(inventory, selected, "mixed", now.Add(20*time.Second))
	if stale[0].Status != "unavailable" || stale[1].Status != "unavailable" {
		t.Fatalf("stale relays = %+v", stale)
	}
}

func TestSendManagementHasNoTransferAlias(t *testing.T) {
	root := newRootCommand()
	for _, command := range root.Commands() {
		if command.Name() == "transfer" {
			t.Fatal("old transfer command remains registered")
		}
	}
	for _, path := range [][]string{{"send", "destination"}, {"send", "destination", "set"}, {"send", "destination", "clear"}, {"send", "list"}, {"send", "status"}, {"send", "cancel"}} {
		command, _, err := root.Find(path)
		if err != nil || command == nil || command.Name() != path[len(path)-1] {
			t.Fatalf("missing %v: %v", path, err)
		}
	}
}

func TestRelayListKeepsUnselectedSignedIdentityWithoutInventingOfflineEntries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	candidates := localapi.RelayInventory{Candidates: []localapi.RelayCandidate{
		{NodeID: "shared", Region: "hel1", State: "ready", ObservedAt: now.Unix() - 1, ExpiresAt: now.Unix() + 30, Roles: []string{"relay"}, Transports: []string{"derp_quic"}},
		{NodeID: "global", Region: "fsn1", State: "ready", ObservedAt: now.Unix() - 1, ExpiresAt: now.Unix() + 30, Roles: []string{"relay"}, Transports: []string{"derp_quic"}},
	}}
	metadata := []api.SelfhostInstallation{
		{NodeID: "shared", Name: "Shared relay", ScopeKind: "team", ScopeID: "team_1", EnrollmentState: "enrolled"},
		{NodeID: "global", Name: "Paperboat relay", ScopeKind: "global", ScopeID: "global", EnrollmentState: "enrolled"},
		{NodeID: "offline_selected", Name: "Selected offline", ScopeKind: "account", ScopeID: "account_1", EnrollmentState: "enrolled", Selected: true},
		{NodeID: "offline_unselected", Name: "Unselected offline", ScopeKind: "account", ScopeID: "account_1", EnrollmentState: "enrolled"},
	}
	mixed := relayListResults(candidates, metadata, "mixed", now)
	if len(mixed) != 3 || mixed[0].Name != "Shared relay" || mixed[0].Source != "self-hosted" || mixed[0].Status != "reported_ready" || mixed[1].Name != "Paperboat relay" || mixed[1].Source != "paperboat" || mixed[2].RelayID != "offline_selected" || mixed[2].Status != "unavailable" {
		t.Fatalf("mixed relays=%+v", mixed)
	}
	strict := relayListResults(candidates, metadata, "self-hosted-only", now)
	if len(strict) != 1 || strict[0].RelayID != "offline_selected" {
		t.Fatalf("strict relays=%+v", strict)
	}
}

func TestSelfhostInventoryRetainsAuthorizedEnrolledMetadataAndComputesSelection(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint("invalid_scope_", invalid), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/selfhost/installations" {
					scope := "team"
					if invalid {
						scope = ""
					}
					fmt.Fprint(w, selfhostTestInventoryJSON(fmt.Sprintf(`[{"installation_id":"shared","node_id":"shared_node","name":"Shared relay","capability":"relay","scope_kind":%q,"scope_id":"team_1","enrollment_state":"enrolled","ready":true},{"installation_id":"selected","node_id":"selected_node","name":"Offline selected","capability":"relay","scope_kind":"account","scope_id":"account_1","enrollment_state":"enrolled","ready":false},{"installation_id":"pending","node_id":"pending_node","name":"Pending relay","capability":"relay","scope_kind":"account","scope_id":"account_1","enrollment_state":"pending","ready":false}]`, scope)))
				} else if r.URL.Path == "/v1/selfhost/pools/relay" {
					fmt.Fprint(w, `{"data":{"mode":"mixed","installation_ids":["selected"]}}`)
				} else {
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			items, pool, err := api.New(server.URL, config.Credential{AccessToken: "test-credential"}, server.Client()).SelfhostInventory(context.Background(), "relay")
			if invalid {
				if err == nil {
					t.Fatal("missing authoritative scope accepted")
				}
				return
			}
			if err != nil || pool.Mode != "mixed" || len(items) != 2 || items[0].Name != "Shared relay" || items[0].Selected || !items[1].Selected {
				t.Fatalf("inventory=%+v pool=%+v err=%v", items, pool, err)
			}
		})
	}
}

func selfhostTestInventoryJSON(itemsJSON string) string {
	var items []api.SelfhostInstallation
	if err := json.Unmarshal([]byte(itemsJSON), &items); err != nil {
		panic(err)
	}
	counts := api.SelfhostCounts{}
	for _, item := range items {
		if item.EnrollmentState == "enrolled" {
			counts.Enrolled++
		} else {
			counts.Pending++
		}
		if item.Ready {
			counts.Ready++
		}
	}
	data, err := json.Marshal(map[string]any{"data": api.SelfhostInstallationPage{Installations: items, Pagination: api.Pagination{Limit: 200, Offset: 0, Total: len(items)}, Counts: counts}})
	if err != nil {
		panic(err)
	}
	return string(data)
}
