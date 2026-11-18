package selfhost

import (
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

// Switching an account to only its own relays hands the Paperboat relays over make-before-break: each is listed
// with a close time one grace away (its own relay is not), a relay back in the pool cancels its handover, and another
// account is untouched.
func TestRelayPoolSwitchListsHandoversOnPostgres(t *testing.T) {
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
	t.Cleanup(func() { store.Close() })
	ctx := t.Context()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	stamp := randomIdentity()
	owner, other := "relay_owner_"+stamp, "relay_other_"+stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	for _, id := range []string{owner, other} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, id, id+"@example.test")
	}
	own := scopedInstallationFixture(t, store, "relay-handover-test-key", owner, "relay")
	exec(`UPDATE paperboat.control_tunnel_nodes SET roles=ARRAY['relay'],state='ready',ready=true WHERE id=$1`, own.NodeID)
	managed := "relay_managed_" + stamp
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,'default','v1','epoch','relay.example.test',443,444,'ready',true,now(),1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100,0,now(),now()+interval '1 hour')`, managed)
	t.Cleanup(func() {
		store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, managed)
		store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id IN ($1,$2)`, owner, other)
		store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.users WHERE id IN ($1,$2)`, owner, other)
	})
	service := New(store, "relay-handover-test-key")
	handovers := func(account string) map[string]time.Time {
		t.Helper()
		rows, err := store.SQL().QueryContext(ctx, `SELECT node_id,close_at FROM paperboat.relay_pool_handovers WHERE account_id=$1`, account)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]time.Time{}
		for rows.Next() {
			var node string
			var at time.Time
			if err = rows.Scan(&node, &at); err != nil {
				t.Fatal(err)
			}
			out[node] = at
		}
		return out
	}
	if _, err = service.SetPool(ctx, owner, "relay", Policy{Mode: "self-hosted-only", InstallationIDs: []string{own.InstallationID}}); err != nil {
		t.Fatal(err)
	}
	got := handovers(owner)
	at, ok := got[managed]
	if len(got) != 1 || !ok || at.Before(time.Now().Add(relaylimits.PoolHandoverGrace-time.Minute)) || at.After(time.Now().Add(relaylimits.PoolHandoverGrace+time.Minute)) {
		t.Fatalf("handovers after the switch to own relays: %v", got)
	}
	if len(handovers(other)) != 0 {
		t.Fatalf("another account was handed over: %v", handovers(other))
	}
	// A repeated switch keeps the first deadline; a relay back in the pool cancels its handover.
	if _, err = service.SetPool(ctx, owner, "relay", Policy{Mode: "self-hosted-only", InstallationIDs: []string{own.InstallationID}}); err != nil {
		t.Fatal(err)
	}
	if again := handovers(owner)[managed]; !again.Equal(at) {
		t.Fatalf("a repeated switch moved the deadline: %v -> %v", at, again)
	}
	// Back to the shared pool alone: Paperboat's relay returns (its handover is cancelled), its own leaves.
	if _, err = service.SetPool(ctx, owner, "relay", Policy{Mode: "mixed", InstallationIDs: []string{}}); err != nil {
		t.Fatal(err)
	}
	if got = handovers(owner); len(got) != 1 || got[own.NodeID].IsZero() {
		t.Fatalf("switching back: handovers %v, want only its own relay", got)
	}
	if _, err = service.SetPool(ctx, owner, "relay", Policy{Mode: "mixed", InstallationIDs: []string{own.InstallationID}}); err != nil {
		t.Fatal(err)
	}
	if got = handovers(owner); len(got) != 0 {
		t.Fatalf("a pool with both relays left handovers: %v", got)
	}
}
