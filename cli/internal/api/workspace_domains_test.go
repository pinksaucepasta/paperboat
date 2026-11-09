package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestWorkspaceDomainCallsUseTheDomainsAPI(t *testing.T) {
	domain := WorkspaceDomain{ID: "wsd_0123456789abcdef", Hostname: "dev.example.com", State: "pending_dns"}
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var data any = domain
		if r.Method == http.MethodGet && r.URL.Path == "/v1/domains" {
			data = map[string]any{"items": []WorkspaceDomain{domain}}
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/domains" {
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["hostname"] != "dev.example.com" {
				t.Errorf("add body = %v", body)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "access-token"}, server.Client())
	ctx := context.Background()
	if items, err := client.ListWorkspaceDomains(ctx); err != nil || len(items) != 1 || items[0].Hostname != domain.Hostname {
		t.Fatalf("list = %+v %v", items, err)
	}
	if _, err := client.AddWorkspaceDomain(ctx, "dev.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetWorkspaceDomain(ctx, domain.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CheckWorkspaceDomain(ctx, domain.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveWorkspaceDomain(ctx, domain.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetWorkspaceDomain(ctx, "../etc"); err == nil {
		t.Fatal("an invalid id reached the server")
	}
	want := []string{"GET /v1/domains", "POST /v1/domains", "GET /v1/domains/" + domain.ID, "POST /v1/domains/" + domain.ID + "/check", "DELETE /v1/domains/" + domain.ID}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
}
