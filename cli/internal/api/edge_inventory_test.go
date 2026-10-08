package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestHostedEdgeInventoryRejectsUntrustedPaginationAndStatus(t *testing.T) {
	for _, response := range []string{
		`{"data":{"items":[{"id":"edge_a","region":"fsn1","status":"healthy"}],"observed_at":"2026-09-23T00:00:00Z"}}`,
		`{"data":{"items":[{"id":"edge_a","region":"fsn1","status":"ready"}],"next_cursor":"edge_other","observed_at":"2026-09-23T00:00:00Z"}}`,
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
