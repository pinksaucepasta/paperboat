package planlimits

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// An operator override replaces the plan's limit in the one place the limit is computed: admission (CheckTx) and the usage
// the customer sees (UsageTx) both follow it; it ends at ends_at, on revoke, or when a newer override of the kind replaces it;
// it can lower a limit as well as raise one; a team's hidden ceiling is replaced the same way.
func TestOperatorOverridesReplaceThePlanLimitPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	limits := config.Default().Billing.Limits
	limits.Machines = config.PlanLimit{Free: 2, Pro: 5, TeamCeiling: 10}
	Configure(limits, "")
	t.Cleanup(func() { Configure(config.Default().Billing.Limits, "") })
	x := fmt.Sprint(time.Now().UnixNano())
	user, team, operator := "ov_u_"+x, "ov-t-"+x, "operator_ov_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{`DELETE FROM paperboat.billing_workspace_grants WHERE workspace_id IN ($1,$2)`} {
			store.SQL().ExecContext(bg, q, "personal:"+user, "team:"+team)
		}
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.user_machines WHERE user_id=$1`, user)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	batch := 0
	machines := func(n int, team string) {
		t.Helper()
		batch++
		col, val := "", ""
		if team != "" {
			col, val = ",owner_team_id", ",'"+team+"'"
		}
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state`+col+`)
 SELECT $1||i,$2,$1||i,'a'||substr(md5($1||i::text),1,12),'linux','amd64','/w','online','occupied'`+val+` FROM generate_series(1,$3::int) i`, fmt.Sprintf("ov_m_%s_%d_", x, batch), user, n)
	}
	grant := func(workspace string, kind Kind, amount int64, endsAt *time.Time) Grant {
		t.Helper()
		var g Grant
		err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			g, err = GrantTx(ctx, tx, operator, workspace, kind, amount, endsAt, "case:test")
			return err
		})
		if err != nil {
			t.Fatalf("grant %s %d: %v", kind, amount, err)
		}
		return g
	}
	usage := func(scope Scope) Usage {
		t.Helper()
		var u map[Kind]Usage
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) { u, err = UsageTx(ctx, tx, scope); return }); err != nil {
			t.Fatal(err)
		}
		return u[Machines]
	}
	personal, personalID := Scope{Account: user}, "personal:"+user
	machines(2, "")
	refused(t, check(t, store, personal, Machines), Free, 2)

	// Raised: admitted up to the override, and the customer's usage shows it as granted.
	g := grant(personalID, Machines, 4, nil)
	if err := check(t, store, personal, Machines); err != nil {
		t.Fatalf("an override of 4 refused the third machine: %v", err)
	}
	if u := usage(personal); u.Limit == nil || *u.Limit != 4 || !u.Granted || u.Used != 2 {
		t.Fatalf("usage under an override: %+v", u)
	}
	machines(2, "")
	var limit *LimitError
	if err := check(t, store, personal, Machines); !errors.As(err, &limit) || limit.Limit != 4 {
		t.Fatalf("the override's own limit must bind: %v", err)
	}

	// A newer override of the kind replaces it, and may lower it below the plan: 1 with 4 machines refuses.
	lower := grant(personalID, Machines, 1, nil)
	if err := check(t, store, personal, Machines); !errors.As(err, &limit) || limit.Limit != 1 {
		t.Fatalf("a lowered override must bind: %v", err)
	}
	var running int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.billing_workspace_grants WHERE workspace_id=$1 AND kind='machines' AND revoked_at IS NULL`, personalID).Scan(&running); err != nil || running != 1 {
		t.Fatalf("running overrides of one kind: %d %v", running, err)
	}
	// Revoking the replaced one is not found; revoking the running one returns the plan's value.
	err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := RevokeTx(ctx, tx, operator, personalID, g.ID)
		return err
	})
	if !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("revoking a replaced grant: %v", err)
	}
	if err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := RevokeTx(ctx, tx, operator, personalID, lower.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	refused(t, check(t, store, personal, Machines), Free, 2)
	if u := usage(personal); u.Granted || u.Limit == nil || *u.Limit != 2 {
		t.Fatalf("usage after a revoke: %+v", u)
	}

	// An override ends by itself.
	soon := time.Now().Add(time.Hour)
	grant(personalID, Machines, 9, &soon)
	if err := check(t, store, personal, Machines); err != nil {
		t.Fatalf("a running override refused: %v", err)
	}
	exec(`UPDATE paperboat.billing_workspace_grants SET ends_at=now()-interval '1 second' WHERE workspace_id=$1 AND kind='machines' AND revoked_at IS NULL`, personalID)
	refused(t, check(t, store, personal, Machines), Free, 2)
	if u := usage(personal); u.Granted {
		t.Fatalf("an ended override still reads as granted: %+v", u)
	}

	// A team's hidden ceiling (10) is replaced too, and its usage turns from unlimited to the override.
	teamScope := Scope{Team: team}
	machines(3, team)
	if u := usage(teamScope); u.Limit != nil || u.Granted {
		t.Fatalf("a team without an override is advertised unlimited: %+v", u)
	}
	grant("team:"+team, Machines, 3, nil)
	refused(t, check(t, store, teamScope, Machines), Team, 3)
	if u := usage(teamScope); u.Limit == nil || *u.Limit != 3 || !u.Granted {
		t.Fatalf("a team override reads: %+v", u)
	}
	if got, err := TeamCeilingFor(ctx, store.Pool(), team, Machines); err != nil || got != 3 {
		t.Fatalf("the team binding ceiling: %d %v", got, err)
	}

	// Other limits and the viewer cap follow their overrides; refusals of bad grants.
	if got, err := ViewersPerSharedTerminalFor(ctx, store.Pool(), personal); err != nil || got != limits.ViewersPerSharedTerminal {
		t.Fatalf("viewers without an override: %d %v", got, err)
	}
	grant(personalID, ViewersPerTerminal, 7, nil)
	if got, err := ViewersPerSharedTerminalFor(ctx, store.Pool(), personal); err != nil || got != 7 {
		t.Fatalf("viewers with an override: %d %v", got, err)
	}
	bad := func(workspace string, kind Kind, amount int64, ends *time.Time, want error) {
		t.Helper()
		err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			_, err := GrantTx(ctx, tx, operator, workspace, kind, amount, ends, "case:test")
			return err
		})
		if !errors.Is(err, want) {
			t.Fatalf("%s %s %d: %v, want %v", workspace, kind, amount, err, want)
		}
	}
	past := time.Now().Add(-time.Minute)
	bad(personalID, Seats, 5, nil, ErrGrant)            // seats are a team's
	bad(personalID, Machines, 0, nil, ErrGrant)         // a limit of zero is not a limit
	bad(personalID, Kind("nonsense"), 5, nil, ErrGrant) // unknown kind
	bad(personalID, Machines, 5, &past, ErrGrant)       // ends already
	bad(personalID, Machines, -1, nil, ErrGrant)        // negative
	bad("workspace_x", Machines, 5, nil, ErrGrant)      // not a workspace id
	bad("personal:nobody_"+x, Machines, 5, nil, ErrGrantNotFound)
	bad("team:nobody_"+x, Machines, 5, nil, ErrGrantNotFound)
}

// Bandwidth grants add up, and the SQL function the readers of the allowance share counts only the running ones.
func TestGrantedBandwidthAddsUpAndEndsPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, operator := "ob_u_"+x, "operator_ob_"+x
	store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_grants WHERE workspace_id=$1`, "personal:"+user)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	grant := func(amount int64, ends *time.Time) Grant {
		t.Helper()
		var g Grant
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			g, err = GrantTx(ctx, tx, operator, "personal:"+user, Bandwidth, amount, ends, "case:test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return g
	}
	granted := func() int64 {
		t.Helper()
		var n int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT workspace_granted_bytes($1)`, "personal:"+user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if granted() != 0 {
		t.Fatal("bytes granted with no grant")
	}
	a := grant(10_000_000_000, nil)
	soon := time.Now().Add(time.Hour)
	grant(5_000_000_000, &soon)
	if granted() != 15_000_000_000 {
		t.Fatalf("two grants add up: %d", granted())
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := RevokeTx(ctx, tx, operator, "personal:"+user, a.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if granted() != 5_000_000_000 {
		t.Fatalf("a revoked grant still counts: %d", granted())
	}
	store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_grants SET ends_at=now()-interval '1 second' WHERE workspace_id=$1 AND revoked_at IS NULL`, "personal:"+user)
	if granted() != 0 {
		t.Fatalf("an ended grant still counts: %d", granted())
	}
	var list []Grant
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
		list, err = GrantsTx(ctx, tx, "personal:"+user)
		return
	}); err != nil || len(list) != 2 || list[0].Running || list[1].Running {
		t.Fatalf("the list keeps what was granted: %+v %v", list, err)
	}
}

// Two operators overriding the same limit at the same moment leave one running override, not two.
func TestConcurrentOverridesLeaveOneRunningPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user := "ovc_u_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_grants WHERE workspace_id=$1`, "personal:"+user)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	errs := make(chan error, 8)
	for i := 1; i <= 8; i++ {
		go func() {
			errs <- store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				_, err := GrantTx(ctx, tx, "operator_c", "personal:"+user, Machines, int64(100+i), nil, "case:test")
				return err
			})
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var running int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.billing_workspace_grants WHERE workspace_id=$1 AND kind='machines' AND revoked_at IS NULL`, "personal:"+user).Scan(&running); err != nil || running != 1 {
		t.Fatalf("running overrides: %d %v", running, err)
	}
}

// Every limit kind follows its override (usage and the team ceiling), an ended grant cannot be revoked, and a grant needs a live
// account or team and a sane amount.
func TestOverridesOfEveryKindAndRefusedGrantsPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, team, operator := "ok_u_"+x, "ok-t-"+x, "operator_ok_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_grants WHERE workspace_id IN ($1,$2)`, "personal:"+user, "team:"+team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	grant := func(workspace string, kind Kind, amount int64, ends *time.Time) (Grant, error) {
		var g Grant
		err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			g, err = GrantTx(ctx, tx, operator, workspace, kind, amount, ends, "case:test")
			return err
		})
		return g, err
	}
	personal := "personal:" + user
	for i, kind := range []Kind{Machines, Publications, BrowserTerminals, PublicTCP} {
		value := int64(40 + i)
		if _, err := grant(personal, kind, value, nil); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		var usage map[Kind]Usage
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			usage, err = UsageTx(ctx, tx, Scope{Account: user})
			return
		}); err != nil {
			t.Fatal(err)
		}
		if u := usage[kind]; u.Limit == nil || *u.Limit != int(value) || !u.Granted {
			t.Fatalf("%s usage under its override: %+v", kind, u)
		}
	}
	for _, kind := range []Kind{Machines, Publications, BrowserTerminals} {
		if _, err := grant("team:"+team, kind, 3, nil); err != nil {
			t.Fatal(err)
		}
		if got, err := TeamCeilingFor(ctx, store.Pool(), team, kind); err != nil || got != 3 {
			t.Fatalf("%s team ceiling: %d %v", kind, got, err)
		}
	}

	// An override that ended by itself is not running, so it cannot be revoked.
	soon := time.Now().Add(time.Hour)
	g, err := grant(personal, Machines, 9, &soon)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE paperboat.billing_workspace_grants SET ends_at=now()-interval '1 second' WHERE id=$1`, g.ID)
	if err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := RevokeTx(ctx, tx, operator, personal, g.ID)
		return err
	}); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("revoking an ended grant: %v", err)
	}

	// Nothing is granted to an account that is not active or a team that is suspended or deleted, or beyond the largest amount.
	exec(`UPDATE paperboat.users SET status='suspended' WHERE id=$1`, user)
	if _, err = grant(personal, Machines, 5, nil); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("a suspended account: %v", err)
	}
	exec(`UPDATE paperboat.users SET status='active' WHERE id=$1`, user)
	exec(`UPDATE paperboat.teams SET suspended=true,suspended_at=now(),suspended_by=$2 WHERE team_id=$1`, team, user)
	if _, err = grant("team:"+team, Machines, 5, nil); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("a suspended team: %v", err)
	}
	exec(`UPDATE paperboat.teams SET suspended=false,suspended_at=NULL,suspended_by=NULL,deleted_at=now() WHERE team_id=$1`, team)
	if _, err = grant("team:"+team, Machines, 5, nil); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("a deleted team: %v", err)
	}
	if _, err = grant(personal, Bandwidth, MaxGrantAmount+1, nil); !errors.Is(err, ErrGrant) {
		t.Fatalf("an amount beyond the largest: %v", err)
	}
}
