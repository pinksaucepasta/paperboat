package operator

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/testutil"
)

func TestReadExpectedErrorsRequireDirectNoRows(t *testing.T) {
	for _, noRows := range []error{pgx.ErrNoRows, sql.ErrNoRows} {
		if got := operatorReadAuthorizationError(noRows); got != ErrForbidden {
			t.Errorf("authorization no-row = %v, want forbidden", got)
		}
		if got := scanErr(noRows); got != ErrNotFound {
			t.Errorf("resource no-row = %v, want not found", got)
		}
	}

	joined := errors.Join(pgx.ErrNoRows, syscall.EIO)
	if got := operatorReadAuthorizationError(joined); got == ErrForbidden || !errors.Is(got, pgx.ErrNoRows) || !errors.Is(got, syscall.EIO) {
		t.Fatalf("mixed authorization read failure was normalized: %v", got)
	}
	if got := scanErr(joined); got == ErrNotFound || !errors.Is(got, pgx.ErrNoRows) || !errors.Is(got, syscall.EIO) {
		t.Fatalf("mixed resource read failure was normalized: %v", got)
	}
}

func TestOperatorReadFailureHasStaticTextAndUnwrapsCause(t *testing.T) {
	private := errors.New("OPERATOR_PRIVATE_QUERY_DETAIL")
	err := &operatorReadFailure{cause: private}
	if err.Error() != "operator read failed" || strings.Contains(err.Error(), private.Error()) {
		t.Fatalf("read failure exposed cause text: %q", err.Error())
	}
	if !errors.Is(err, private) {
		t.Fatal("read failure dropped the original cause")
	}
}

func TestRoleMatrixAndIdentifiers(t *testing.T) {
	tests := []struct {
		role auth.Role
		kind string
		want bool
	}{
		{auth.Role("support"), "account", true},
		{auth.Role("support"), "infrastructure", false},
		{auth.Role("billing"), "billing_operation", true},
		{auth.Role("billing"), "control_operation", false},
		{auth.Role("operations"), "infrastructure", true},
		{auth.Role("operations"), "billing", false},
		{auth.Role("admin"), "audit", true},
		{auth.Role("admin"), "overview", true},
		{auth.Role("support"), "accounts", true},
		{auth.Role("support"), "control_operations", false},
		{auth.Role("billing"), "workspaces", true},
		{auth.Role("operations"), "control_operations", true},
		{auth.Role("operations"), "tunnel_status", true},
		{auth.Role("support"), "tunnel_status", false},
		{auth.Role("user"), "account", false},
		{auth.Role("system_worker"), "infrastructure", false},
	}
	for _, test := range tests {
		if got := allowed(test.role, test.kind); got != test.want {
			t.Errorf("allowed(%q,%q)=%v want %v", test.role, test.kind, got, test.want)
		}
	}
	for _, value := range []string{"usr_123", "team:abc", "resource/id", "aud.123"} {
		if !safeID(value) {
			t.Errorf("safeID(%q)=false", value)
		}
	}
	for _, value := range []string{"", "contains space", "x?y", "x\ny"} {
		if safeID(value) {
			t.Errorf("safeID(%q)=true", value)
		}
	}
}

func TestOperatorDTOsDoNotExposeSensitiveFields(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(Billing{}), reflect.TypeOf(BillingOperation{}),
		reflect.TypeOf(ControlOperation{}), reflect.TypeOf(Infrastructure{}),
		reflect.TypeOf(AuditEvent{}), reflect.TypeOf(TunnelStatus{}), reflect.TypeOf(ObservationLayer{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.ToLower(typ.Field(i).Name)
			for _, forbidden := range []string{"providerid", "cipher", "secret", "token", "requesthash", "result", "error", "metadata", "endpoint", "connection", "config"} {
				if strings.Contains(name, forbidden) {
					t.Fatalf("%s exposes sensitive field %s", typ, typ.Field(i).Name)
				}
			}
		}
	}
}

func TestInfrastructureHealthNeverReportsStaleAsReady(t *testing.T) {
	stale := canonicalInfrastructure("host", Infrastructure{State: "online", Ready: true, Stale: true})
	if stale.Ready || stale.ObservedStatus != "stale" {
		t.Fatalf("stale health=%+v", stale)
	}
	tunnel := canonicalInfrastructure("tunnel", Infrastructure{State: "active", Ready: true, Stale: false})
	if tunnel.Ready || !tunnel.Stale || tunnel.ObservedAt != nil || tunnel.ObservedStatus != "unknown" || tunnel.DesiredState != "active" || tunnel.State != "" {
		t.Fatalf("tunnel health=%+v", tunnel)
	}
}

func TestReadUsesCurrentDatabaseRoleAndAuditsAvailability(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
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
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	actorID, accountID := "usr_operator_"+suffix, "usr_account_"+suffix
	if _, err := store.Pool().Exec(ctx, `INSERT INTO operator_accounts(id,email,password_hash,status,role) VALUES($1,$2,'unused test login','active','admin')`, actorID, actorID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `INSERT INTO users(id,workos_subject,primary_email,display_name,status,role) VALUES($1,$1,$2,'Customer','active','user')`, accountID, accountID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Pool().Exec(context.Background(), `DELETE FROM audit_events WHERE actor_user_id=$1 OR resource_id=$2`, actorID, accountID)
		store.Pool().Exec(context.Background(), `DELETE FROM operator_accounts WHERE id=$1`, actorID)
		store.Pool().Exec(context.Background(), `DELETE FROM users WHERE id=$1`, accountID)
	})
	service := NewService(store)
	actor := auth.User{ID: actorID, Role: auth.Role("admin")}
	tunnelID, otherTunnelID, connectorID := "tun_operator_"+suffix, "tun_other_"+suffix, "con_operator_"+suffix
	endpointID, otherEndpointID := testutil.EndpointID("operator-read:"+suffix), testutil.EndpointID("operator-other:"+suffix)
	for _, statement := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code) VALUES($1,$2,$3,'active','private',7,$4,$5,'host-test',$2,'degraded'),($6,$2,$7,'active','private',3,$8,$9,'host-other',$2,'ready')`, []any{tunnelID, accountID, "operator-" + suffix, endpointID, "https://operator-" + suffix + ".tunnels.test", otherTunnelID, "other-" + suffix, otherEndpointID, "https://other-" + suffix + ".tunnels.test"}},
		{`INSERT INTO tunnel_connectors(id,tunnel_id,host_id,credential_reference,credential_thumbprint,protocol_version,last_heartbeat_at) VALUES($1,$2,'host-test',$3,$4,'1.0',now())`, []any{connectorID, tunnelID, "secret-ref-" + suffix, "thumbprint-" + suffix}},
		{`INSERT INTO tunnel_connector_sessions(id,connector_id,process_generation,protocol_version,state,lease_deadline,last_heartbeat_at,ready_at,retained_until) VALUES($1,$2,1,'1.0','ready',now()+interval '5 minutes',now(),now(),now()+interval '1 hour')`, []any{"session_operator_" + suffix, connectorID}},
	} {
		if _, err := store.Pool().Exec(ctx, statement.q, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		store.Pool().Exec(context.Background(), `DELETE FROM tunnels WHERE id IN($1,$2)`, tunnelID, otherTunnelID)
	})
	teamID, workspaceID, billingOpID, controlOpID := "zzzz_operator_"+suffix, "team:operator_"+suffix, "billop_operator_"+suffix, "ctrlop_operator_"+suffix
	fixtureSQL := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, []any{teamID, accountID}},
		{`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, []any{teamID, accountID}},
		{`INSERT INTO billing_workspaces(id,team_id,state,quantity) VALUES($1,$2,'past_due',1)`, []any{workspaceID, teamID}},
		{`INSERT INTO billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state) VALUES($1,$2,$3,'seats','safe-hash','uncertain')`, []any{billingOpID, workspaceID, accountID}},
		{`INSERT INTO control_operations(id,operation_key,operation_type,request_hash,state) VALUES($1,$1,'reconcile',decode(repeat('00',32),'hex'),'dead_letter')`, []any{controlOpID}},
	}
	for _, statement := range fixtureSQL {
		if _, err := store.Pool().Exec(ctx, statement.q, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, statement := range []struct {
			q   string
			arg any
		}{{`DELETE FROM control_operations WHERE id=$1`, controlOpID}, {`DELETE FROM billing_workspace_operations WHERE idempotency_key=$1`, billingOpID}, {`DELETE FROM billing_workspaces WHERE id=$1`, workspaceID}, {`DELETE FROM team_members WHERE team_id=$1`, teamID}, {`DELETE FROM teams WHERE team_id=$1`, teamID}} {
			store.Pool().Exec(context.Background(), statement.q, statement.arg)
		}
	})
	value, err := service.Read(ctx, actor, "account", accountID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	account, ok := value.(Account)
	if !ok || account.ID != accountID || account.PrimaryEmail != accountID+"@example.test" {
		t.Fatalf("account=%#v", value)
	}
	var available int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND event_type='operator.read' AND resource_type='account' AND resource_id=$2 AND metadata->>'outcome'='available'`, actorID, accountID).Scan(&available); err != nil || available != 1 {
		t.Fatalf("available audit count=%d err=%v", available, err)
	}
	missing := "usr_missing_" + suffix
	if _, err := service.Read(ctx, actor, "account", missing, "", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err=%v", err)
	}
	var unavailable int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND resource_id=$2 AND metadata->>'outcome'='unavailable'`, actorID, missing).Scan(&unavailable); err != nil || unavailable != 1 {
		t.Fatalf("unavailable audit count=%d err=%v", unavailable, err)
	}
	if value, err = service.Read(ctx, actor, "teams", "", "zzzy", 1); err != nil || len(value.(TeamPage).Teams) != 1 || value.(TeamPage).Teams[0].ID != teamID {
		t.Fatalf("teams=%#v err=%v", value, err)
	}
	if value, err = service.Read(ctx, actor, "workspaces", "past_due", "team:operator_", 1); err != nil || len(value.(BillingPage).Workspaces) != 1 || value.(BillingPage).Workspaces[0].ID != workspaceID {
		t.Fatalf("workspaces=%#v err=%v", value, err)
	}
	if value, err = service.Read(ctx, actor, "billing_operations", "uncertain", "billop_operator_", 1); err != nil || len(value.(BillingOperationPage).Operations) != 1 || value.(BillingOperationPage).Operations[0].ID != billingOpID {
		t.Fatalf("billing operations=%#v err=%v", value, err)
	}
	if value, err = service.Read(ctx, actor, "control_operations", "dead_letter", "ctrlop_operator_", 1); err != nil || len(value.(ControlOperationPage).Operations) != 1 || value.(ControlOperationPage).Operations[0].ID != controlOpID {
		t.Fatalf("control operations=%#v err=%v", value, err)
	}
	value, err = service.Read(ctx, actor, "tunnel_status", tunnelID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	status := value.(TunnelStatus)
	if status.ID != tunnelID || status.Generation != 7 || status.SummaryCode != "degraded" || len(status.ConnectorSessions.States) != 1 || status.ConnectorSessions.States[0].State != "ready" || status.ConnectorSessions.States[0].Count != 1 || status.ConnectorSessions.LatestObservedAt == nil {
		t.Fatalf("tunnel status=%+v", status)
	}
	if raw, err := json.Marshal(status); err != nil || bytes.Contains(raw, []byte("secret-ref")) || bytes.Contains(raw, []byte("thumbprint")) || bytes.Contains(raw, []byte("host-test")) {
		t.Fatalf("unsafe tunnel status=%s err=%v", raw, err)
	}
	functionName, triggerName := "operator_audit_fail_"+suffix, "operator_audit_trigger_"+suffix
	if _, err := store.Pool().Exec(ctx, fmt.Sprintf(`CREATE FUNCTION paperboat.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='operator.read' AND NEW.resource_id='%s' THEN RAISE EXCEPTION 'synthetic operator audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER %s BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION paperboat.%s()`, functionName, controlOpID, triggerName, functionName)); err != nil {
		t.Fatal(err)
	}
	defer store.Pool().Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON paperboat.audit_events; DROP FUNCTION IF EXISTS paperboat.%s()`, triggerName, functionName))
	if _, err := service.Read(ctx, actor, "control_operation", controlOpID, "", 1); err == nil {
		t.Fatal("audit failure was hidden")
	} else {
		var postgresErr *pgconn.PgError
		if !errors.As(err, &postgresErr) || postgresErr.Code != "P0001" {
			t.Fatalf("audit failure lost its PostgreSQL cause: %T", err)
		}
		if strings.Contains(err.Error(), "synthetic operator audit failure") {
			t.Fatal("operator read error exposed raw PostgreSQL text")
		}
	}
	var failedAuditRows int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND resource_type='control_operation' AND resource_id=$2`, actorID, controlOpID).Scan(&failedAuditRows); err != nil || failedAuditRows != 0 {
		t.Fatalf("failed read audit rows=%d err=%v", failedAuditRows, err)
	}
	if _, err := store.Pool().Exec(ctx, fmt.Sprintf(`DROP TRIGGER %s ON paperboat.audit_events; DROP FUNCTION paperboat.%s()`, triggerName, functionName)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `UPDATE operator_accounts SET status='disabled' WHERE id=$1`, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Read(ctx, actor, "account", accountID, "", 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked role err=%v", err)
	}
	if _, err := service.Read(ctx, auth.User{ID: accountID, Role: auth.Role("admin")}, "account", accountID, "", 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("forged role err=%v", err)
	}
}
