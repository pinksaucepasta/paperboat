package runtimeattachment

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
)

// Lane W scale matrix: an edge with endpoint deltas gets its endpoint routes only from the endpoint
// pull, as a full set once and then only what changed since its cursor; an idle poll carries no
// admission, a stopped preview arrives as a removed route, a new one as an admission.
func TestEndpointAdmissionDeltasOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "epdelta")
	ctx := f.ctx
	register := func(lease string) {
		t.Helper()
		host := strings.ReplaceAll(lease, "_", "-") + ".preview.example.test"
		f.exec(t, `INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline)
VALUES($1,$2,$3,$3,$4,'owner_session_delta','foreground','http','127.0.0.1:3000','public',now()+interval '10 minutes')`, lease, "https://"+host, f.account, f.machine)
		if err := f.database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			return endpoints.RegisterPreviewTx(ctx, tx, endpoints.Preview{LeaseID: lease, AccountID: f.account, AgentMachineID: f.machine, AccessMode: "public", Endpoint: "https://" + host})
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, second := f.prefix+"_first", f.prefix+"_second"
	register(first)
	deltas := []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityEndpointDeltas}
	if _, err := f.service.Pull(ctx, f.edge, deltas); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Pull(ctx, f.backupEdge, deltas); err != nil {
		t.Fatal(err)
	}
	request := f.request
	request.Capabilities = []string{runtimewire.CapabilityEndpoints}
	if a, err := f.service.Attach(ctx, f.proof, request); err != nil || !a.EndpointsServed {
		t.Fatalf("attach: served=%v %v", a.EndpointsServed, err)
	}
	// The full runtime pull of a delta edge leaves the endpoint routes out.
	if values, err := f.service.Pull(ctx, f.edge, deltas); err != nil || len(values) != 1 || values[0].EndpointID != "" {
		t.Fatalf("full runtime pull of a delta edge = %d admissions, %v", len(values), err)
	}
	full, err := f.service.PullEndpoints(ctx, f.edge, deltas, "")
	if err != nil || !full.Complete || full.Admissions == nil || len(*full.Admissions) != 1 || (*full.Admissions)[0].EndpointID != first || full.Cursor == "" {
		t.Fatalf("full endpoint pull = %+v, %v", full, err)
	}
	// Nothing changed since the cursor (rows older than the overlap): an empty delta.
	f.exec(t, `UPDATE endpoint_edge_assignments SET changed_at=now()-interval '1 minute' WHERE edge_node_id IN ($1,$2)`, f.node, f.backupNode)
	idle, err := f.service.PullEndpoints(ctx, f.edge, deltas, full.Cursor)
	if err != nil || idle.Complete || idle.Admissions == nil || len(*idle.Admissions) != 0 || len(idle.RemovedRouteIDs) != 0 || idle.ExpiresAt == nil {
		t.Fatalf("idle delta = %+v, %v", idle, err)
	}
	// A new preview arrives as an admission, a stopped one as a removed route.
	register(second)
	f.exec(t, `UPDATE preview_leases SET terminal_state='stopped',stopped_at=now() WHERE id=$1`, first)
	changed, err := f.service.PullEndpoints(ctx, f.edge, deltas, idle.Cursor)
	if err != nil || changed.Complete || changed.Admissions == nil || len(*changed.Admissions) != 1 || (*changed.Admissions)[0].EndpointID != second ||
		len(changed.RemovedRouteIDs) != 1 || changed.RemovedRouteIDs[0] != first {
		t.Fatalf("delta after a change = %+v, %v", changed, err)
	}
	// An access change arrives as the updated admission.
	f.exec(t, `UPDATE endpoint_edge_assignments SET changed_at=now()-interval '1 minute' WHERE edge_node_id IN ($1,$2)`, f.node, f.backupNode)
	f.exec(t, `UPDATE endpoints SET access_mode='private' WHERE id=$1`, second)
	access, err := f.service.PullEndpoints(ctx, f.edge, deltas, changed.Cursor)
	if err != nil || len(*access.Admissions) != 1 || (*access.Admissions)[0].RouteKind != runtimewire.EndpointRestrictedRouteKind {
		t.Fatalf("delta after an access change = %+v, %v", access, err)
	}
	// A carrier renewal (only expires_at inside its admission) is not a change.
	f.exec(t, `UPDATE endpoint_edge_assignments SET changed_at=now()-interval '1 minute' WHERE edge_node_id IN ($1,$2)`, f.node, f.backupNode)
	f.exec(t, `UPDATE runtime_carrier_attachments SET admission=jsonb_set(admission,'{expires_at}',to_jsonb(to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'))) WHERE machine_id=$1`, f.machine)
	renewed, err := f.service.PullEndpoints(ctx, f.edge, deltas, changed.Cursor)
	if err != nil || len(*renewed.Admissions) != 0 || len(renewed.RemovedRouteIDs) != 0 {
		t.Fatalf("a renewal produced a delta: %+v, %v", renewed, err)
	}
	// An old or missing cursor gets a full answer.
	if again, err := f.service.PullEndpoints(ctx, f.edge, deltas, "not-a-cursor"); err != nil || !again.Complete {
		t.Fatalf("invalid cursor = %+v, %v", again, err)
	}
}

// An edge without endpoint deltas decodes strictly: its answer has no cursor, removed list or
// expiry, exactly as before.
func TestEndpointPullWithoutDeltasKeepsItsShapeOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "epnodelta")
	snapshot, err := f.service.PullEndpoints(f.ctx, f.edge, []string{runtimewire.CapabilityEndpoints}, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil || string(raw) != `{"schema":"paperboat.runtime-carrier/v1","complete":true,"admissions":[]}` {
		t.Fatalf("answer to an edge without deltas = %s, %v", raw, err)
	}
}
