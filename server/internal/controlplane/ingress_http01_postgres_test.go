package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// A verified edge receives the http-01 responses the server holds for it, and
// no other edge does. The responses are fetched in the same snapshot the edge
// uses for its routes, so no new edge-control endpoint is needed.
func TestIngressSnapshotCarriesHTTP01ResponsesPerEdgeOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	stamp := fmt.Sprint(time.Now().UnixNano())
	edge := "http01_edge_" + stamp
	other := "http01_other_" + stamp
	for _, id := range []string{edge, other} {
		if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,public_ingress_verified_at) VALUES($1,'default','1.0','epoch','ready',true,now(),'region','domain-'||$1,ARRAY['edge']::text[],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour',now())`, id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
		})
	}
	registry := tunnelcert.NewHTTP01Responses(time.Now)
	token := "http01_token_" + stamp
	if err := registry.Publish(ctx, tunnelcert.HTTP01Record{Hostname: "app.customer.test", Token: token, KeyAuthorization: token + ".thumb", EdgeNodeIDs: []string{edge}}); err != nil {
		t.Fatal(err)
	}
	service := NewEdgeService(database, "credential")
	service.SetHTTP01Responses(registry)
	snapshot := func(node string) []map[string]string {
		body := fmt.Sprintf(`{"edge_node_id":%q,"process_epoch":"epoch-0001"}`, node)
		request := httptest.NewRequest(http.MethodPost, "/v1/edge/ingress/desired-state", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		service.handleIngress(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("snapshot for %s: status %d body %s", node, recorder.Code, recorder.Body.String())
		}
		var out struct {
			HTTP01Responses []map[string]string `json:"http01_responses"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.HTTP01Responses
	}
	if got := snapshot(edge); len(got) != 1 || got[0]["token"] != token || got[0]["key_authorization"] != token+".thumb" || got[0]["hostname"] != "app.customer.test" {
		t.Fatalf("verified edge did not receive its response: %v", got)
	}
	if got := snapshot(other); len(got) != 0 {
		t.Fatalf("edge outside the domain received a response: %v", got)
	}
}
