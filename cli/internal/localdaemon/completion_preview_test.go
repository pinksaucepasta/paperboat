package localdaemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestCompletionInventoryReadsCanonicalPreviewPages(t *testing.T) {
	now := time.Now().UTC()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/terminal-sessions" {
			if r.URL.Query().Get("closed") != "false" {
				t.Errorf("completion listed closed terminal sessions: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"sessions": []any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0, "next_offset": nil}}})
			return
		}
		if r.URL.Path != "/v1/previews" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls++
		if r.URL.Query().Get("state") != "active" {
			t.Errorf("completion walked previews without state=active: %s", r.URL.RawQuery)
		}
		id, next := "preview_first", "page-two"
		if r.URL.Query().Get("cursor") == "page-two" {
			id, next = "preview_second", ""
		}
		lease := api.PreviewLease{Schema: api.PreviewTunnelSchemaV1, Kind: "preview_lease", ID: id, AccountID: "account_owner", ActorID: "account_owner", OwnerMachineID: "machine_owner", OwnerSessionID: "session_owner", OwnerSessionKind: "foreground", Target: api.PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public", Endpoint: "https://preview.example.test", EndpointState: "ready", LeaseDeadline: now.Add(time.Hour), State: "ready", AllocationState: "ready", EdgeState: "ready", OriginState: "ready", CreatedAt: now, LastRenewedAt: now}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.PreviewLeasePage{Items: []api.PreviewLease{lease}, NextCursor: next}})
	}))
	defer server.Close()
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}, HTTPClient: server.Client()}
	items, err := source.ListCompletionItems(context.Background(), []api.UserMachine{{ID: "machine_owner", EnvironmentID: "environment_owner"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(items) != 3 || items[1].Kind != "preview" || items[1].Value != "preview_first" || items[2].Value != "preview_second" || items[1].EnvironmentID != "environment_owner" {
		t.Fatalf("preview completion did not consume canonical pages: calls=%d items=%v", calls, items)
	}
}

// Session completions come from one catalog request for the caller's own sessions without live
// host metadata, never from a request per machine.
func TestCompletionInventoryListsSessionsInOneCatalogRequest(t *testing.T) {
	catalogRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/previews":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.PreviewLeasePage{}})
		case "/v1/terminal-sessions":
			catalogRequests++
			if query := r.URL.Query(); query.Get("owner") != "mine" || query.Get("live") != "false" {
				t.Errorf("catalog query %q, want owner=mine and live=false", r.URL.RawQuery)
			}
			session := func(id, name, machine string) map[string]any {
				return map[string]any{"id": id, "name": name, "state": "running", "target": map[string]any{"kind": "machine", "id": machine, "machine_id": machine}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"sessions": []any{
				session("pts_a", "build", "machine_a"), session("pts_b", "pts_b", "machine_b"), session("pts_x", "other", "machine_unlisted"),
			}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 3, "next_offset": nil}}})
		default:
			t.Errorf("per-machine or unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}, HTTPClient: server.Client()}
	items, err := source.ListCompletionItems(context.Background(), []api.UserMachine{{ID: "machine_a", EnvironmentID: "env_a"}, {ID: "machine_b", EnvironmentID: "env_b"}})
	if err != nil {
		t.Fatal(err)
	}
	var sessions []string
	for _, item := range items {
		if item.Kind == "session" {
			sessions = append(sessions, item.EnvironmentID+"/"+item.Value)
		}
	}
	want := []string{"env_a/build", "env_a/pts_a", "env_b/pts_b"}
	if catalogRequests != 1 || len(sessions) != len(want) || sessions[0] != want[0] || sessions[1] != want[1] || sessions[2] != want[2] {
		t.Fatalf("catalog requests=%d sessions=%v, want 1 and %v", catalogRequests, sessions, want)
	}
}

// Machine names still complete when the preview or session lists cannot be read (the server answered
// /v1/previews 503 under load and every new daemon completed nothing); the failure is still reported.
func TestCompletionKeepsMachineNamesWhenPreviewsAndSessionsFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"control_unavailable","message":"unavailable"}}`))
	}))
	defer server.Close()
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}, HTTPClient: server.Client()}
	items, err := source.ListCompletionItems(context.Background(), []api.UserMachine{{ID: "machine_owner", Alias: "studio", EnvironmentID: "environment_owner", State: "online"}})
	if err == nil {
		t.Fatal("the preview and session failures were not reported")
	}
	names := map[string]bool{}
	for _, item := range items {
		if item.Kind == "machine" {
			names[item.Value] = true
		}
	}
	if !names["studio"] || !names["machine_owner"] {
		t.Fatalf("machine names lost: %v", items)
	}
}
