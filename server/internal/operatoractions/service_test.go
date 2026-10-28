package operatoractions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

type operatorCycleError struct{}

func (operatorCycleError) Error() string   { return "cycle" }
func (e operatorCycleError) Unwrap() error { return e }

type operatorUncomparableError []error

func (operatorUncomparableError) Error() string { return "uncomparable" }

func TestOperatorExpectedErrorRequiresCompletePureCause(t *testing.T) {
	if !operatorNoRowsOnly(fmt.Errorf("wrapped: %w", pgx.ErrNoRows)) {
		t.Fatal("pure wrapped no-row result was not recognized")
	}
	for _, err := range []error{
		errors.Join(pgx.ErrNoRows, syscall.EIO),
		errors.Join(pgx.ErrNoRows, errors.New("unknown failure")),
		operatorCycleError{},
		operatorUncomparableError{pgx.ErrNoRows},
	} {
		if operatorNoRowsOnly(err) {
			t.Fatalf("mixed or incomplete cause tree was accepted as a missing operator: %v", err)
		}
	}
	var typedNil *operatorCycleError
	if operatorNoRowsOnly(typedNil) {
		t.Fatal("typed nil was accepted as a missing operator")
	}
	if got := mapTeamError(fmt.Errorf("wrapped: %w", teams.ErrConflict)); got != ErrConflict {
		t.Fatalf("pure team conflict mapping = %v", got)
	}
	teamMixed := mapTeamError(errors.Join(teams.ErrConflict, syscall.EIO))
	if !errors.Is(teamMixed, syscall.EIO) || operatorExpectedErrorOnly(teamMixed, ErrConflict) {
		t.Fatalf("mixed team conflict lost its operational cause or became a safe conflict: %v", teamMixed)
	}
	if got := mapTunnelError(fmt.Errorf("wrapped: %w", tunnelv1.ErrGenerationConflict)); got != ErrConflict {
		t.Fatalf("pure tunnel conflict mapping = %v", got)
	}
	tunnelMixed := mapTunnelError(errors.Join(tunnelv1.ErrGenerationConflict, syscall.EIO))
	if !errors.Is(tunnelMixed, syscall.EIO) || operatorExpectedErrorOnly(tunnelMixed, ErrConflict) {
		t.Fatalf("mixed tunnel conflict lost its operational cause or became a safe conflict: %v", tunnelMixed)
	}
}

func TestAccountBanRevokesAuthorityAndReplaysExactResult(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	actor, target := "operator_"+uuid.NewString(), "opa_target_"+x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status,role) VALUES($1,$1,$1||'@test.invalid','active','user')`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,'csrf',now()+interval '1 hour')`, "sess_"+x, target, "hash_"+x); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,'cli','test','cli','linux','{}','active',now(),now())`, "cli_"+x, target); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_access_tokens(token_hash,cli_client_session_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, "access_"+x, "cli_"+x); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_refresh_tokens(token_hash,cli_client_session_id,state,expires_at,created_at) VALUES($1,$2,'active',now()+interval '1 hour',now())`, "refresh_"+x, "cli_"+x); err != nil {
		t.Fatal(err)
	}
	s := NewService(store)
	got, err := s.Execute(ctx, actor, "ban-key-"+x, "account_ban", target, "case:test", Input{Revision: 1})
	if err != nil || got.State != "suspended" {
		t.Fatalf("result=%#v err=%v", got, err)
	}
	replay, err := s.Execute(ctx, actor, "ban-key-"+x, "account_ban", target, "case:test", Input{Revision: 1})
	if err != nil || replay != got {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	if _, err := s.Execute(ctx, actor, "ban-key-"+x, "account_unban", target, "case:test", Input{Revision: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict=%v", err)
	}
	var revoked bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT revoked_at IS NOT NULL FROM paperboat.sessions WHERE id=$1`, "sess_"+x).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("revoked=%t err=%v", revoked, err)
	}
	var clientState, refreshState string
	var accessRevoked bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT s.state,r.state,a.revoked_at IS NOT NULL FROM paperboat.cli_client_sessions s JOIN paperboat.cli_refresh_tokens r ON r.cli_client_session_id=s.id JOIN paperboat.cli_access_tokens a ON a.cli_client_session_id=s.id WHERE s.id=$1`, "cli_"+x).Scan(&clientState, &refreshState, &accessRevoked); err != nil || clientState != "revoked" || refreshState != "revoked" || !accessRevoked {
		t.Fatalf("client=%s refresh=%s access=%t err=%v", clientState, refreshState, accessRevoked, err)
	}
	unbanned, err := s.Execute(ctx, actor, "unban-key-"+x, "account_unban", target, "case:test", Input{Revision: got.Version})
	if err != nil || unbanned.State != "active" {
		t.Fatalf("unban=%#v err=%v", unbanned, err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.cli_client_sessions WHERE id=$1`, "cli_"+x).Scan(&clientState); err != nil || clientState != "revoked" {
		t.Fatalf("unban restored credential state=%s err=%v", clientState, err)
	}
	if _, err := s.Execute(ctx, actor, "self-ban-"+x, "account_ban", actor, "case:test", Input{Revision: 1}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("self ban=%v", err)
	}
	concurrentTarget := "opa_concurrent_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status,role) VALUES($1,$1,$1||'@test.invalid','active','user')`, concurrentTarget); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := range errs {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Execute(ctx, actor, "concurrent-key-"+x, "account_ban", concurrentTarget, "case:test", Input{Revision: 1})
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent errors=%v", errs)
	}
	var receipts int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.operator_actions WHERE actor_user_id=$1 AND idempotency_key=$2`, actor, "concurrent-key-"+x).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("receipts=%d err=%v", receipts, err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_accounts SET role='support' WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(ctx, actor, "ban-key-"+x, "account_ban", target, "case:test", Input{Revision: 1}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("demoted replay=%v", err)
	}
}

func TestOperatorActionsRelayDrainResumeAndAuthority(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	actor, node := "operator_"+uuid.NewString(), "opa_relay_"+x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','operations')`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,roles,transports,version,region,failure_domain,capacity_limit,capacity_observed_at,registry_expires_at) VALUES($1,'default','1.0',$2,'ready',true,ARRAY['relay'],ARRAY['derp_quic','derp_wss'],1,'test','test-domain',100,now(),now()+interval '1 minute')`, node, "epoch_"+x); err != nil {
		t.Fatal(err)
	}
	s := NewService(store)
	drained, err := s.Execute(ctx, actor, "drain-key-"+x, "relay_drain", node, "case:test", Input{Revision: 1})
	if err != nil || drained.State != "draining" {
		t.Fatalf("drain=%#v err=%v", drained, err)
	}
	heartbeat, err := store.Queries().HeartbeatControlTunnelNode(ctx, dbsqlc.HeartbeatControlTunnelNodeParams{ID: node, ProcessEpoch: "epoch_" + x, Ready: true, Observation: []byte("{}"), Now: sql.NullTime{Time: time.Now(), Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	var ready bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT state,ready FROM paperboat.control_tunnel_nodes WHERE id=$1`, node).Scan(&state, &ready); err != nil || state != "draining" || ready {
		t.Fatalf("heartbeat state=%s ready=%t err=%v", state, ready, err)
	}
	resumed, err := s.Execute(ctx, actor, "resume-key-"+x, "relay_resume", node, "case:test", Input{Revision: heartbeat.Version})
	if err != nil || resumed.State != "registered" {
		t.Fatalf("resume=%#v err=%v", resumed, err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT ready FROM paperboat.control_tunnel_nodes WHERE id=$1`, node).Scan(&ready); err != nil || ready {
		t.Fatalf("resume ready=%t err=%v", ready, err)
	}
	failKey := "audit-fail-" + x
	fn := "fail_operator_audit_" + x
	trigger := "fail_operator_audit_trigger_" + x
	if _, err = store.SQL().ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION paperboat.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.idempotency_key=$key$%s$key$ THEN RAISE EXCEPTION 'forced audit failure'; END IF; RETURN NEW; END $$`, fn, failKey)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON paperboat.audit_events FOR EACH ROW EXECUTE FUNCTION paperboat.%s()`, trigger, fn)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON paperboat.audit_events`, trigger))
		store.SQL().ExecContext(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS paperboat.%s()`, fn))
	})
	if _, err = s.Execute(ctx, actor, failKey, "relay_drain", node, "case:test", Input{Revision: resumed.Version}); err == nil {
		t.Fatal("forced audit failure succeeded")
	}
	var version int64
	if err = store.SQL().QueryRowContext(ctx, `SELECT state,version FROM paperboat.control_tunnel_nodes WHERE id=$1`, node).Scan(&state, &version); err != nil || state != "registered" || version != resumed.Version {
		t.Fatalf("audit rollback state/version=%s/%d err=%v", state, version, err)
	}
	var failedReceipts int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.operator_actions WHERE actor_user_id=$1 AND idempotency_key=$2`, actor, failKey).Scan(&failedReceipts); err != nil || failedReceipts != 0 {
		t.Fatalf("failed receipts=%d err=%v", failedReceipts, err)
	}
	fresh, freshErr := store.Queries().HeartbeatControlTunnelNode(ctx, dbsqlc.HeartbeatControlTunnelNodeParams{ID: node, ProcessEpoch: "epoch_" + x, Ready: true, Observation: []byte("{}"), Now: sql.NullTime{Time: time.Now(), Valid: true}})
	if freshErr != nil || fresh.State != "ready" || !fresh.Ready {
		t.Fatalf("resume did not recover on heartbeat: %s %t %v", fresh.State, fresh.Ready, freshErr)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_accounts SET role='support' WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(ctx, actor, "denied-key-"+x, "relay_drain", node, "case:test", Input{Revision: resumed.Version}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("support drain=%v", err)
	}
}

func TestOperatorActionsTunnelAndTeamUseAuthoritativeTransitions(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	actor, owner := "operator_"+uuid.NewString(), "opa_owner_tt_"+x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status,role) VALUES($1,$1,$1||'@test.invalid','active','user')`, owner); err != nil {
		t.Fatal(err)
	}
	team := "opa_team_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, owner); err != nil {
		t.Fatal(err)
	}
	s := NewService(store)
	banned, err := s.Execute(ctx, actor, "team-ban-"+x, "team_ban", team, "case:test", Input{Revision: 1})
	if err != nil || banned.State != "suspended" || banned.Version != 2 {
		t.Fatalf("team ban=%#v err=%v", banned, err)
	}
	active, err := s.Execute(ctx, actor, "team-unban-"+x, "team_unban", team, "case:test", Input{Revision: banned.Version})
	if err != nil || active.State != "active" || active.Version != 3 {
		t.Fatalf("team unban=%#v err=%v", active, err)
	}
	sum := sha256.Sum256([]byte(x))
	endpointID := testutil.EndpointID("operator-tunnel:" + x)
	tunnel := "opa_tunnel_" + x
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$3,'active','private',1,$4,$5,$6,$2)`, tunnel, owner, "tunnel-"+x, endpointID, "https://"+testutil.EndpointLabel(x)+".test.invalid", testutil.PublicationMachine(t, store.SQL(), owner)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_config_generations(tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until) VALUES($1,1,$2,'{}','pending',$3,now()+interval '1 day')`, tunnel, sum[:], owner); err != nil {
		t.Fatal(err)
	}
	disabled, err := s.Execute(ctx, actor, "tunnel-disable-"+x, "tunnel_disable", tunnel, "case:test", Input{Revision: 1})
	if err != nil || disabled.State != "paused" || disabled.Version != 2 {
		t.Fatalf("disable=%#v err=%v", disabled, err)
	}
	var configs, ops, receipts int
	if err = store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.tunnel_config_generations WHERE tunnel_id=$1),(SELECT count(*) FROM paperboat.operations WHERE resource_id=$1 AND operation_type='tunnel.paused'),(SELECT count(*) FROM paperboat.operator_actions WHERE target=$1)`, tunnel).Scan(&configs, &ops, &receipts); err != nil || configs != 2 || ops != 1 || receipts != 1 {
		t.Fatalf("disable evidence configs/ops/receipts=%d/%d/%d err=%v", configs, ops, receipts, err)
	}
	failKey := "tunnel-audit-fail-" + x
	fn := "fail_tunnel_audit_" + x
	trigger := "fail_tunnel_audit_trigger_" + x
	if _, err = store.SQL().ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION paperboat.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.idempotency_key=$key$%s$key$ THEN RAISE EXCEPTION 'forced audit failure'; END IF; RETURN NEW; END $$`, fn, failKey)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON paperboat.audit_events FOR EACH ROW EXECUTE FUNCTION paperboat.%s()`, trigger, fn)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON paperboat.audit_events`, trigger))
		store.SQL().ExecContext(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS paperboat.%s()`, fn))
	})
	if _, err = s.Execute(ctx, actor, failKey, "tunnel_enable", tunnel, "case:test", Input{Revision: disabled.Version}); err == nil {
		t.Fatal("forced tunnel audit failure succeeded")
	}
	var tunnelState string
	var tunnelGeneration int64
	if err = store.SQL().QueryRowContext(ctx, `SELECT desired_state,generation FROM paperboat.tunnels WHERE id=$1`, tunnel).Scan(&tunnelState, &tunnelGeneration); err != nil || tunnelState != "paused" || tunnelGeneration != disabled.Version {
		t.Fatalf("tunnel audit rollback=%s/%d err=%v", tunnelState, tunnelGeneration, err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.tunnel_config_generations WHERE tunnel_id=$1`, tunnel).Scan(&configs); err != nil || configs != 2 {
		t.Fatalf("rollback configs=%d err=%v", configs, err)
	}
	enabled, err := s.Execute(ctx, actor, "tunnel-enable-"+x, "tunnel_enable", tunnel, "case:test", Input{Revision: disabled.Version})
	if err != nil || enabled.State != "active" || enabled.Version != 3 {
		t.Fatalf("enable=%#v err=%v", enabled, err)
	}
}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return store
}
