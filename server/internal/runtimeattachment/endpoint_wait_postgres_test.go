package runtimeattachment

import (
	"context"
	"strings"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
)

type endpointPullResult struct {
	answer  runtimewire.Snapshot
	err     error
	elapsed time.Duration
}

// A delta endpoint pull with wait_ms and nothing to report is held, and a write to an assignment of that edge answers
// it at once with the route, so every edge learns of a new preview together instead of at its next poll. A pull
// without a wait answers at once, and a write for another edge does not wake it.
func TestEndpointPullHeldUntilAnAssignmentIsWrittenOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "epwait")
	ctx := f.ctx
	hub := machineevents.NewHub(f.database.Pool(), f.database.Direct())
	f.service.Events = hub
	listening, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		for listening.Err() == nil {
			_ = hub.ListenFor(listening, time.Minute)
		}
	}()
	time.Sleep(500 * time.Millisecond) // LISTEN is attached; its start wakes every waiter once
	register := func(lease string) {
		t.Helper()
		host := strings.ReplaceAll(lease, "_", "-") + ".preview.example.test"
		f.exec(t, `INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline)
VALUES($1,$2,$3,$3,$4,'owner_session_wait','foreground','http','127.0.0.1:3000','public',now()+interval '10 minutes')`, lease, "https://"+host, f.account, f.machine)
		if err := f.database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			return endpoints.RegisterPreviewTx(ctx, tx, endpoints.Preview{LeaseID: lease, AccountID: f.account, AgentMachineID: f.machine, AccessMode: "public", Endpoint: "https://" + host})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for node, host := range map[string]string{f.node: "edge.example.test", f.backupNode: "backup.example.test"} {
		chain, pin := testEdgeCertificate(t, host)
		f.exec(t, `UPDATE control_tunnel_nodes SET carrier_server_certificate_chain_pem=$2,carrier_server_spki_sha256=$3 WHERE id=$1`, node, chain, pin)
	}
	register(f.prefix + "_first")
	deltas := []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityEndpointDeltas}
	for _, edge := range []previewattachment.PreviewEdgeIdentity{f.edge, f.backupEdge} {
		if _, err := f.service.Pull(ctx, edge, deltas); err != nil {
			t.Fatal(err)
		}
	}
	request := f.request
	request.Capabilities = []string{runtimewire.CapabilityEndpoints}
	if _, err := f.service.Attach(ctx, f.proof, request); err != nil {
		t.Fatal(err)
	}
	full, err := f.service.PullEndpoints(ctx, f.edge, deltas, "", "", 0)
	if err != nil || !full.Complete {
		t.Fatalf("full pull = %+v, %v", full, err)
	}
	backupFull, err := f.service.PullEndpoints(ctx, f.backupEdge, deltas, "", "", 0)
	if err != nil || !backupFull.Complete {
		t.Fatalf("backup full pull = %+v, %v", backupFull, err)
	}
	// Older than the overlap: nothing is left to report.
	f.exec(t, `UPDATE endpoint_edge_assignments SET changed_at=now()-interval '1 minute'`)
	pull := func(edge previewattachment.PreviewEdgeIdentity, since string, wait time.Duration) <-chan endpointPullResult {
		out := make(chan endpointPullResult, 1)
		go func() {
			started := time.Now()
			answer, err := f.service.PullEndpoints(ctx, edge, deltas, since, "", wait)
			out <- endpointPullResult{answer, err, time.Since(started)}
		}()
		return out
	}

	// Without wait_ms the pull answers at once, as before.
	if r := <-pull(f.edge, full.Cursor, 0); r.err != nil || len(flatten(r.answer)) != 0 || r.elapsed > time.Second {
		t.Fatalf("pull without a wait = %d routes, %v after %v", len(flatten(r.answer)), r.err, r.elapsed)
	}
	// A write for the other edge does not wake a held pull.
	held := pull(f.backupEdge, backupFull.Cursor, 800*time.Millisecond)
	other := pull(f.edge, full.Cursor, 800*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	f.exec(t, `UPDATE endpoint_edge_assignments SET changed_at=clock_timestamp() WHERE edge_node_id=$1`, f.node)
	if r := <-held; r.err != nil || r.elapsed < 700*time.Millisecond || len(flatten(r.answer)) != 0 {
		t.Fatalf("backup pull = %d routes after %v, %v; want it held for its whole wait", len(flatten(r.answer)), r.elapsed, r.err)
	}
	<-other
	f.exec(t, `UPDATE endpoint_edge_assignments SET changed_at=now()-interval '1 minute'`)

	// A new preview answers the held pull of each edge within 300 ms, with the route.
	heldFirst := pull(f.edge, full.Cursor, 5*time.Second)
	heldBackup := pull(f.backupEdge, backupFull.Cursor, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	second := f.prefix + "_second"
	written := time.Now()
	register(second)
	for name, ch := range map[string]<-chan endpointPullResult{"first edge": heldFirst, "second edge": heldBackup} {
		r := <-ch
		got := flatten(r.answer)
		if r.err != nil || len(got) != 1 || got[0].EndpointID != second || time.Since(written) > 300*time.Millisecond {
			t.Fatalf("%s: held pull = %d routes, %v, answered %v after the write", name, len(got), r.err, time.Since(written))
		}
	}
}
