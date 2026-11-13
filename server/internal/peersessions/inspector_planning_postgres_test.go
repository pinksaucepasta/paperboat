package peersessions

import (
	"encoding/json"
	"testing"
	"time"
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
