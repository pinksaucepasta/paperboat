package peersessions

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Every configuration used to plan the expanded inspector authorization view,
// even when no credential existed: 120-424 ms of CPU per request on prod.
// Measure planner time, not elapsed request time, so host scheduling cannot fail it.
func TestInspectorAdmissionPlanningIsBoundedOnPostgres(t *testing.T) {
	f := newEndpointsFixture(t)
	conn, err := f.store.SQL().Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var total float64
	for i := 0; i < 21; i++ {
		var raw []byte
		err := conn.QueryRowContext(f.ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+inspectorAdmissionsQuery,
			f.account+"_m_0", f.account, time.Now(), []string{}).Scan(&raw)
		if err != nil {
			t.Fatal(err)
		}
		var plan []struct {
			PlanningTime float64 `json:"Planning Time"`
		}
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		if len(plan) != 1 {
			t.Fatalf("expected one plan, got %d", len(plan))
		}
		if i > 0 {
			total += plan[0].PlanningTime
		}
	}
	if total > 200 {
		t.Fatalf("20 warm inspector lookups spent %.1f ms planning; budget 200 ms", total)
	}
	t.Logf("20 warm inspector lookups: %.3f ms planning", total)
}

// A new database connection planned the expanded inspector view on its first configuration (the function keeps its
// generic plan per connection afterwards): 75-115 ms on prod, 7 ms with join_collapse_limit=1 (migration 456).
// The best of several fresh connections, so a loaded host cannot fail it.
func TestInspectorAdmissionFirstCallOfANewConnectionPlansFastOnPostgres(t *testing.T) {
	f := newEndpointsFixture(t)
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	var times []time.Duration
	for range 6 {
		conn, err := pgx.Connect(f.ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		_, err = conn.Exec(f.ctx, `SELECT * FROM paperboat.peer_network_inspector_admissions($1,$2,now(),'{}')`, f.account+"_m_0", f.account)
		times = append(times, time.Since(started))
		_ = conn.Close(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	}
	best := slices.Min(times)
	t.Logf("RESULT first inspector lookup on a new connection: best of %d %v (all %v)", len(times), best, times)
	if best > 40*time.Millisecond {
		t.Fatalf("the first inspector lookup of a new connection took %v at best, want planning under 40 ms", best)
	}
}
