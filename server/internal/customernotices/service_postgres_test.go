package customernotices

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/announcements"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

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

// A notice reaches exactly its audience once: a paid audience is the paying owners (personal, team owner), never a free
// or unverified account; a retried key queues nothing more; the same key for other content conflicts; a preview sends nothing.
func TestSendReachesTheAudienceOnce(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	s := New(store)
	x := fmt.Sprint(time.Now().UnixNano())
	operator := "operator_" + uuid.NewString()
	paid, free, unverified, owner, member, team := "nt_paid_"+x, "nt_free_"+x, "nt_unv_"+x, "nt_owner_"+x, "nt_member_"+x, "nt-team-"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','x','active','admin')`, operator)
	for id, verified := range map[string]bool{paid: true, free: true, unverified: false, owner: true, member: true} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status,role) VALUES($1,'wo_'||$1,$1||'@example.test',$2,'active','user')`, id, verified)
	}
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,product_code,state,provider_subscription_id,period_start,period_end) VALUES('personal:'||$1,$1,'pro','active','sub_'||$1,now()-interval '1 day',now()+interval '1 day'),('personal:'||$2,$2,'pro','active','sub_'||$2,now()-interval '1 day',now()+interval '1 day')`, paid, unverified)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, team, owner, member)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,product_code,state,provider_subscription_id,period_start,period_end) VALUES('team:'||$1,$1,'team','active','sub_t_'||$1,now()-interval '1 day',now()+interval '1 day')`, team)
	var ids []string
	t.Cleanup(func() {
		bg := context.Background()
		for _, id := range ids {
			store.SQL().ExecContext(bg, `DELETE FROM paperboat.transactional_mail_outbox WHERE mail_id LIKE $1`, "notice:"+id+":%")
		}
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.customer_notices WHERE created_by=$1`, operator)
	})
	sent := func(id, user string) bool {
		var n int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.transactional_mail_outbox WHERE mail_id=$1`, "notice:"+id+":"+user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	price := Input{Kind: "price_increase", AudienceKind: "paid", Detail: "Pro $9 to $12 a month", Effective: time.Now().UTC().AddDate(0, 0, 31).Format("2006-01-02")}

	if _, err := s.Send(ctx, operator, "key-"+x, price); !errors.Is(err, ErrMailOff) {
		t.Fatalf("send without mail configured: %v", err)
	}
	s.ConfigureMail(config.TransactionalMail{From: "Paperboat <noreply@example.test>"}, true)
	preview := price
	preview.DryRun = true
	out, err := s.Send(ctx, operator, "preview-"+x, preview)
	if err != nil || out.ID != "" || out.Recipients < 2 || !strings.Contains(out.Subject, "prices change") {
		t.Fatalf("preview = %+v err=%v", out, err)
	}
	var notices int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.customer_notices WHERE created_by=$1`, operator).Scan(&notices); err != nil || notices != 0 {
		t.Fatalf("a preview recorded %d notices (err %v)", notices, err)
	}

	first, err := s.Send(ctx, operator, "key-"+x, price)
	if err != nil || first.ID == "" {
		t.Fatalf("send = %+v err=%v", first, err)
	}
	ids = append(ids, first.ID)
	for user, want := range map[string]bool{paid: true, owner: true, free: false, unverified: false, member: false} {
		if sent(first.ID, user) != want {
			t.Errorf("mail to %s sent=%v, want %v", user, !want, want)
		}
	}
	again, err := s.Send(ctx, operator, "key-"+x, price)
	if err != nil || again.ID != first.ID || again.Recipients != first.Recipients {
		t.Fatalf("retry = %+v err=%v", again, err)
	}
	other := price
	other.Detail = "something else"
	if _, err = s.Send(ctx, operator, "key-"+x, other); !errors.Is(err, ErrConflict) {
		t.Fatalf("the same key for another notice: %v", err)
	}

	breach := Input{Kind: "breach", AudienceKind: "user", AudienceValue: free, AwareAt: time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339), What: "w", Data: "d", Actions: "a", Contact: "privacy@example.test"}
	late, err := s.Send(ctx, operator, "breach-"+x, breach)
	if err != nil || !late.Late || late.Recipients != 1 {
		t.Fatalf("late breach notice = %+v err=%v", late, err)
	}
	ids = append(ids, late.ID)
	if !sent(late.ID, free) {
		t.Error("the affected user was not mailed")
	}
	breach.AudienceKind, breach.AudienceValue = "team", "nt-no-such-team"
	if _, err = s.Send(ctx, operator, "breach2-"+x, breach); !errors.Is(err, announcements.ErrInvalid) {
		t.Fatalf("a team that does not exist: %v", err)
	}
	page, _, err := s.List(ctx, 0, 100)
	if err != nil || len(page) < 2 {
		t.Fatalf("list = %d err=%v", len(page), err)
	}
}
