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
	"github.com/pinksaucepasta/paperboat/server/internal/hosthealth"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
)

type renewalNodes map[string]string

func (n renewalNodes) RuntimeNode(_ context.Context, credential string) (string, error) {
	if node, ok := n[credential]; ok {
		return node, nil
	}
	return "", nodeauth.ErrNotFound
}
func (renewalNodes) RuntimeCredential(context.Context, string) (string, error) {
	return "", nodeauth.ErrNotFound
}
func (renewalNodes) IsSelfHosted(context.Context, string) (bool, error) { return false, nil }

// The certificate renewal alert rides in each edge's authenticated heartbeat: a failing check raises that node's
// alert, a success clears it, and another node's credential cannot raise or clear it.
func TestHeartbeatCertificateRenewalRaisesAndClearsOnlyThatNodesAlertPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = db.Migrate(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	a, b := fmt.Sprintf("cert-a-%d", suffix), fmt.Sprintf("cert-b-%d", suffix)
	for _, node := range []string{a, b} {
		if _, err = store.SQL().ExecContext(t.Context(), `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,'default','1.0','epoch','ready',true,now(),'region','domain',ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour')`, node); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id = ANY($1)`, []string{a, b})
	})
	service := NewEdgeService(store, strings.Repeat("m", 40))
	service.SetNodeCredentials(renewalNodes{"cred-a": a, "cred-b": b})
	handler := service.Handler()
	beat := func(credential, node, renewal string) int {
		body := fmt.Sprintf(`{"edge_node_id":%q,"process_epoch":"epoch","ready":true,"at":%q%s}`, node, time.Now().UTC().Format(time.RFC3339Nano), renewal)
		r := httptest.NewRequest(http.MethodPost, "/v1/nodes/heartbeat", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	health := hosthealth.NewService(store)
	entry := func(node string) map[string]any {
		entries, err := health.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Source == "edge-certificate:node-"+node {
				var payload map[string]any
				if err := json.Unmarshal(e.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				return payload
			}
		}
		return nil
	}
	checked := time.Now().UTC().Format(time.RFC3339)
	failing := fmt.Sprintf(`,"certificate_renewal":{"ok":false,"error":"certbot failed","checked_at":%q}`, checked)
	healthy := fmt.Sprintf(`,"certificate_renewal":{"ok":true,"error":"","checked_at":%q}`, checked)

	// Raise: A's failing check is A's alert.
	if code := beat("cred-a", a, failing); code/100 != 2 {
		t.Fatalf("A heartbeat = %d", code)
	}
	if got := entry(a); got == nil || got["ok"] != false || got["error"] != "certbot failed" || got["stale_after_seconds"] != float64(86400) {
		t.Fatalf("A entry after failing check = %v", got)
	}
	// B's credential cannot clear (or write) A's: a heartbeat naming A is refused and A stays failing.
	if code := beat("cred-b", a, healthy); code/100 == 2 {
		t.Fatalf("B's credential heartbeating as A = %d, want refused", code)
	}
	if code := beat("cred-b", b, healthy); code/100 != 2 {
		t.Fatalf("B heartbeat = %d", code)
	}
	if got := entry(a); got == nil || got["ok"] != false {
		t.Fatalf("A entry after B's heartbeats = %v, want still failing", got)
	}
	if got := entry(b); got == nil || got["ok"] != true {
		t.Fatalf("B entry = %v", got)
	}
	// Clear: A's next successful check clears A's alert.
	if code := beat("cred-a", a, healthy); code/100 != 2 {
		t.Fatalf("A heartbeat = %d", code)
	}
	if got := entry(a); got == nil || got["ok"] != true {
		t.Fatalf("A entry after a successful check = %v, want cleared", got)
	}
	// A malformed check is refused and leaves the entry as it was.
	for _, bad := range []string{`,"certificate_renewal":{"ok":false,"error":"x"}`, `,"certificate_renewal":{"ok":false,"error":"` + strings.Repeat("x", maxCertificateRenewalError+1) + `","checked_at":"` + checked + `"}`} {
		if code := beat("cred-a", a, bad); code/100 == 2 {
			t.Fatalf("malformed renewal accepted: %d", code)
		}
	}
	if got := entry(a); got == nil || got["ok"] != true {
		t.Fatalf("A entry after refused reports = %v", got)
	}
	if code := beat("cred-a", a, `,"certificate_renewal":{"ok":false,"error":"x","checked_at":"0000-01-01T00:00:00Z"}`); code/100 == 2 {
		t.Fatalf("year-zero check accepted: %d", code)
	}
	// A failing check stays until the node reports another: a heartbeat without a check (status file missing or
	// unreadable) keeps it, and so does the node going offline.
	if code := beat("cred-a", a, failing); code/100 != 2 {
		t.Fatalf("A heartbeat = %d", code)
	}
	if code := beat("cred-a", a, ""); code/100 != 2 {
		t.Fatalf("A heartbeat without a check = %d", code)
	}
	if got := entry(a); got == nil || got["ok"] != false {
		t.Fatalf("A entry after a heartbeat without a check = %v, want still failing", got)
	}
	if _, err := store.SQL().ExecContext(t.Context(), `UPDATE paperboat.control_tunnel_nodes SET state='offline' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	if got := entry(a); got == nil || got["ok"] != false {
		t.Fatalf("offline A entry = %v, want still failing", got)
	}
	// One node's unreadable stored value never breaks the snapshot for everyone: it shows as a failing check.
	if _, err := store.SQL().ExecContext(t.Context(), `UPDATE paperboat.control_tunnel_nodes SET observation='{"certificate_renewal":{"ok":true,"checked_at":"0000-01-01"}}' WHERE id=$1`, b); err != nil {
		t.Fatal(err)
	}
	if got := entry(b); got == nil || got["ok"] != false {
		t.Fatalf("B entry with an unreadable stored check = %v", got)
	}
	if got := entry(a); got == nil {
		t.Fatal("snapshot lost A beside B's unreadable value")
	}
}
