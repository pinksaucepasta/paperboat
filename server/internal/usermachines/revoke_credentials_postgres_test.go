package usermachines

import (
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// Revoking a machine ends every credential it holds at once: the CLI session created when it was paired, and the
// access and refresh tokens of that session, so the very next request with the old token is unauthenticated.
func TestRevokingAMachineEndsItsPairTimeCLICredentialAtOnceOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine, environment, session := "revoke_owner_"+suffix, "machine_revoke_"+suffix, "env_revoke_"+suffix, "session_revoke_"+suffix
	other := "session_other_" + suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.cli_client_sessions WHERE id IN ($1,$2)`, session, other)
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.users WHERE id=$1`, owner)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, owner, owner+"@example.test")
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'revoke','linux','amd64','/workspace','online','occupied',true,1)`, machine, owner, environment)
	for _, id := range []string{session, other} {
		bound := any(nil)
		if id == session {
			bound = machine
		}
		exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,user_machine_id) VALUES($1,$2,$1,'pair','desktop','linux',ARRAY['machines:read'],'active',now(),now(),$3)`, id, owner, bound)
		exec(`INSERT INTO paperboat.cli_access_tokens(token_hash,cli_client_session_id,expires_at,created_at) VALUES($1,$2,now()+interval '10 minutes',now())`, "access_"+id, id)
		exec(`INSERT INTO paperboat.cli_refresh_tokens(token_hash,cli_client_session_id,state,expires_at,created_at) VALUES($1,$2,'active',now()+interval '1 day',now())`, "refresh_"+id, id)
	}
	authenticated := func(token string) bool {
		_, err := store.Queries().AuthenticateClientAccessToken(ctx, dbsqlc.AuthenticateClientAccessTokenParams{TokenHashes: token, Now: time.Now().UTC()})
		return err == nil
	}
	if !authenticated("access_" + session) {
		t.Fatal("precondition: the paired machine's token authenticates")
	}
	service := New(store, nil, Policy{}, nil)
	if err := service.Disconnect(ctx, owner, machine); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if authenticated("access_" + session) {
		t.Fatal("the revoked machine's CLI access token still authenticates")
	}
	var refresh string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.cli_refresh_tokens WHERE token_hash=$1`, "refresh_"+session).Scan(&refresh); err != nil || refresh != "revoked" {
		t.Fatalf("the revoked machine's refresh token is %q (%v), want revoked", refresh, err)
	}
	var state, reason string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,coalesce(revocation_reason,'') FROM paperboat.cli_client_sessions WHERE id=$1`, session).Scan(&state, &reason); err != nil || state != "revoked" {
		t.Fatalf("the revoked machine's CLI session is %q/%q (%v), want revoked", state, reason, err)
	}
	if !authenticated("access_" + other) {
		t.Fatal("revoking one machine ended the account's other session")
	}
}
