package privateaccess

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
)

// F21-18: a machine waits on the private_routes topic instead of re-fetching its routes every second. A change
// to what the routes answer moves the account's generation and wakes a waiter; time-only refreshes publish nothing.
func TestPrivateRoutesTopicMovesOnRouteChangesOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newAccessorAcceptanceFixture(fmt.Sprint(time.Now().UnixNano()), now)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = database.SQL().ExecContext(bg, `DELETE FROM paperboat.control_environments WHERE id IN (SELECT environment_id FROM paperboat.user_machines WHERE id=$1)`, f.hostID)
		_, _ = database.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id IN ($1,$2)`, f.accountID, f.otherAccountID)
		_, _ = database.SQL().ExecContext(bg, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID)
	})
	f.insert(t, database)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	generation := func(account string) int64 {
		t.Helper()
		var g int64
		if err := database.SQL().QueryRowContext(ctx, `SELECT coalesce(max(generation),0) FROM paperboat.account_topic_generations WHERE account_id=$1 AND topic='private_routes'`, account).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	hub := machineevents.NewHub(database.Pool(), database.Direct())
	go func() {
		for ctx.Err() == nil {
			_ = hub.ListenFor(ctx, 10*time.Second)
		}
	}()
	time.Sleep(300 * time.Millisecond)

	base := generation(f.accountID)
	if base == 0 {
		t.Fatal("serving the route did not publish private_routes")
	}
	// No change: lease renewal, observation refresh and a carrier renewal that only moves expires_at.
	exec(`UPDATE paperboat.preview_leases SET lease_deadline=lease_deadline+interval '1 minute', last_renewed_at=now() WHERE id=$1`, f.previewID)
	exec(`UPDATE paperboat.endpoint_edge_assignments SET observed_at=now() WHERE endpoint_id=$1`, f.previewID)
	exec(`UPDATE paperboat.runtime_carrier_attachments SET expires_at=expires_at+interval '1 minute', admission=jsonb_set(admission,'{expires_at}','"2099-01-01T00:00:00Z"') WHERE machine_id=$1`, f.hostID)
	if got := generation(f.accountID); got != base {
		t.Fatalf("time-only refreshes moved private_routes %d -> %d", base, got)
	}
	// A waiter that knows the generation is woken by a route change and by nothing else.
	wait := func() <-chan map[string]int64 {
		out := make(chan map[string]int64, 1)
		known := map[string]int64{machineevents.TopicPrivateRoutes: generation(f.accountID)}
		go func() {
			changed, _ := hub.Wait(ctx, f.accessorID, known, 10*time.Second)
			out <- changed
		}()
		time.Sleep(200 * time.Millisecond)
		return out
	}
	for name, change := range map[string]string{
		"edge stops serving it":  `UPDATE paperboat.endpoint_edge_assignments SET observed_ready=false WHERE endpoint_id=$1`,
		"edge serves it again":   `UPDATE paperboat.endpoint_edge_assignments SET observed_ready=true WHERE endpoint_id=$1`,
		"audience changes":       `UPDATE paperboat.endpoints SET access_mode='public' WHERE id=$1`,
		"route hostname changes": `UPDATE paperboat.endpoint_routes SET hostname='moved-'||hostname, revision=revision+1 WHERE endpoint_id=$1`,
		"preview stops":          `UPDATE paperboat.preview_leases SET terminal_state='stopped' WHERE id=$1`,
	} {
		before := generation(f.accountID)
		waiting := wait()
		started := time.Now()
		exec(change, f.previewID)
		select {
		case changed := <-waiting:
			if changed[machineevents.TopicPrivateRoutes] <= before || time.Since(started) > 3*time.Second {
				t.Fatalf("%s: wait answered %v after %v, want private_routes > %d", name, changed, time.Since(started), before)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: waiter was not woken", name)
		}
	}
	if got := generation(f.otherAccountID); got != 0 {
		t.Fatalf("another account's private_routes moved to %d", got)
	}
}
