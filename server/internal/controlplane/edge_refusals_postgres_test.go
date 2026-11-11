package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// A heartbeat's flood-protection refusals add up per hostname, edge and hour; malformed entries are
// skipped, and counts past retention are deleted by the same writer.
func TestEdgeRefusalsAccumulateHourlyAndExpire(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	host, old := "busy-"+suffix+".example.test", "old-"+suffix+".example.test"
	node := "node_refusals_" + suffix
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.edge_refusals WHERE edge_node_id=$1`, node)
	})
	now := time.Date(2026, 11, 11, 14, 20, 0, 0, time.UTC)
	service := NewEdgeService(store, "credential")
	service.clock = func() time.Time { return now }
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.edge_refusals(hostname,edge_node_id,hour,refused) VALUES($1,$2,$3,5)`, old, node, now.Add(-edgeRefusalRetention-time.Hour)); err != nil {
		t.Fatal(err)
	}
	service.recordEdgeRefusals(ctx, node, []edgeHostRefusals{{Hostname: host, Count: 3}, {Hostname: "", Count: 9}, {Hostname: "neg-" + suffix, Count: -1}})
	service.recordEdgeRefusals(ctx, node, []edgeHostRefusals{{Hostname: host, Count: 4}})
	var refused, rows int64
	var hour time.Time
	if err := store.SQL().QueryRowContext(ctx, `SELECT refused, hour FROM paperboat.edge_refusals WHERE hostname=$1 AND edge_node_id=$2`, host, node).Scan(&refused, &hour); err != nil {
		t.Fatal(err)
	}
	if refused != 7 || !hour.Equal(now.Truncate(time.Hour)) {
		t.Fatalf("refused=%d hour=%v, want 7 in %v", refused, hour, now.Truncate(time.Hour))
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.edge_refusals WHERE edge_node_id=$1`, node).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v, want only the current count (malformed skipped, expired deleted)", rows, err)
	}
}

// A taken-down hostname's decisions and quota denials are withheld from the edge snapshot and the
// name is listed with its legal flag; other names are untouched.
func TestWithholdBlockedRemovesTakenDownNames(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taken, quota, open, restored := "taken-"+suffix+".example.test", "quota-"+suffix+".example.test", "open-"+suffix+".example.test", "restored-"+suffix+".example.test"
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.publication_blocks WHERE hostname = ANY($1)`, []string{taken, quota, restored})
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.publication_blocks(hostname,blocked,legal,reason) VALUES($1,true,true,'case'),($2,true,false,'case'),($3,false,false,'case')`, taken, quota, restored); err != nil {
		t.Fatal(err)
	}
	decision := func(host string) wire.IngressDecision {
		var d wire.IngressDecision
		d.Binding.Hostname = host
		return d
	}
	decisions := []wire.IngressDecision{decision(taken), decision(open), decision(restored)}
	denied := []wire.IngressQuotaDenial{{Hostname: quota}}
	blocked, err := withholdBlocked(ctx, store, &decisions, &denied)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].Binding.Hostname != open || decisions[1].Binding.Hostname != restored || len(denied) != 0 {
		t.Fatalf("decisions=%v denied=%v", decisions, denied)
	}
	got := map[string]bool{}
	for _, b := range blocked {
		got[b.Hostname] = b.Legal
	}
	if len(got) != 2 || got[taken] != true || got[quota] != false {
		t.Fatalf("blocked=%v", blocked)
	}
}
