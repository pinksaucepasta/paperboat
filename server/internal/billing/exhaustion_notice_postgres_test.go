package billing_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// F13-03: when a debit uses up the allowance, the owner (personal) or owner and admins (team)
// get one Activity event and one mail per allowance period, and nobody else does.
func TestBandwidthExhaustionNoticeGoesOnceToOwnerAndAdminsPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, admin, member, solo := "notice_o_"+x, "notice_a_"+x, "notice_m_"+x, "notice_s_"+x
	team, product := "notice-team-"+x, "notice_plan_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []string{owner, admin, member, solo} {
		insertUser(t, store, u, u+"@example.test")
		exec(`UPDATE paperboat.users SET email_verified=true WHERE id=$1`, u)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.transactional_mail_outbox WHERE mail_id LIKE '%'||$1||'%'`,
			`DELETE FROM paperboat.audit_events WHERE idempotency_key LIKE '%'||$1||'%'`,
			`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id LIKE '%'||$1||'%'`,
			`DELETE FROM paperboat.billing_workspaces WHERE id LIKE '%'||$1||'%'`,
			`DELETE FROM paperboat.team_members WHERE team_id=$2`,
			`DELETE FROM paperboat.teams WHERE team_id=$2`,
			`DELETE FROM paperboat.billing_products WHERE code=$3`,
		} {
			if strings.Contains(q, "$3") {
				_, _ = store.SQL().ExecContext(context.Background(), q, x, team, product)
			} else if strings.Contains(q, "$2") {
				_, _ = store.SQL().ExecContext(context.Background(), q, x, team)
			} else {
				_, _ = store.SQL().ExecContext(context.Background(), q, x)
			}
		}
		for _, u := range []string{owner, admin, member, solo} {
			_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, u)
		}
	})
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	for _, m := range []struct{ id, role string }{{owner, "admin"}, {admin, "admin"}, {member, "member"}} {
		exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,$3,true)`, team, m.id, m.role)
	}
	at := time.Now().UTC().Truncate(time.Second)
	start, end := at.Add(-time.Hour), at.AddDate(0, 1, 0)
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref) VALUES($1,$1,'stripe',$1,$1,'personal_subscription','{"allowance_bytes":100}')`, product)
	soloWorkspace, teamWorkspace := "personal:"+solo, "team:"+team
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,state,product_code,period_start,period_end,bandwidth_anchor) VALUES($1,$2,'active',$3,$4,$5,$4)`, soloWorkspace, solo, product, start, end)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,period_start,period_end,bandwidth_anchor) VALUES($1,$2,'active',$3,$4,$5,$4)`, teamWorkspace, team, product, start, end)

	s := billing.NewWorkspaceService(store, nil, "")
	s.ConfigureNotificationMail(config.TransactionalMail{From: "Paperboat <noreply@example.test>", ConsoleOrigin: "https://console.example.test"}, true)
	debit := func(workspace string, n int64) bool {
		t.Helper()
		var exhausted bool
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var err error
			_, exhausted, err = s.DebitWorkspaceBandwidthTx(ctx, tx, workspace, n, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return exhausted
	}
	count := func(q string, args ...any) (n int) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	notices := func(workspace string) int {
		return count(`SELECT count(*) FROM paperboat.audit_events WHERE event_type=$1 AND idempotency_key LIKE $2`, billing.BandwidthExhaustedEvent, "bandwidth-exhausted:"+workspace+":%")
	}
	mails := func(workspace, account string) int {
		return count(`SELECT count(*) FROM paperboat.transactional_mail_outbox WHERE mail_id LIKE $1`, "bandwidth-exhausted:"+workspace+":%:"+account)
	}

	if debit(soloWorkspace, 60) || notices(soloWorkspace) != 0 {
		t.Fatal("a debit below the allowance produced a notice")
	}
	if !debit(soloWorkspace, 60) {
		t.Fatal("crossing the allowance did not report exhaustion")
	}
	if notices(soloWorkspace) != 1 || mails(soloWorkspace, solo) != 1 {
		t.Fatalf("personal notice: events=%d mails=%d, want 1/1", notices(soloWorkspace), mails(soloWorkspace, solo))
	}
	debit(soloWorkspace, 10)
	debit(soloWorkspace, 10)
	if notices(soloWorkspace) != 1 || mails(soloWorkspace, solo) != 1 {
		t.Fatal("repeated debits in the same period sent the notice again")
	}
	var activityWorkspace string
	if err := store.SQL().QueryRowContext(ctx, `SELECT activity_workspace_id FROM paperboat.audit_events WHERE event_type=$1 AND resource_id=$2`, billing.BandwidthExhaustedEvent, solo).Scan(&activityWorkspace); err != nil || activityWorkspace != soloWorkspace {
		t.Fatalf("event is not in the workspace's Activity feed: %q %v", activityWorkspace, err)
	}

	debit(teamWorkspace, 150)
	if notices(teamWorkspace) != 1 {
		t.Fatalf("team events=%d, want 1", notices(teamWorkspace))
	}
	if mails(teamWorkspace, owner) != 1 || mails(teamWorkspace, admin) != 1 || mails(teamWorkspace, member) != 0 {
		t.Fatalf("team mails owner=%d admin=%d member=%d, want 1/1/0", mails(teamWorkspace, owner), mails(teamWorkspace, admin), mails(teamWorkspace, member))
	}
	var body string
	if err := store.SQL().QueryRowContext(ctx, `SELECT payload->>'html' FROM paperboat.transactional_mail_outbox WHERE mail_id LIKE $1`, "bandwidth-exhausted:"+teamWorkspace+":%:"+admin).Scan(&body); err != nil || !strings.Contains(body, "https://console.example.test/billing") || !strings.Contains(body, "Direct machine-to-machine traffic keeps working") {
		t.Fatalf("mail lacks the billing link or the direct-traffic note: %q %v", body, err)
	}
}
