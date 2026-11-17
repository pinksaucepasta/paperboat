package operatoractions

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func staffOperator(t *testing.T, store *db.DB, role string) string {
	t.Helper()
	id := "operator_" + uuid.NewString()
	if _, err := store.SQL().ExecContext(context.Background(), `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active',$2)`, id, role); err != nil {
		t.Fatal(err)
	}
	return id
}

func staffSession(t *testing.T, store *db.DB, operator string) string {
	t.Helper()
	token := uuid.NewString()
	if _, err := store.SQL().ExecContext(context.Background(), `INSERT INTO paperboat.operator_sessions(token_hash,operator_id,csrf_hash,expires_at) VALUES(sha256($1::bytea),$2,sha256('csrf'::bytea),now()+interval '1 hour')`, token, operator); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(mustSHA(t, store, token))
}

func mustSHA(t *testing.T, store *db.DB, token string) []byte {
	t.Helper()
	var sum []byte
	if err := store.SQL().QueryRowContext(context.Background(), `SELECT sha256($1::bytea)`, token).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

func liveSessions(t *testing.T, store *db.DB, operator string) int {
	t.Helper()
	var n int
	if err := store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FROM paperboat.operator_sessions WHERE operator_id=$1 AND revoked_at IS NULL AND expires_at>now()`, operator).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Operator management through Execute: invitation (token shown once, never in the receipt), disable (ends
// sessions, refuses self and the last active admin), enable, MFA reset (deletes the factor and ends sessions),
// session revoke (one or all, never your own), admin-only, idempotent replay.
func TestOperatorManagementThroughReceipts(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	s := NewService(store)
	x := fmt.Sprint(time.Now().UnixNano())
	admin, other := staffOperator(t, store, "admin"), staffOperator(t, store, "admin")
	support := staffOperator(t, store, "support")
	// Every other admin the shared test database holds must not count as a spare.
	rows, err := store.SQL().QueryContext(ctx, `UPDATE paperboat.operator_accounts SET status='disabled' WHERE role='admin' AND status='active' AND id NOT IN ($1,$2) RETURNING id`, admin, other)
	if err != nil {
		t.Fatal(err)
	}
	var disabledHere []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		disabledHere = append(disabledHere, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `UPDATE paperboat.operator_accounts SET status='active' WHERE id=ANY($1)`, disabledHere)
	})
	email := "new-" + x + "@example.test"
	run := func(actor, key, kind, target string, in Input) (Result, error) {
		return s.Execute(ctx, actor, key, kind, target, "case:test", in)
	}

	if _, err := run(support, "create-support-"+x, "operator_create", "operators", Input{Email: email, Role: "support"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin created an operator: %v", err)
	}
	if _, err := run(admin, "create-rev-"+x, "operator_create", "operators", Input{Revision: 1, Email: email, Role: "support"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("operator actions take no revision: %v", err)
	}
	for _, bad := range []Input{{Email: "not-an-email", Role: "support"}, {Email: email, Role: "root"}, {Email: " a@b@c.test", Role: "admin"}} {
		if _, err := run(admin, "create-bad-"+x, "operator_create", "operators", bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("input %+v: %v", bad, err)
		}
	}
	created, err := run(admin, "create-"+x, "operator_create", "operators", Input{Email: email, Role: "support"})
	if err != nil || created.State != "invited" || created.SetupToken == "" || created.ExpiresAt == nil || created.ResourceID == "" {
		t.Fatalf("create: %+v %v", created, err)
	}
	var stored string
	if err = store.SQL().QueryRowContext(ctx, `SELECT result::text FROM paperboat.operator_actions WHERE actor_user_id=$1 AND idempotency_key=$2`, admin, "create-"+x).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := created.SetupToken; want != "" && strings.Contains(stored, want) {
		t.Fatal("the receipt stores the setup token")
	}
	replay, err := run(admin, "create-"+x, "operator_create", "operators", Input{Email: email, Role: "support"})
	if err != nil || replay.ResourceID != created.ResourceID || replay.SetupToken != "" {
		t.Fatalf("replay must return the same operator without the token: %+v %v", replay, err)
	}
	if _, err = run(admin, "create-"+x, "operator_create", "operators", Input{Email: email, Role: "billing"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key, different request: %v", err)
	}
	// Inviting again before setup finishes replaces the token (a lost token is recovered this way).
	again, err := run(admin, "create-again-"+x, "operator_create", "operators", Input{Email: email, Role: "billing"})
	if err != nil || again.ResourceID != created.ResourceID || again.SetupToken == created.SetupToken {
		t.Fatalf("re-invite: %+v %v", again, err)
	}
	var tokens int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.operator_invitations WHERE operator_id=$1`, created.ResourceID).Scan(&tokens); err != nil || tokens != 1 {
		t.Fatalf("invitations=%d %v", tokens, err)
	}
	// An operator who already has a password cannot be invited over.
	if _, err = run(admin, "create-exists-"+x, "operator_create", "operators", Input{Email: support + "@test.invalid", Role: "support"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("invite over an existing operator: %v", err)
	}

	// Disable: ends sessions, refuses self and the last active admin, enable brings back sign-in only.
	one, two := staffSession(t, store, support), staffSession(t, store, support)
	staffSession(t, store, admin)
	if _, err = run(admin, "self-disable-"+x, "operator_disable", admin, Input{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("self disable: %v", err)
	}
	if _, err = run(admin, "rev-one-"+x, "operator_sessions_revoke", support, Input{SessionID: one}); err != nil {
		t.Fatal(err)
	}
	if n := liveSessions(t, store, support); n != 1 {
		t.Fatalf("revoking one session left %d live", n)
	}
	if _, err = run(admin, "rev-none-"+x, "operator_sessions_revoke", support, Input{SessionID: one}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking an ended session: %v", err)
	}
	if _, err = run(admin, "rev-bad-"+x, "operator_sessions_revoke", support, Input{SessionID: "zz"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad session id: %v", err)
	}
	if _, err = run(admin, "rev-self-"+x, "operator_sessions_revoke", admin, Input{SessionID: "all"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoking your own sessions: %v", err)
	}
	_ = two
	disabled, err := run(admin, "disable-"+x, "operator_disable", support, Input{})
	if err != nil || disabled.State != "disabled" || liveSessions(t, store, support) != 0 {
		t.Fatalf("disable: %+v live=%d %v", disabled, liveSessions(t, store, support), err)
	}
	enabled, err := run(admin, "enable-"+x, "operator_enable", support, Input{})
	if err != nil || enabled.State != "active" || liveSessions(t, store, support) != 0 {
		t.Fatalf("enable: %+v live=%d %v", enabled, liveSessions(t, store, support), err)
	}
	// Admins can disable each other while another stays active; a disabled operator can no longer act.
	if _, err = run(admin, "disable-other-"+x, "operator_disable", other, Input{}); err != nil {
		t.Fatalf("disabling a spare admin: %v", err)
	}
	if _, err = run(other, "disabled-actor-"+x, "operator_disable", admin, Input{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a disabled operator acted: %v", err)
	}
	if _, err = run(admin, "missing-"+x, "operator_disable", "operator_missing", Input{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing target: %v", err)
	}

	// MFA reset deletes the factor and ends sessions; it cannot be aimed at yourself.
	victim := staffOperator(t, store, "billing")
	staffSession(t, store, victim)
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_totp(operator_id,secret_cipher,confirmed_at) VALUES($1,'\x00',now())`, victim); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_recovery_codes(operator_id,code_hash) VALUES($1,sha256('r'::bytea))`, victim); err != nil {
		t.Fatal(err)
	}
	if _, err = run(admin, "reset-self-"+x, "operator_mfa_reset", admin, Input{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("self reset: %v", err)
	}
	got, err := run(admin, "reset-"+x, "operator_mfa_reset", victim, Input{})
	if err != nil || got.State != "mfa_reset" {
		t.Fatalf("reset: %+v %v", got, err)
	}
	var factors, codes int
	if err = store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.operator_totp WHERE operator_id=$1),(SELECT count(*) FROM paperboat.operator_recovery_codes WHERE operator_id=$1)`, victim).Scan(&factors, &codes); err != nil || factors != 0 || codes != 0 || liveSessions(t, store, victim) != 0 {
		t.Fatalf("after reset: totp=%d codes=%d live=%d %v", factors, codes, liveSessions(t, store, victim), err)
	}
	var events int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='operator.operator_mfa_reset' AND resource_id=$1 AND resource_type='operator_account'`, victim).Scan(&events); err != nil || events != 1 {
		t.Fatalf("audit events=%d %v", events, err)
	}
}
