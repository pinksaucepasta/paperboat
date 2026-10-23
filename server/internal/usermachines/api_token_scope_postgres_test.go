package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

// A request made with a scoped API token is held to the scope at the one machine authorization
// of terminal, exec, managed SSH and files (PLAN F18-06); an unscoped token acts as its creator.
func TestMachineAuthorizationHoldsAPITokensToTheirScope(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := fmt.Sprint(time.Now().UnixNano())
	user, ci, laptop := "ts_user_"+s, "ts_ci_"+s, "ts_laptop_"+s
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	for _, m := range []string{ci, laptop} {
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES($1,$2,$1,'linux','amd64','/w','online','occupied',true)`, m, user)
	}
	exec(`INSERT INTO paperboat.workspace_policies(owner_account,version,source,digest) VALUES($1,1,'{}','test')`, user)
	exec(`INSERT INTO paperboat.workspace_policy_tags(policy_id,tag,machine_id) SELECT id,'tag:ci',$2 FROM paperboat.workspace_policies WHERE owner_account=$1`, user, ci)
	token := func(id string, scoped bool) {
		scope := sql.NullString{String: `[{"dst":["tag:ci"],"allow":["exec"]}]`, Valid: scoped}
		exec(`INSERT INTO paperboat.api_tokens(id,account_id,name,secret_hash,scope,expires_at) VALUES($1,$2,$1,sha256(convert_to($1::text,'UTF8')),$3::jsonb,now()+interval '1 day')`, id, user, scope)
	}
	token("tok_scoped_"+s, true)
	token("tok_free_"+s, false)
	exec(`INSERT INTO paperboat.api_token_scope_rules(token_id,entry,target_kind,target_id,action,ports) VALUES($1,0,'tag','tag:ci','exec',int4range(1,65536))`, "tok_scoped_"+s)
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		for _, q := range []string{`DELETE FROM paperboat.api_tokens WHERE account_id=$1`, `DELETE FROM paperboat.workspace_policies WHERE owner_account=$1`, `DELETE FROM paperboat.user_machines WHERE user_id=$1`, `DELETE FROM paperboat.users WHERE id=$1`} {
			_, _ = store.SQL().ExecContext(c, q, user)
		}
	})
	check := func(tok, machine, capability string, want error) {
		t.Helper()
		c := ctx
		if tok != "" {
			c = auth.WithAPIToken(ctx, tok)
		}
		_, err := getMachineForCapability(c, store.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: machine, UserID: user, Capability: capability})
		if !errors.Is(err, want) && !(want == nil && err == nil) {
			t.Errorf("token=%q %s %s: %v, want %v", tok, machine, capability, err, want)
		}
	}
	check("", laptop, "terminal", nil)
	check("tok_scoped_"+s, ci, "exec", nil)
	check("tok_scoped_"+s, ci, "terminal", auth.ErrOutsideTokenScope)
	check("tok_scoped_"+s, laptop, "exec", auth.ErrOutsideTokenScope)
	check("tok_free_"+s, laptop, "terminal", nil)
	// Moving the tag moves the scope; revoking the token covers nothing.
	exec(`UPDATE paperboat.workspace_policy_tags SET machine_id=$2 WHERE machine_id=$1`, ci, laptop)
	check("tok_scoped_"+s, laptop, "exec", nil)
	check("tok_scoped_"+s, ci, "exec", auth.ErrOutsideTokenScope)
	exec(`UPDATE paperboat.api_tokens SET revoked_at=now(),revoked_by=account_id WHERE id=$1`, "tok_free_"+s)
	check("tok_free_"+s, laptop, "terminal", auth.ErrOutsideTokenScope)
}
