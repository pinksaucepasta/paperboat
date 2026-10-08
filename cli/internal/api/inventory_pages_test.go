package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOffsetInventoriesCollectBeyondFirstPage(t *testing.T) {
	for _, resource := range []struct{ path, field string }{
		{"/v1/config-repositories", "items"}, {"/v1/team-inbox/requests", "requests"}, {"/v1/terminal-sessions", "sessions"}, {"/v1/team-invitations", "items"},
	} {
		t.Run(resource.path, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != resource.path || r.URL.Query().Get("limit") != "200" {
					t.Errorf("request %s", r.URL.String())
				}
				calls++
				next := any(nil)
				if calls == 1 {
					next = 1
				}
				wantOffset := "0"
				if calls == 2 {
					wantOffset = "1"
				}
				if r.URL.Query().Get("offset") != wantOffset {
					t.Errorf("offset=%s", r.URL.Query().Get("offset"))
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{resource.field: []map[string]string{{"id": "resource"}}, "pagination": map[string]any{"limit": 200, "offset": calls - 1, "next_offset": next, "total": 2}}})
			}))
			defer server.Close()
			items, err := collectOffsetInventory[map[string]string](context.Background(), New(server.URL, config.Credential{}, server.Client()), resource.path, resource.field, nil)
			if err != nil || len(items) != 2 || calls != 2 {
				t.Fatalf("items=%v calls=%d err=%v", items, calls, err)
			}
		})
	}
}
func TestOffsetInventoryRejectsNonadvancingOrEmptyContinuation(t *testing.T) {
	for _, response := range []string{
		`{"data":{"items":[{}],"pagination":{"next_offset":0}}}`,
		`{"data":{"items":[],"pagination":{"next_offset":200}}}`,
		`{"data":{"items":[{}],"pagination":{"next_offset":200}}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response)) }))
		_, err := collectOffsetInventory[map[string]string](context.Background(), New(server.URL, config.Credential{}, server.Client()), "/v1/config-repositories", "items", nil)
		server.Close()
		if err == nil {
			t.Fatalf("accepted %s", response)
		}
	}
}
func TestPendingInboxFiltersBeforePagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("owner") != "mine" || r.URL.Query().Get("state") != "pending" {
			t.Errorf("missing filters: %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"data":{"requests":[],"pagination":{"next_offset":null}}}`))
	}))
	defer server.Close()
	if _, err := New(server.URL, config.Credential{}, server.Client()).PendingTeamInboxRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryCandidatesExhaustProviderPages(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/config-repositories/candidates" || r.URL.Query().Get("limit") != "100" {
			t.Errorf("path=%s", r.URL.String())
		}
		next := "provider/next+page"
		if calls == 2 {
			if r.URL.Query().Get("cursor") != next {
				t.Errorf("cursor lost: %s", r.URL.String())
			}
			next = ""
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []ConfigRepositoryCandidate{{ExternalID: fmt.Sprint(calls)}}, "next_cursor": next}})
	}))
	defer server.Close()
	items, err := New(server.URL, config.Credential{}, server.Client()).ConfigRepositoryCandidates(t.Context())
	if err != nil || len(items) != 2 || calls != 2 {
		t.Fatalf("items=%v calls=%d err=%v", items, calls, err)
	}
}
