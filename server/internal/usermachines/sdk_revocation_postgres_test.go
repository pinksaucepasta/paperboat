package usermachines

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/apitokens"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/server/internal/nativeprivateaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
)

// Destination teardown and feed-outage timers are Lane R's separate admission checks.
func TestSDKLiveSessionRevocation(t *testing.T) {
	f, _, signer := newWorkspaceDescriptorFixture(t)
	ctx, token, session := sdkSession(t, f, "exec")
	for i, capability := range []string{"files", "private_access"} {
		if _, err := f.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.api_token_scope_rules(token_id,entry,target_kind,target_id,action,ports) VALUES($1,$2,'machine',$3,$4,'[5432,5433)')`, token, i+1, f.machine, capability); err != nil {
			t.Fatal(err)
		}
	}
	exec, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_revoke", true)
	if err != nil {
		t.Fatal(err)
	}
	file, err := f.service.FileTransferDescriptor(ctx, f.account, "", f.machine, session, "", TransferIntent{Mode: "automatic", DeclaredBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	services := machineservices.New(f.store, "local.example.test")
	if err := services.RecordMachineServices(ctx, "env_"+f.suffix, f.machine, wire.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "sdk-revoke-0123456789", StartedAt: now, Generation: 1, Services: []wire.Port{{Port: 5432, AddressFamily: "ipv4"}}}, now); err != nil {
		t.Fatal(err)
	}
	resolver, _ := nativeprivateaccess.NewSQLResolver(f.store)
	service := nativeprivateaccess.Service{Resolver: resolver, Signer: signer, Issuer: f.service.issuer}
	request := nativeprivateaccess.Request{AccountID: f.account, UserID: f.account, CLIClientSessionID: session, OperationID: "operation_sdk_revoke_private", ResourceKind: "machine_service", ResourceID: f.machine, RouteID: "tcp:5432", Protocol: "tcp"}
	private, err := service.Issue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	topics := func() map[string]int64 {
		t.Helper()
		out := map[string]int64{}
		for _, topic := range []string{"peer_network", "machine_access"} {
			var generation int64
			err := f.store.SQL().QueryRowContext(ctx, `SELECT coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic=$2),0)`, f.machine, topic).Scan(&generation)
			if err != nil {
				t.Fatal(err)
			}
			out[topic] = generation
		}
		return out
	}
	before := topics()
	// Admission expiry must not make a long-running operation disappear from the revoke feed.
	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_access_sessions SET expires_at=now()-interval '1 minute' WHERE cli_client_session_id=$1`, session); err != nil {
		t.Fatal(err)
	}
	tokens := apitokens.New(f.store, f.service.audit, config.Default().APITokens, "")
	if _, err := tokens.Revoke(ctx, f.account, workspace.Scope{ID: "personal"}, token); err != nil {
		t.Fatal(err)
	}
	for topic, generation := range topics() {
		if generation <= before[topic] {
			t.Fatalf("revocation did not publish %s", topic)
		}
	}
	for _, id := range []any{exec.Auth["access_session_id"], file.Auth["access_session_id"], private.Target.AccessSessionID} {
		var revoked bool
		if err := f.store.SQL().QueryRowContext(ctx, `SELECT state='revoked' AND revoked_at IS NOT NULL FROM paperboat.user_machine_access_sessions WHERE id=$1`, id).Scan(&revoked); err != nil || !revoked {
			t.Fatal("live operation retained access after token revoke")
		}
	}
	live := make([]string, 0, 2)
	for _, d := range []struct {
		auth  map[string]any
		class string
	}{{exec.Auth, "exec_operation"}, {file.Auth, "file_transfer"}} {
		raw, _ := d.auth["token"].(string)
		claims, err := signer.VerifyCredential(raw, f.service.issuer, d.class, now)
		if err != nil {
			t.Fatal(err)
		}
		live = append(live, claims.JTI)
	}
	edge := controlplane.NewEdgeService(f.store, "")
	revoked, err := edge.OperationRevocations(ctx, "env_"+f.suffix, live)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, id := range revoked {
		found[id] = true
	}
	for _, id := range live {
		if !found[id] {
			t.Fatal("admitted operation missing from host revocation feed")
		}
	}
	foreign, err := edge.OperationRevocations(ctx, "foreign-environment", live)
	if err != nil || len(foreign) != 0 {
		t.Fatal("host read another environment's revocations")
	}
	if _, err := edge.OperationRevocations(ctx, "env_"+f.suffix, make([]string, controlplane.MaxLiveOperationJTIs+1)); err == nil {
		t.Fatal("unbounded live credential list accepted")
	}
	for _, invalid := range [][]string{nil, {""}, {"duplicate_jti", "duplicate_jti"}, {strings.Repeat("x", 129)}} {
		if _, err := edge.OperationRevocations(ctx, "env_"+f.suffix, invalid); !errors.Is(err, controlplane.ErrLiveOperationJTIsInvalid) {
			t.Fatal("invalid host watch list was not rejected")
		}
	}
	if _, err := resolver.MachineServiceCurrent(ctx, request, private.Target.AccessSessionID, private.Target); err == nil {
		t.Fatal("revoked private operation renewed its authorization lease")
	}
	if _, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_after_revoke", true); err == nil {
		t.Fatal("revoked token obtained new descriptor")
	}
	testSDKRevocationFeedBounds(t, f, session, edge, live)
}

func testSDKRevocationFeedBounds(t *testing.T, f workspaceMachineFixture, session string, edge *controlplane.EdgeService, live []string) {
	ctx := f.ctx
	// A bounded feed must fail closed rather than silently omit a revoked operation.
	if _, err := f.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_access_sessions
 (credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,state,revoked_at,helper_terminal_session_id,capabilities,operation_id)
 SELECT 'control_plane_signed','overflow_'||$1||'_'||g,$2,$3,$4,$5,'',now()+interval '1 minute','revoked',now(),'overflow_jti_'||$1||'_'||g,ARRAY['exec'],'operation_overflow'
 FROM generate_series(1,10001) g`, f.suffix, f.machine, f.account, "env_"+f.suffix, session); err != nil {
		t.Fatal(err)
	}
	if _, err := edge.Revocations(ctx); err == nil {
		t.Fatal("global revocation feed silently truncated credentials")
	}
	if _, err := edge.OperationRevocations(ctx, "env_"+f.suffix, live); err == nil {
		t.Fatal("revocation feed silently truncated live-operation revocations")
	}

	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_access_sessions SET expires_at=now()-interval '1 minute' WHERE id LIKE $1`, "overflow_"+f.suffix+"_%"); err != nil {
		t.Fatal(err)
	}
	if current, err := edge.OperationRevocations(ctx, "env_"+f.suffix, live); err != nil || len(current) != 2 {
		t.Fatal("completed credential history filled the live host revocation feed")
	}

}
