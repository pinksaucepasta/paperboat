package tunnelv1

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat/server/internal/teams/teamstest"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// Exercises durable scope, authorization and replay against the production SQL.
// The fixture uses the existing isolated remote PostgreSQL opt-in boundary.
func TestPublisherWorkspaceOwnershipOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	owner, host := f.addAccount(t, "workspace-owner")
	member, _ := f.addAccount(t, "workspace-member")
	slugSuffix := strings.ReplaceAll(f.suffix, "_", "-")
	teamA, teamB := "ws-a-"+slugSuffix, "ws-b-"+slugSuffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	ids := []string{"workspace-pa-" + f.suffix, "workspace-pb-" + f.suffix, "workspace-personal-" + f.suffix}
	previewID := "workspace-preview-" + f.suffix
	t.Cleanup(func() {
		exec(`DELETE FROM paperboat.preview_leases WHERE id=$1`, previewID)
		exec(`DELETE FROM paperboat.tunnels WHERE id=ANY($1::text[])`, ids)
		exec(`DELETE FROM paperboat.managed_hostnames WHERE resource_id=ANY($1::text[])`, append(ids, previewID))
		exec(`DELETE FROM paperboat.billing_workspaces WHERE team_id=ANY($1::text[])`, []string{teamA, teamB})
		exec(`DELETE FROM paperboat.teams WHERE team_id=ANY($1::text[])`, []string{teamA, teamB})
	})
	exec(`UPDATE paperboat.user_machines SET online=true WHERE id=$1`, host)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$3,1),($2,$3,1)`, teamA, teamB, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$3,1,'admin',true),($2,$3,1,'admin',true),($1,$4,1,'member',true),($2,$4,1,'member',true)`, teamA, teamB, owner, member)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$3,'active','team-pro',2,now()-interval '1 hour',now()+interval '1 hour'),($2,$4,'active','team-pro',2,now()-interval '1 hour',now()+interval '1 hour')`, "team:"+teamA, "team:"+teamB, teamA, teamB)
	selected := func(id string) context.Context {
		t.Helper()
		scope, err := workspace.Parse(id)
		if err != nil {
			t.Fatal(err)
		}
		return workspace.WithContext(ctx, scope)
	}
	ctxA, ctxB, personal := selected(teamA), selected(teamB), selected("personal")
	var initial CreateRecord
	for i, c := range []context.Context{ctxA, ctxB, personal} {
		id := ids[i]
		hash := sha256.Sum256([]byte(id))
		endpoint := testutil.EndpointID(id)
		input := CreateRecord{OperationID: "op-" + id, TunnelID: id, AccountID: owner, Name: "same-name", AccessMode: AccessPrivate, StableEndpointID: endpoint, StableEndpoint: "https://" + testutil.EndpointLabel(endpoint) + ".tunnels.example.test", HostID: host, ActorID: owner, AuditActorID: owner, ActorType: "host", AuditEventID: "audit-" + id, Origin: OriginRequest{Scheme: "http", Address: "127.0.0.1:3000"}, IdempotencyKey: id, RequestHash: hash, CorrelationID: id, RequestID: id}
		result, err := f.repository.Create(c, input)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if i == 0 {
			want = teamA
			initial = input
		}
		if i == 1 {
			want = teamB
		}
		if result.Tunnel.OwnerTeamID.String != want || result.Tunnel.AccountID != owner {
			t.Fatalf("ownership=%+v", result.Tunnel)
		}
		var bindings, grants int
		if want != "" {
			if err := f.database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_resource_bindings WHERE team_id=$1 AND resource_kind='tunnel' AND resource_id=$2`, want, id).Scan(&bindings); err != nil {
				t.Fatal(err)
			}
			if err := f.database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_resource_grants WHERE team_id=$1 AND resource_id=$2 AND account_id=$3`, want, id, owner).Scan(&grants); err != nil {
				t.Fatal(err)
			}
			if bindings != 1 || grants != 2 {
				t.Fatalf("binding/grants=%d/%d", bindings, grants)
			}
		}
	}
	for i, c := range []context.Context{ctxA, ctxB, personal} {
		items, err := f.repository.List(c, owner, nil, 10)
		if err != nil || len(items) != 1 || items[0].ID != ids[i] {
			t.Fatalf("scope %d items=%+v err=%v", i, items, err)
		}
	}
	if _, err := f.repository.Get(ctxB, owner, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace get: %v", err)
	}
	if _, err := f.repository.Create(ctxB, initial); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("cross-workspace replay: %v", err)
	}
	if _, _, err := f.repository.ResolveManagementAccount(ctxA, member, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("membership granted host execution: %v", err)
	}
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, teamA, host, owner)
	teamstest.SetMachineAccess(t, f.database, teamA, host, member, "tunnel_manage")
	request := previewtunnelapi.RequestContext{Actor: previewtunnelapi.Actor{AccountID: member, ActorID: member, Role: "user"}}
	scoped, mapped, _, err := scopeTunnelManagement(ctxA, f.repository, request, ids[0])
	if err != nil || mapped.Actor.AccountID != owner || mapped.Actor.ActorID != member {
		t.Fatalf("selected management %+v %v", mapped, err)
	}
	if _, _, err := f.repository.ResolveManagementAccount(ctxB, member, ids[1]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other-team grant borrowed: %v", err)
	}
	teamstest.SetMachineAccess(t, f.database, teamA, host, member)
	if err := f.database.InTx(scoped, func(ctx context.Context, tx *db.Tx) error { return authorizeTunnelManagementTx(ctx, tx) }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked resolved management: %v", err)
	}

	store, err := previewtunnelstore.New(f.database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := sha256.Sum256([]byte(previewID))
	previewInput := previewtunnelstore.CreatePreviewLeaseV1Input{OperationID: "op-" + previewID, LeaseID: previewID, AuditEventID: "audit-" + previewID, AccountID: owner, ActorID: owner, ActorType: "host", OwnerMachineID: host, OwnerSessionID: "session-" + previewID, OwnerSessionKind: "foreground", TargetScheme: "http", TargetAddress: "127.0.0.1:3000", AccessMode: "private", Endpoint: "https://" + testutil.EndpointLabel(previewID) + ".preview.example.test", LeaseDeadline: now.Add(time.Minute), RequestHash: hash[:], IdempotencyKey: previewID, CorrelationID: previewID, RequestID: previewID, SourceMachineID: host, Now: now}

	// Unpaid admission must fail before operation, hostname, grants or resource
	// writes. Restore paid entitlement and reuse the exact request to prove no
	// idempotency slot was consumed by either rejected create.
	snapshotCounts := func() string {
		t.Helper()
		var counts string
		err := f.database.SQL().QueryRowContext(ctx, `SELECT jsonb_build_array(
   (SELECT count(*) FROM paperboat.operations),
   (SELECT count(*) FROM paperboat.audit_events),
   (SELECT count(*) FROM paperboat.tunnels),
   (SELECT count(*) FROM paperboat.preview_leases),
   (SELECT count(*) FROM paperboat.managed_hostnames),
   (SELECT count(*) FROM paperboat.team_resource_bindings),
   (SELECT count(*) FROM paperboat.team_resource_grants),
   (SELECT count(*) FROM paperboat.tunnel_routes),
   (SELECT count(*) FROM paperboat.tunnel_config_generations))::text`).Scan(&counts)
		if err != nil {
			t.Fatal(err)
		}
		return counts
	}
	before := snapshotCounts()
	exec(`UPDATE paperboat.billing_workspaces SET state='past_due' WHERE team_id=$1`, teamA)
	unpaidTunnel := initial
	unpaidTunnel.OperationID = "unpaid-" + initial.OperationID
	unpaidTunnel.TunnelID = "unpaid-" + initial.TunnelID
	unpaidTunnel.Name = "unpaid-name"
	unpaidTunnel.AuditEventID = "unpaid-" + initial.AuditEventID
	unpaidTunnel.IdempotencyKey = "unpaid-" + initial.IdempotencyKey
	unpaidTunnel.StableEndpointID = testutil.EndpointID(unpaidTunnel.TunnelID)
	unpaidTunnel.StableEndpoint = "https://" + testutil.EndpointLabel(unpaidTunnel.TunnelID) + ".tunnels.example.test"
	if _, err := f.repository.Create(ctxA, unpaidTunnel); !errors.Is(err, previewtunnelstore.ErrTeamSubscriptionRequired) {
		t.Fatalf("unpaid tunnel create: %v", err)
	}
	if _, err := store.CreatePreviewLeaseV1(ctxA, previewInput); !errors.Is(err, previewtunnelstore.ErrTeamSubscriptionRequired) {
		t.Fatalf("unpaid preview create: %v", err)
	}
	if after := snapshotCounts(); before != after {
		t.Fatalf("denied publication persisted state: before=%s after=%s", before, after)
	}
	exec(`UPDATE paperboat.billing_workspaces SET state='active' WHERE team_id=$1`, teamA)
	if _, err := f.repository.Create(ctxA, unpaidTunnel); err != nil {
		t.Fatalf("paid tunnel recovery: %v", err)
	}
	ids = append(ids, unpaidTunnel.TunnelID)
	lease, err := store.CreatePreviewLeaseV1(ctxA, previewInput)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Lease.OwnerTeamID.String != teamA {
		t.Fatal("preview workspace was not persisted")
	}
	if _, err := store.GetPreviewLeaseV1(ctxB, owner, previewID); !errors.Is(err, previewtunnelstore.ErrNotFound) {
		t.Fatalf("cross-workspace preview get: %v", err)
	}
	exec(`UPDATE paperboat.team_members SET active=false WHERE team_id=$1 AND account_id=$2`, teamA, owner)
	if _, err := f.repository.Create(ctxA, initial); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("revoked creator replay: %v", err)
	}
	if _, err := store.StopPreviewLeaseV1(ctxA, previewtunnelstore.StopPreviewLeaseV1Input{ManagementAccountID: owner, OperationID: "stop-" + previewID, AuditEventID: "stop-audit-" + previewID, AccountID: owner, ActorID: owner, ActorType: "user", PreviewID: previewID, OwnerMachineID: host, OwnerSessionID: lease.Lease.OwnerSessionID, ExpectedGeneration: lease.Lease.Generation, RequestHash: hash[:], IdempotencyKey: "stop-" + previewID, CorrelationID: previewID, RequestID: previewID, Now: now}); !errors.Is(err, previewtunnelstore.ErrNotFound) {
		t.Fatalf("revoked preview stop: %v", err)
	}
}
