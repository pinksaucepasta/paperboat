package controlplane

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
)

// `route add --wait` polls the operation until it succeeded, but nothing marked a route.create or
// route.patch operation done: the tunnel was ready and the operation stayed running forever.
func TestRouteOperationSucceedsOnceTheEdgeServesTheCurrentRouteOnPostgres(t *testing.T) {
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
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("routeop_%d", time.Now().UnixNano()), now)
	f.insertBase(t, database)
	defer func() {
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, f.accountID)
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID)
	}()
	f.insertEngine(t, database)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	svc := &EdgeService{store: database, clock: func() time.Time { return now }}
	store, err := previewtunnelstore.New(database)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"route.create", "route.patch"} {
		exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id) VALUES($1,$2,$1,decode(repeat('00',32),'hex'),$4,'route',$3,'connecting','running',60,'changed',$1)`, "op_"+kind+"_"+f.suffix, f.accountID, f.routeID, kind)
	}
	state := func(kind string) string {
		t.Helper()
		op, err := store.GetOperation(ctx, f.accountID, "op_"+kind+"_"+f.suffix)
		if err != nil {
			t.Fatal(err)
		}
		return op.State
	}
	for _, kind := range []string{"route.create", "route.patch"} {
		if got := state(kind); got != "running" {
			t.Fatalf("%s while the assignment is only staged = %s", kind, got)
		}
	}
	if err := svc.ObserveRoutes(ctx, f.nodeID, []RouteObservation{f.observation(f.nodeID, f.epoch, f.carrierSession(), "ready")}); err != nil {
		t.Fatal(err)
	}
	// A patch after the edge went ready raises the route generation: it is done only when the edge serves that one.
	exec(`UPDATE paperboat.tunnel_routes SET generation=generation+1 WHERE id=$1`, f.routeID)
	if got := state("route.patch"); got != "running" {
		t.Fatalf("route.patch before the edge serves the new generation = %s", got)
	}
	exec(`UPDATE paperboat.tunnel_routes SET generation=generation-1 WHERE id=$1`, f.routeID)
	for _, kind := range []string{"route.create", "route.patch"} {
		if got := state(kind); got != "succeeded" {
			t.Fatalf("%s once the edge serves the route = %s", kind, got)
		}
	}
}
