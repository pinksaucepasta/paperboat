package runtimeattachment

import (
	"strings"
	"testing"
)

func TestAttachTunnelSyncCanFindEndpointsByAgentWithoutScanningPostgres(t *testing.T) {
	f := newAgentFixture(t, "attach_endpoint_index")
	tx, err := f.database.SQL().BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// A tiny fixture is legitimately cheaper to scan. Discourage that so the
	// plan proves the production lookup has an index that covers removed rows.
	if _, err := tx.ExecContext(f.ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(f.ctx, `EXPLAIN (COSTS OFF)
 SELECT id FROM paperboat.endpoints WHERE agent_machine_id=$1 AND kind='tunnel'`, f.machine)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "Index Cond: (agent_machine_id =") {
		t.Fatalf("attach tunnel sync has no index lookup by agent:\n%s", plan.String())
	}
}
