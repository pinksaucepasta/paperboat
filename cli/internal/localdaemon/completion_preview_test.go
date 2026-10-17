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
		if r.URL.Path == "/v1/machines/machine_owner/terminal-sessions" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0, "next_offset": nil}}})
			return
		}
		if r.URL.Path != "/v1/previews" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls++
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
