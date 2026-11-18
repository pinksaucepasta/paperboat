package operatoractions

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A ban shares the revoke path, so it ends API tokens and recipes too (it used to leave them live).
func TestAccountBanEndsApiTokensAndRecipes(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	actor, target := "operator_"+uuid.NewString(), "opa_banend_"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, actor)
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,role) VALUES($1,$1,$1||'@test.invalid','active','user')`, target)
	exec(`INSERT INTO paperboat.api_tokens(id,account_id,name,secret_hash,expires_at) VALUES($1,$2,'ci',sha256(convert_to($3,'UTF8')),now()+interval '1 day')`, "tok_"+x, target, "secret_"+x)
	exec(`INSERT INTO paperboat.recipes(id,token_id,account_id,name,source,source_format,canonical,auth_hash,expires_at) VALUES($1,$1,$2,'r','x','toml','{}'::jsonb,sha256(convert_to($3,'UTF8')),now()+interval '1 day')`, "rcp_"+x, target, "recipe_"+x)
	if _, err := NewService(store).Execute(ctx, actor, "ban-"+x, "account_ban", target, "case:test", Input{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	var token, recipe bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT (SELECT revoked_at IS NOT NULL FROM paperboat.api_tokens WHERE id=$1),(SELECT revoked_at IS NOT NULL FROM paperboat.recipes WHERE id=$2)`, "tok_"+x, "rcp_"+x).Scan(&token, &recipe); err != nil || !token || !recipe {
		t.Fatalf("api token revoked=%t recipe revoked=%t %v", token, recipe, err)
	}
}

// An operator's "sign out everywhere" ends every credential the account holds and leaves the account usable.
func TestAccountSessionsRevokeEndsEveryCredentialAndKeepsTheAccountActive(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	s := NewService(store)
	x := fmt.Sprint(time.Now().UnixNano())
	actor, target := "operator_"+uuid.NewString(), "opa_signout_"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, actor)
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,role) VALUES($1,$1,$1||'@test.invalid','active','user')`, target)
	exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,'csrf',now()+interval '1 hour')`, "sess_"+x, target, "hash_"+x)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,'cli','test','cli','linux','{}','active',now(),now())`, "cli_"+x, target)
	exec(`INSERT INTO paperboat.cli_access_tokens(token_hash,cli_client_session_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, "access_"+x, "cli_"+x)
	exec(`INSERT INTO paperboat.cli_refresh_tokens(token_hash,cli_client_session_id,state,expires_at,created_at) VALUES($1,$2,'active',now()+interval '1 hour',now())`, "refresh_"+x, "cli_"+x)
	exec(`INSERT INTO paperboat.api_tokens(id,account_id,name,secret_hash,expires_at) VALUES($1,$2,'ci',sha256(convert_to($3,'UTF8')),now()+interval '1 day')`, "tok_"+x, target, "secret_"+x)

	exec(`INSERT INTO paperboat.recipes(id,token_id,account_id,name,source,source_format,canonical,auth_hash,expires_at) VALUES($1,$1,$2,'r','x','toml','{}'::jsonb,sha256(convert_to($3,'UTF8')),now()+interval '1 day')`, "rcp_"+x, target, "recipe_"+x)
	run := func(key string, revision int64) (Result, error) {
		return s.Execute(ctx, actor, key, "account_sessions_revoke", target, "case:test", Input{Revision: revision})
	}
	if _, err := run("stale-"+x, 99); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale revision: %v", err)
	}
	got, err := run("revoke-"+x, 1)
	if err != nil || got.State != "active" || got.Version != 2 {
		t.Fatalf("revoke: %+v %v", got, err)
	}
	if replay, err := run("revoke-"+x, 1); err != nil || replay != got {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	var sessionRevoked, accessRevoked, apiRevoked, recipeRevoked bool
	var cliState, refreshState, accountStatus, reason string
	if err = store.SQL().QueryRowContext(ctx, `SELECT s.revoked_at IS NOT NULL FROM paperboat.sessions s WHERE s.id=$1`, "sess_"+x).Scan(&sessionRevoked); err != nil {
		t.Fatal(err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT c.state,r.state,a.revoked_at IS NOT NULL,coalesce(c.revocation_reason,'') FROM paperboat.cli_client_sessions c JOIN paperboat.cli_refresh_tokens r ON r.cli_client_session_id=c.id JOIN paperboat.cli_access_tokens a ON a.cli_client_session_id=c.id WHERE c.id=$1`, "cli_"+x).Scan(&cliState, &refreshState, &accessRevoked, &reason); err != nil {
		t.Fatal(err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT revoked_at IS NOT NULL FROM paperboat.api_tokens WHERE id=$1`, "tok_"+x).Scan(&apiRevoked); err != nil {
		t.Fatal(err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT revoked_at IS NOT NULL FROM paperboat.recipes WHERE id=$1`, "rcp_"+x).Scan(&recipeRevoked); err != nil {
		t.Fatal(err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT status FROM paperboat.users WHERE id=$1`, target).Scan(&accountStatus); err != nil {
		t.Fatal(err)
	}
	if !recipeRevoked || !sessionRevoked || cliState != "revoked" || refreshState != "revoked" || !accessRevoked || !apiRevoked || reason != "operator_revoked" || accountStatus != "active" {
		t.Fatalf("session=%t cli=%s refresh=%s access=%t api=%t reason=%q status=%s", sessionRevoked, cliState, refreshState, accessRevoked, apiRevoked, reason, accountStatus)
	}
	// The next one needs the new version, and an unknown account is not found.
	if _, err = run("again-"+x, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("an old version after the revoke: %v", err)
	}
	if _, err = s.Execute(ctx, actor, "missing-"+x, "account_sessions_revoke", "opa_missing_"+x, "case:test", Input{Revision: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	if _, err = s.Execute(ctx, actor, "email-"+x, "account_sessions_revoke", target, "case:test", Input{Revision: 2, Email: "x@y.test"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("operator fields on an account action: %v", err)
	}
	exec(`UPDATE paperboat.operator_accounts SET role='support' WHERE id=$1`, actor)
	if _, err = run("support-"+x, 2); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin: %v", err)
	}
}
