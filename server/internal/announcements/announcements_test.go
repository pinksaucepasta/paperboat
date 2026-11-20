package announcements

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/retention"
)

func TestNormalizeRefusesWhatTheConsoleCannotShow(t *testing.T) {
	ok := Input{Kind: KindNotice, Title: "Hello", AudienceKind: AudienceAll}
	bad := map[string]func(*Input){
		"kind":          func(i *Input) { i.Kind = "toast" },
		"empty title":   func(i *Input) { i.Title = "  " },
		"long title":    func(i *Input) { i.Title = strings.Repeat("a", MaxTitle+1) },
		"long body":     func(i *Input) { i.Body = strings.Repeat("a", MaxBody+1) },
		"http link":     func(i *Input) { i.LinkURL = "http://example.test/x" },
		"js link":       func(i *Input) { i.LinkURL = "javascript:alert(1)" },
		"scheme-less":   func(i *Input) { i.LinkURL = "//example.test/x" },
		"backslash":     func(i *Input) { i.LinkURL = `/\example.test` },
		"spaced link":   func(i *Input) { i.LinkURL = "https://example.test/a b" },
		"severity":      func(i *Input) { i.Severity = "fatal" },
		"audience":      func(i *Input) { i.AudienceKind = "everyone" },
		"unnamed team":  func(i *Input) { i.AudienceKind = AudienceTeam },
		"ends first":    func(i *Input) { s := time.Now(); i.StartsAt, i.EndsAt = &s, &s },
		"user no value": func(i *Input) { i.AudienceKind = AudienceUser },
	}
	for name, mutate := range bad {
		in := ok
		mutate(&in)
		if _, err := normalize(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: normalize = %v, want ErrInvalid", name, err)
		}
	}
	for _, link := range []string{"", "https://example.test/changelog?a=b", "/billing", "/"} {
		in := ok
		in.LinkURL = link
		if _, err := normalize(in); err != nil {
			t.Errorf("link %q refused: %v", link, err)
		}
	}
	in := ok
	in.Kind, in.Severity = KindNotice, "critical"
	if got, _ := normalize(in); got.Severity != "info" {
		t.Errorf("a notice keeps severity %q, want info", got.Severity)
	}
}

type fixture struct {
	ctx                  context.Context
	store                *db.DB
	svc                  *Service
	admin, alice, bob    string
	team, otherTeam, sfx string
}

func newFixture(t *testing.T) *fixture {
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
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	sfx := fmt.Sprint(time.Now().UnixNano())
	f := &fixture{ctx: ctx, store: store, svc: New(store), sfx: sfx,
		admin: "ann-admin-" + sfx, alice: "ann-alice-" + sfx, bob: "ann-bob-" + sfx, team: "ann-team-" + sfx, otherTeam: "ann-other-" + sfx}
	f.exec(t, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active'),($3,$3,$3||'@example.test','active')`, f.admin, f.alice, f.bob)
	f.exec(t, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1),($3,$2,1)`, f.team, f.alice, f.otherTeam)
	f.exec(t, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, f.team, f.alice)
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.announcements WHERE created_by=$1`, f.admin)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspaces WHERE user_id=$1`, f.bob)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_members WHERE team_id=$1`, f.team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id IN ($1,$2)`, f.team, f.otherTeam)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id IN ($1,$2,$3)`, f.admin, f.alice, f.bob)
	})
	return f
}

func (f *fixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.store.SQL().ExecContext(f.ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (f *fixture) create(t *testing.T, in Input) Announcement {
	t.Helper()
	a, err := f.svc.Create(f.ctx, f.admin, fmt.Sprint("key-", time.Now().UnixNano()), in)
	if err != nil {
		t.Fatalf("create %q: %v", in.Title, err)
	}
	return a
}

func titles(v View) []string {
	out := []string{}
	for _, n := range v.Notices {
		out = append(out, n.Title)
	}
	return out
}

func TestNoticesReachTheirAudienceAndKeepReadStatePostgres(t *testing.T) {
	f := newFixture(t)
	f.create(t, Input{Kind: KindNotice, Title: "to all " + f.sfx, LinkURL: "https://example.test/changelog?a=b", AudienceKind: AudienceAll})
	f.create(t, Input{Kind: KindNotice, Title: "to alice " + f.sfx, AudienceKind: AudienceUser, AudienceValue: f.alice})
	teamNotice := f.create(t, Input{Kind: KindNotice, Title: "to team " + f.sfx, AudienceKind: AudienceTeam, AudienceValue: f.team})
	f.create(t, Input{Kind: KindNotice, Title: "to the other team " + f.sfx, AudienceKind: AudienceTeam, AudienceValue: f.otherTeam})
	future := time.Now().Add(time.Hour)
	f.create(t, Input{Kind: KindNotice, Title: "later " + f.sfx, AudienceKind: AudienceUser, AudienceValue: f.alice, StartsAt: &future})

	mine := func(v View) []string { // other tests' rows may be live for everyone: look only at ours
		out := []string{}
		for _, title := range titles(v) {
			if strings.HasSuffix(title, f.sfx) {
				out = append(out, title)
			}
		}
		return out
	}
	alice, err := f.svc.ForUser(f.ctx, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first: the notices were sent team, alice, all in reverse.
	if got, want := strings.Join(mine(alice), "|"), fmt.Sprintf("to team %[1]s|to alice %[1]s|to all %[1]s", f.sfx); got != want {
		t.Fatalf("alice sees %q, want %q", got, want)
	}
	bob, _ := f.svc.ForUser(f.ctx, f.bob)
	if got := mine(bob); len(got) != 1 || got[0] != "to all "+f.sfx {
		t.Fatalf("bob sees %q, want only the notice for everyone", got)
	}

	var all, other string
	for _, n := range alice.Notices {
		if n.Title == "to all "+f.sfx {
			all = n.ID
			if n.LinkURL != "https://example.test/changelog?a=b" {
				t.Fatalf("notice link = %q", n.LinkURL)
			}
		}
		if n.Title == "to the other team "+f.sfx {
			other = n.ID
		}
	}
	if other != "" {
		t.Fatal("alice sees a notice for a team she is not in")
	}
	if err = f.svc.MarkRead(f.ctx, f.alice, []string{all, "ann_nonexistent"}); err != nil {
		t.Fatal(err)
	}
	// Bob cannot plant a receipt for a notice that is not his (the team's).
	if err = f.svc.MarkRead(f.ctx, f.bob, []string{teamNotice.ID}); err != nil {
		t.Fatal(err)
	}
	var planted int
	f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.announcement_receipts WHERE announcement_id=$1 AND user_id=$2`, teamNotice.ID, f.bob).Scan(&planted)
	if planted != 0 {
		t.Fatal("a receipt was written for a notice outside the audience")
	}
	after, _ := f.svc.ForUser(f.ctx, f.alice)
	read := map[string]bool{}
	for _, n := range after.Notices {
		read[n.Title] = n.Read
	}
	if !read["to all "+f.sfx] || read["to alice "+f.sfx] {
		t.Fatalf("read state = %v", read)
	}
	if after.Unread != alice.Unread-1 {
		t.Fatalf("unread %d, want %d", after.Unread, alice.Unread-1)
	}
	if err = f.svc.MarkRead(f.ctx, f.alice, make([]string, MaxMarked+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many ids = %v, want ErrInvalid", err)
	}

	// Retracting takes it out of the bell at once and the operator's list counts the read.
	retracted, err := f.svc.Retract(f.ctx, f.admin, "retract-1", all)
	if err != nil || retracted.State != "retracted" || retracted.Reads != 1 {
		t.Fatalf("retract = %+v, %v", retracted, err)
	}
	if again, err := f.svc.Retract(f.ctx, f.admin, "retract-2", all); err != nil || again.State != "retracted" {
		t.Fatalf("retract twice = %+v, %v", again, err)
	}
	last, _ := f.svc.ForUser(f.ctx, f.alice)
	for _, n := range last.Notices {
		if n.ID == all {
			t.Fatal("a retracted notice is still in the bell")
		}
	}
}

func TestBannerPicksOneAndRemembersDismissalPostgres(t *testing.T) {
	f := newFixture(t)
	yes, no := true, false
	forAll := f.create(t, Input{Kind: KindBanner, Title: "beta " + f.sfx, LinkURL: "/billing", AudienceKind: AudienceAll, Dismissible: &yes})
	// One ribbon at a time per audience: a second banner for everyone in the same window is refused.
	_, err := f.svc.Create(f.ctx, f.admin, "dup-"+f.sfx, Input{Kind: KindBanner, Title: "second", AudienceKind: AudienceAll})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("overlapping banner = %v, want ErrConflict", err)
	}
	// So is a later window, while the first has no end.
	start, end := time.Now().Add(2*time.Hour), time.Now().Add(3*time.Hour)
	if _, err = f.svc.Create(f.ctx, f.admin, "later-"+f.sfx, Input{Kind: KindBanner, Title: "later", AudienceKind: AudienceAll, StartsAt: &start, EndsAt: &end}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a banner inside the running one's open end = %v, want ErrConflict", err)
	}
	userBanner := f.create(t, Input{Kind: KindBanner, Title: "only alice " + f.sfx, Severity: "critical", AudienceKind: AudienceUser, AudienceValue: f.alice, Dismissible: &no})

	// The most specific audience wins; bob only has the one for everyone (other tests' banners may also run: ours is the check).
	alice, _ := f.svc.ForUser(f.ctx, f.alice)
	if alice.Banner == nil || alice.Banner.ID != userBanner.ID || alice.Banner.Dismissible {
		t.Fatalf("alice banner = %+v", alice.Banner)
	}
	if err = f.svc.Dismiss(f.ctx, f.alice, userBanner.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dismissing an undismissable banner = %v, want ErrNotFound", err)
	}
	if err = f.svc.Dismiss(f.ctx, f.alice, "ann_nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dismissing nothing = %v, want ErrNotFound", err)
	}
	// Retract hers: she falls back to the one for everyone, dismisses it, and it stays dismissed for her only.
	if _, err = f.svc.Retract(f.ctx, f.admin, "r-"+f.sfx, userBanner.ID); err != nil {
		t.Fatal(err)
	}
	if alice, _ = f.svc.ForUser(f.ctx, f.alice); alice.Banner == nil || alice.Banner.ID != forAll.ID {
		t.Fatalf("alice banner after retract = %+v", alice.Banner)
	}
	if err = f.svc.Dismiss(f.ctx, f.alice, forAll.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.Dismiss(f.ctx, f.alice, forAll.ID); err != nil {
		t.Fatalf("dismissing twice: %v", err)
	}
	if alice, _ = f.svc.ForUser(f.ctx, f.alice); alice.Banner != nil && alice.Banner.ID == forAll.ID {
		t.Fatal("a dismissed banner came back")
	}
	if bob, _ := f.svc.ForUser(f.ctx, f.bob); bob.Banner == nil || bob.Banner.ID != forAll.ID || bob.Banner.LinkURL != "/billing" {
		t.Fatalf("bob's banner = %+v, want the one for everyone", bob.Banner)
	}
	// Making it undismissable brings it back for those who dismissed it.
	updated, err := f.svc.Update(f.ctx, f.admin, "u-"+f.sfx, forAll.ID, Input{Kind: KindBanner, Title: "beta " + f.sfx, AudienceKind: AudienceAll, Dismissible: &no})
	if err != nil || updated.Dismissals != 1 {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	if alice, _ = f.svc.ForUser(f.ctx, f.alice); alice.Banner == nil || alice.Banner.ID != forAll.ID {
		t.Fatalf("alice banner after it became undismissable = %+v", alice.Banner)
	}
	if _, err = f.svc.Update(f.ctx, f.admin, "u2-"+f.sfx, forAll.ID, Input{Kind: KindNotice, Title: "x", AudienceKind: AudienceAll}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changing the kind = %v, want ErrConflict", err)
	}
}

func TestPlanAudienceAndValidationPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Create(f.ctx, f.admin, "p1-"+f.sfx, Input{Kind: KindNotice, Title: "x", AudienceKind: AudiencePlan, AudienceValue: "no-such-plan-" + f.sfx}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown plan = %v, want ErrInvalid", err)
	}
	if _, err := f.svc.Create(f.ctx, f.admin, "p2-"+f.sfx, Input{Kind: KindNotice, Title: "x", AudienceKind: AudienceTeam, AudienceValue: "no-such-team"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown team = %v, want ErrInvalid", err)
	}
	if _, err := f.svc.Create(f.ctx, f.admin, "p3-"+f.sfx, Input{Kind: KindNotice, Title: "x", AudienceKind: AudienceUser, AudienceValue: "no-such-user"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown user = %v, want ErrInvalid", err)
	}
	past := time.Now().Add(-time.Hour)
	if _, err := f.svc.Create(f.ctx, f.admin, "p4-"+f.sfx, Input{Kind: KindNotice, Title: "x", AudienceKind: AudienceAll, EndsAt: &past}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ends in the past = %v, want ErrInvalid", err)
	}
	// A retried key returns the row it made.
	a1, err := f.svc.Create(f.ctx, f.admin, "same-"+f.sfx, Input{Kind: KindNotice, Title: "once " + f.sfx, AudienceKind: AudienceUser, AudienceValue: f.alice})
	a2, err2 := f.svc.Create(f.ctx, f.admin, "same-"+f.sfx, Input{Kind: KindNotice, Title: "once " + f.sfx, AudienceKind: AudienceUser, AudienceValue: f.alice})
	if err != nil || err2 != nil || a1.ID != a2.ID {
		t.Fatalf("retry = %v / %v, %v %v", a1.ID, a2.ID, err, err2)
	}

	// free: users without a paid personal plan; a paid plan code: users on it (inserted directly: no billing product fixture).
	f.exec(t, `INSERT INTO paperboat.billing_workspaces(id,user_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','ann-pro',1,now()-interval '1 day',now()+interval '30 days')`, "bw_"+f.sfx, f.bob)
	for kind, value := range map[string]string{"free-" + f.sfx: FreePlan, "pro-" + f.sfx: "ann-pro"} {
		f.exec(t, `INSERT INTO paperboat.announcements(id,kind,title,audience_kind,audience_value,starts_at,created_by,idempotency_key) VALUES($1,'notice',$1,'plan',$2,now(),$3,$1)`, "ann_"+kind, value, f.admin)
	}
	has := func(user, title string) bool {
		v, err := f.svc.ForUser(f.ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range v.Notices {
			if n.Title == "ann_"+title {
				return true
			}
		}
		return false
	}
	if !has(f.alice, "free-"+f.sfx) || has(f.alice, "pro-"+f.sfx) {
		t.Fatal("alice (no paid plan) should see free and not pro")
	}
	if has(f.bob, "free-"+f.sfx) || !has(f.bob, "pro-"+f.sfx) {
		t.Fatal("bob (pro) should see pro and not free")
	}

	page, err := f.svc.List(f.ctx, "live", 0, 2)
	if err != nil || len(page.Announcements) != 2 || page.NextAfter == "" || page.Total < 3 {
		t.Fatalf("list = %d rows next %q total %d, %v", len(page.Announcements), page.NextAfter, page.Total, err)
	}
	if _, err = f.svc.List(f.ctx, "sleeping", 0, 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("list state = %v, want ErrInvalid", err)
	}
}

func TestRetentionDeletesEndedAndOpenEndedNoticesButKeepsRunningBannersPostgres(t *testing.T) {
	f := newFixture(t)
	insert := func(id, kind, starts, ends, retracted string) {
		f.exec(t, `INSERT INTO paperboat.announcements(id,kind,title,audience_kind,starts_at,ends_at,retracted_at,retracted_by,created_by,idempotency_key)
 VALUES($1,$2,$1,'all',now()-$3::interval,now()-nullif($4,'')::interval,now()-nullif($5,'')::interval,CASE WHEN $5='' THEN NULL ELSE 'op' END,$6,$1)`, id, kind, starts, ends, retracted, f.admin)
	}
	old, fresh := "ann_ret_old_"+f.sfx, "ann_ret_new_"+f.sfx
	insert(old+"_notice_open", "notice", "200 days", "", "")
	insert(old+"_notice_ended", "notice", "200 days", "150 days", "")
	insert(old+"_banner_retracted", "banner", "200 days", "", "120 days")
	insert(old+"_banner_open", "banner", "200 days", "", "")
	insert(fresh+"_notice_open", "notice", "1 day", "", "")
	f.exec(t, `INSERT INTO paperboat.announcement_receipts(announcement_id,user_id,read_at) VALUES($1,$2,now())`, old+"_notice_open", f.alice)
	if _, err := retention.Run(f.ctx, f.store, config.Default().Retention); err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	rows, err := f.store.SQL().QueryContext(f.ctx, `SELECT id FROM paperboat.announcements WHERE created_by=$1`, f.admin)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left[id] = true
	}
	rows.Close()
	if len(left) != 2 || !left[old+"_banner_open"] || !left[fresh+"_notice_open"] {
		t.Fatalf("left after retention = %v, want only the open banner and the recent notice", left)
	}
	var receipts int
	f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.announcement_receipts WHERE announcement_id=$1`, old+"_notice_open").Scan(&receipts)
	if receipts != 0 {
		t.Fatal("receipts outlived their announcement")
	}
}

func TestRetriedKeyMustMeanTheSameWriteAndRetractIsAuditedOncePostgres(t *testing.T) {
	f := newFixture(t)
	in := Input{Kind: KindNotice, Title: "once " + f.sfx, AudienceKind: AudienceUser, AudienceValue: f.alice}
	a, err := f.svc.Create(f.ctx, f.admin, "k-"+f.sfx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Title = "something else"
	if _, err = f.svc.Create(f.ctx, f.admin, "k-"+f.sfx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key, different write = %v, want ErrConflict", err)
	}
	past := time.Now().Add(-time.Hour)
	if _, err = f.svc.Update(f.ctx, f.admin, "u-"+f.sfx, a.ID, Input{Kind: KindNotice, Title: "x", AudienceKind: AudienceAll, EndsAt: &past}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an edit that ends in the past = %v, want ErrInvalid", err)
	}
	for i := 0; i < 2; i++ {
		if _, err = f.svc.Retract(f.ctx, f.admin, fmt.Sprint("r", i, f.sfx), a.ID); err != nil {
			t.Fatal(err)
		}
	}
	var events int
	f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_type='announcement' AND resource_id=$1 AND event_type='operator.announcement_retract'`, a.ID).Scan(&events)
	if events != 1 {
		t.Fatalf("retract audited %d times, want once", events)
	}
	if _, err = f.svc.Retract(f.ctx, f.admin, "r-none", "ann_nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retract of nothing = %v, want ErrNotFound", err)
	}
}

func TestOperatorNoticeIsOnePerKeyPostgres(t *testing.T) {
	f := newFixture(t)
	key := "release-metadata:test:" + f.sfx
	t.Cleanup(func() {
		f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.announcements WHERE created_by=$1 AND idempotency_key LIKE $2`, SystemCreator, "%"+f.sfx+"%")
	})
	for i := 0; i < 2; i++ {
		if err := f.svc.NoticeOperators(f.ctx, key+":a", "Timestamp release metadata expires soon "+f.sfx, "body", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.NoticeOperators(f.ctx, key+":b", "Targets release metadata expires soon "+f.sfx, "body", time.Hour); err != nil {
		t.Fatal(err)
	}
	notices, err := f.svc.ForOperators(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, n := range notices {
		if strings.HasSuffix(n.Title, f.sfx) {
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("operators saw %d of this test's notices, want 2 (a repeated key notifies once): %+v", seen, notices)
	}
}
