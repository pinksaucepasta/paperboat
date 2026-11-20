package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

func TestHostedEdgeInventoryRejectsUntrustedPaginationAndStatus(t *testing.T) {
	for _, response := range []string{
		`{"data":{"items":[{"id":"frankfurt","name":"Frankfurt","status":"healthy"}],"observed_at":"2026-09-23T00:00:00Z"}}`,
		`{"data":{"items":[{"id":"frankfurt","name":"Frankfurt","status":"ready"}],"next_cursor":"mumbai","observed_at":"2026-09-23T00:00:00Z"}}`,
		`{"data":{"items":[{"id":"frankfurt","status":"ready"}],"observed_at":"2026-09-23T00:00:00Z"}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(response))
		}))
		_, err := New(server.URL, config.Credential{AccessToken: "test"}, server.Client()).ListHostedEdges(context.Background())
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "edge inventory") {
			t.Fatalf("accepted invalid inventory %s: %v", response, err)
		}
	}
}

func TestHostedRelayInventoryReadsRegions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/relays" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":"frankfurt","name":"Frankfurt","status":"ready"}],"observed_at":"2026-09-23T00:00:00Z"}}`))
	}))
	defer server.Close()
	page, err := New(server.URL, config.Credential{AccessToken: "test"}, server.Client()).ListHostedRelays(context.Background())
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "Frankfurt" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}
