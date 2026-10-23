package billing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// PLAN F19-03: only the owner and billing admins manage a team's billing ("Manage billing"
// opens the Stripe portal); admins, auditors and members may only read it.
func TestPostgresTeamBillingManagedByOwnerAndBillingAdmins(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, admin, payer, auditor, member := "usr_bo_"+x, "usr_ba_"+x, "usr_bb_"+x, "usr_bau_"+x, "usr_bm_"+x
	team := "biz-" + x
	for _, u := range []string{owner, admin, payer, auditor, member} {
		insertUser(t, store, u, u+"@example.test")
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Biz')`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active)
 VALUES($1,$2,1,'admin',true),($1,$3,1,'admin',true),($1,$4,1,'billing',true),($1,$5,1,'auditor',true),($1,$6,1,'member',true)`,
		team, owner, admin, payer, auditor, member)
	access := func(account string, manage bool) error {
		return store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			_, err := billing.WorkspaceTx(ctx, tx, account, team, manage)
			return err
		})
	}
	for _, who := range []string{owner, payer} {
		if err := access(who, true); err != nil {
			t.Fatalf("%s may not manage billing: %v", who, err)
		}
	}
	for _, who := range []string{admin, auditor, member} {
		if err := access(who, true); !errors.Is(err, billing.ErrWorkspaceForbidden) {
			t.Fatalf("%s managed billing: %v", who, err)
		}
		if err := access(who, false); err != nil {
			t.Fatalf("%s may not read billing: %v", who, err)
		}
	}
}
