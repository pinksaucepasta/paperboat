package operatoractions

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// An operator's ban of an account does everything it promises in one receipt: the ban record keeps the identity that may not
// come back and the reference, the peer certificates are revoked, the teams the account owns are suspended with it, its
// subscriptions are queued to end when chosen, and the chosen networks are blocked. Unban lifts the record and the team bans it
// caused (not the networks); a team can be banned and unbanned on its own.
func TestAccountAndTeamBanThroughReceipts(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	s := NewService(store)
	x := fmt.Sprint(time.Now().UnixNano())
	admin, support := "operator_"+uuid.NewString(), "operator_"+uuid.NewString()
	user, team, solo := "ban_user_"+x, "ban-team-"+x, "ban-solo-"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	scan := func(dest any, query string, args ...any) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, query, args...).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','x','active','admin'),($2,$2||'@test.invalid','x','active','support')`, admin, support)
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,role) VALUES($1,'wo_'||$1,'Person.'||$1||'+work@Test.Invalid','active','user')`, user)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1),($3,$2,1)`, team, user, solo)
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,state,provider_subscription_id,period_start,period_end) VALUES('personal:'||$1,$1,'active','sub_p_'||$1,now()-interval '1 day',now()+interval '1 day')`, user)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,provider_subscription_id,period_start,period_end) VALUES('team:'||$1,$1,'active','sub_t_'||$1,now()-interval '1 day',now()+interval '1 day')`, team)
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
VALUES(sha256(convert_to($1,'UTF8')),$1,'machine_'||$1,'machine',1,1,decode(repeat('00',200),'hex'),sha256(convert_to('q'||$1,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',sha256(convert_to('s'||$1,'UTF8')))`, user)
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM paperboat.account_deletion_jobs WHERE user_id=$1`, `DELETE FROM paperboat.bans WHERE subject_id IN ($1,$2,$3)`,
		} {
			store.SQL().ExecContext(bg, q, user, team, solo)
		}
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.ip_blocks WHERE reason LIKE $1`, "ban:%"+x)
	})
	run := func(actor, key, kind, target string, in Input) (Result, error) {
		return s.Execute(ctx, actor, key, kind, target, "ban:"+x, in)
	}
	version := func() int64 {
		var v int64
		scan(&v, `SELECT version FROM paperboat.users WHERE id=$1`, user)
		return v
	}
	count := func(query string, args ...any) (n int) {
		t.Helper()
		scan(&n, query, args...)
		return
	}

	for name, in := range map[string]Input{
		"unknown subscriptions": {Revision: version(), Subscriptions: "weekly"},
		"bad network":           {Revision: version(), BlockIPs: []string{"not-an-ip"}},
		"whole internet":        {Revision: version(), BlockIPs: []string{"0.0.0.0/0"}},
		"too many networks":     {Revision: version(), BlockIPs: make([]string, MaxBanIPBlocks+1)},
	} {
		if _, err := run(admin, "bad-"+name+x, "account_ban", user, in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := run(admin, "unban-subs-"+x, "account_unban", user, Input{Revision: version(), Subscriptions: "immediate"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unban with subscriptions: %v", err)
	}
	if _, err := run(support, "support-ban-"+x, "account_ban", user, Input{Revision: version()}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin banned an account: %v", err)
	}
	if n := count(`SELECT count(*) FROM paperboat.bans WHERE subject_id=$1`, user); n != 0 {
		t.Fatalf("a refused ban left %d rows", n)
	}

	ban := Input{Revision: version(), Subscriptions: "period_end", BlockIPs: []string{"203.0.113.0/24", "2001:db8:99::/48"}}
	res, err := run(admin, "ban-"+x, "account_ban", user, ban)
	if err != nil || res.State != "suspended" || res.Teams != 2 {
		t.Fatalf("ban: %+v %v", res, err)
	}
	if replay, err := run(admin, "ban-"+x, "account_ban", user, ban); err != nil || replay.Teams != 2 {
		t.Fatalf("an exact replay: %+v %v", replay, err)
	}
	var banStamp time.Time
	scan(&banStamp, `SELECT banned_at FROM paperboat.bans WHERE subject_kind='user' AND subject_id=$1 AND lifted_at IS NULL`, user)
	// A second ban of a banned account (a new key, the current revision) is a conflict that changes nothing, and neither is a
	// legal flag accepted on a ban.
	if _, err := run(admin, "reban-"+x, "account_ban", user, Input{Revision: version(), Subscriptions: "immediate"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second ban: %v", err)
	}
	if _, err := run(admin, "legal-"+x, "account_ban", user, Input{Revision: version(), Legal: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a ban with the legal flag: %v", err)
	}
	var again time.Time
	scan(&again, `SELECT banned_at FROM paperboat.bans WHERE subject_kind='user' AND subject_id=$1 AND lifted_at IS NULL`, user)
	if !again.Equal(banStamp) || count(`SELECT count(*) FROM paperboat.account_deletion_jobs WHERE user_id=$1 AND kind='stripe_subscription'`, user) != 0 {
		t.Fatal("a refused second ban changed the ban or queued provider work")
	}
	var status, reason, email, subject string
	scan(&status, `SELECT status FROM paperboat.users WHERE id=$1`, user)
	if status != "suspended" {
		t.Fatalf("status %s", status)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT reason,canonical_email,workos_subject FROM paperboat.bans WHERE subject_kind='user' AND subject_id=$1 AND lifted_at IS NULL`, user).Scan(&reason, &email, &subject); err != nil {
		t.Fatal(err)
	}
	if reason != "ban:"+x || email != "person."+user+"@test.invalid" || subject != "wo_"+user {
		t.Fatalf("the ban record keeps %q %q %q (the e-mail is canonical: lower case, no +suffix)", reason, email, subject)
	}
	if n := count(`SELECT count(*) FROM paperboat.peer_endpoint_certificates WHERE user_id=$1 AND revoked_at IS NULL`, user); n != 0 {
		t.Fatalf("%d peer certificates still live", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.teams WHERE owner_account=$1 AND suspended`, user); n != 2 {
		t.Fatalf("%d owned teams suspended, want 2", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.bans b JOIN paperboat.bans c ON c.id=b.cause_ban_id WHERE b.subject_kind='team' AND c.subject_id=$1 AND b.lifted_at IS NULL`, user); n != 2 {
		t.Fatalf("%d team bans caused by the owner's ban, want 2", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.account_deletion_jobs WHERE user_id=$1 AND kind='stripe_cancel_at_period_end' AND target IN ('sub_p_'||$1,'sub_t_'||$2)`, user, team); n != 2 {
		t.Fatalf("%d period-end jobs, want 2 (the account's and the team's subscription)", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.ip_blocks WHERE reason=$1 AND revoked_at IS NULL AND cidr IN ('203.0.113.0/24','2001:db8:99::/48')`, "ban:"+x); n != 2 {
		t.Fatalf("%d networks blocked, want 2", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.audit_events WHERE event_type='operator.account_ban' AND resource_id=$1 AND metadata->>'subscriptions'='period_end'`, user); n != 1 {
		t.Fatalf("%d audit events", n)
	}

	// A team deleted while its owner was banned does not keep a running ban once the owner is unbanned.
	exec(`UPDATE paperboat.teams SET deleted_at=now() WHERE team_id=$1`, solo)
	// Unban: the account and the teams its ban caused are back; the networks stay blocked until an operator lifts them.
	res, err = run(admin, "unban-"+x, "account_unban", user, Input{Revision: version()})
	if err != nil || res.State != "active" || res.Teams != 1 {
		t.Fatalf("unban: %+v %v", res, err)
	}
	if n := count(`SELECT count(*) FROM paperboat.bans WHERE subject_id IN ($1,$2,$3) AND lifted_at IS NULL`, user, team, solo); n != 0 {
		t.Fatalf("%d bans still running", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.teams WHERE owner_account=$1 AND suspended AND deleted_at IS NULL`, user); n != 0 {
		t.Fatalf("%d teams still suspended", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.ip_blocks WHERE reason=$1 AND revoked_at IS NULL`, "ban:"+x); n != 2 {
		t.Fatalf("the unban lifted the network blocks (%d left)", n)
	}

	// A team alone: its ban, with its subscription ended now, and its unban.
	var generation int64
	scan(&generation, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, team)
	if res, err = run(admin, "team-ban-"+x, "team_ban", team, Input{Revision: generation, Subscriptions: "immediate"}); err != nil || res.State != "suspended" {
		t.Fatalf("team ban: %+v %v", res, err)
	}
	if n := count(`SELECT count(*) FROM paperboat.bans WHERE subject_kind='team' AND subject_id=$1 AND lifted_at IS NULL AND subscriptions='immediate'`, team); n != 1 {
		t.Fatalf("%d team ban rows", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.account_deletion_jobs WHERE user_id=$1 AND kind='stripe_subscription' AND target='sub_t_'||$2`, user, team); n != 1 {
		t.Fatalf("%d immediate-cancel jobs", n)
	}
	if _, err = run(admin, "team-ban-ips-"+x, "team_ban", solo, Input{Revision: 1, BlockIPs: []string{"203.0.113.9"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a team ban with networks: %v", err)
	}
	if res, err = run(admin, "team-unban-"+x, "team_unban", team, Input{Revision: generation + 1}); err != nil || res.State != "active" {
		t.Fatalf("team unban: %+v %v", res, err)
	}
	if n := count(`SELECT count(*) FROM paperboat.bans WHERE subject_kind='team' AND subject_id=$1 AND lifted_at IS NULL`, team); n != 0 {
		t.Fatalf("%d team bans running after the unban", n)
	}

	// A provider job that gave up (failed) is queued again by a later ban of the same subscription; a done one is left alone.
	exec(`UPDATE paperboat.account_deletion_jobs SET state='failed',attempts=12 WHERE user_id=$1 AND kind='stripe_cancel_at_period_end' AND target='sub_p_'||$1`, user)
	exec(`UPDATE paperboat.account_deletion_jobs SET state='done',completed_at=now() WHERE user_id=$1 AND kind='stripe_cancel_at_period_end' AND target='sub_t_'||$2`, user, team)
	if res, err = run(admin, "ban-again-"+x, "account_ban", user, Input{Revision: version(), Subscriptions: "period_end"}); err != nil || res.State != "suspended" {
		t.Fatalf("a ban after the unban: %+v %v", res, err)
	}
	if n := count(`SELECT count(*) FROM paperboat.account_deletion_jobs WHERE user_id=$1 AND kind='stripe_cancel_at_period_end' AND target='sub_p_'||$1 AND state='queued' AND attempts=0`, user); n != 1 {
		t.Fatalf("the failed job was not queued again (%d)", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.account_deletion_jobs WHERE user_id=$1 AND kind='stripe_cancel_at_period_end' AND target='sub_t_'||$2 AND state='done'`, user, team); n != 1 {
		t.Fatalf("a finished job was disturbed (%d)", n)
	}
}

// An operator blocks a network by address or range with a reference and an optional end, and lifts it; only an admin;
// a duplicate, a past end and a range wider than /8 (IPv6 /16) are refused.
func TestIPBlockThroughReceipts(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	s := NewService(store)
	x := fmt.Sprint(time.Now().UnixNano())
	admin, support := "operator_"+uuid.NewString(), "operator_"+uuid.NewString()
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','x','active','admin'),($2,$2||'@test.invalid','x','active','support')`, admin, support); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.ip_blocks WHERE reason=$1`, "ipb:"+x)
	})
	run := func(actor, key, kind, target string, in Input) (Result, error) {
		return s.Execute(ctx, actor, key, kind, target, "ipb:"+x, in)
	}
	ends := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := run(support, "ip-support-"+x, "ip_block_add", "ip-blocks", Input{CIDR: "198.51.100.0/24"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin blocked a network: %v", err)
	}
	for name, in := range map[string]Input{
		"no range":       {},
		"not an address": {CIDR: "banana"},
		"whole internet": {CIDR: "0.0.0.0/0"},
		"too wide":       {CIDR: "10.0.0.0/7"},
		"too wide IPv6":  {CIDR: "2000::/15"},
		"past end":       {CIDR: "198.51.100.0/24", EndsAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		"bad end":        {CIDR: "198.51.100.0/24", EndsAt: "soon"},
		"revision":       {CIDR: "198.51.100.0/24", Revision: 1},
		"plan fields":    {CIDR: "198.51.100.0/24", ProductCode: "pro"},
	} {
		if _, err := run(admin, "ip-bad-"+name+x, "ip_block_add", "ip-blocks", in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A bare address is a single-address block; a mapped IPv6 address blocks the IPv4 one.
	one, err := run(admin, "ip-one-"+x, "ip_block_add", "ip-blocks", Input{CIDR: "::ffff:198.51.100.7", EndsAt: ends})
	if err != nil || one.State != "blocked" || one.ResourceID == "" || one.ExpiresAt == nil {
		t.Fatalf("add: %+v %v", one, err)
	}
	var cidr string
	if err = store.SQL().QueryRowContext(ctx, `SELECT cidr::text FROM paperboat.ip_blocks WHERE id=$1`, one.ResourceID).Scan(&cidr); err != nil || cidr != "198.51.100.7/32" {
		t.Fatalf("stored %q %v", cidr, err)
	}
	if _, err = run(admin, "ip-dup-"+x, "ip_block_add", "ip-blocks", Input{CIDR: "198.51.100.7"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a duplicate: %v", err)
	}
	if replay, err := run(admin, "ip-one-"+x, "ip_block_add", "ip-blocks", Input{CIDR: "::ffff:198.51.100.7", EndsAt: ends}); err != nil || replay.ResourceID != one.ResourceID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	// A block that ran out is replaced by a new one of the same range.
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.ip_blocks SET expires_at=now()-interval '1 second' WHERE id=$1`, one.ResourceID); err != nil {
		t.Fatal(err)
	}
	again, err := run(admin, "ip-again-"+x, "ip_block_add", "ip-blocks", Input{CIDR: "198.51.100.7"})
	if err != nil || again.ResourceID == one.ResourceID {
		t.Fatalf("re-adding an expired block: %+v %v", again, err)
	}
	if _, err = run(admin, "ip-rm-bad-"+x, "ip_block_remove", one.ResourceID, Input{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lifting an ended block: %v", err)
	}
	if res, err := run(admin, "ip-rm-"+x, "ip_block_remove", again.ResourceID, Input{}); err != nil || res.State != "lifted" {
		t.Fatalf("lift: %+v %v", res, err)
	}
	if _, err = run(admin, "ip-rm-twice-"+x, "ip_block_remove", again.ResourceID, Input{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lifting a lifted block: %v", err)
	}
	var events int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type IN ('operator.ip_block_add','operator.ip_block_remove') AND resource_type='ip_block' AND resource_id IN ($1,$2)`, one.ResourceID, again.ResourceID).Scan(&events); err != nil || events != 3 {
		t.Fatalf("audit events %d %v", events, err)
	}
}

// A ban waits for a sign-in in flight (which holds the canonical e-mail lock while it resolves the identity) and then
// revokes what that sign-in created, so a banned account never ends up holding a session minted after the ban.
func TestBanWaitsForASignInInFlight(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	s := NewService(store)
	x := fmt.Sprint(time.Now().UnixNano())
	admin, user := "operator_"+uuid.NewString(), "ban_race_"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','x','active','admin')`, admin)
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,role) VALUES($1,'wo_'||$1,$1||'@test.invalid','active','user')`, user)
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.bans WHERE subject_id=$1`, user)
	})
	signIn, err := store.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = signIn.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(paperboat.canonical_email_address($1||'@test.invalid'),174))`, user); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Execute(ctx, admin, "race-"+x, "account_ban", user, "ban:"+x, Input{Revision: 1})
		done <- err
	}()
	// The ban is provably queued behind the sign-in's lock once PostgreSQL lists it as a waiter on that key.
	var key int64
	if err = store.SQL().QueryRowContext(ctx, `SELECT hashtextextended(paperboat.canonical_email_address($1||'@test.invalid'),174)`, user).Scan(&key); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		var waiting int
		if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted
 AND ((classid::bigint<<32)|objid::bigint)=$1`, key).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the ban never queued behind the sign-in's identity lock")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("the ban finished while a sign-in held the identity lock: %v", err)
	default:
	}
	if err = signIn.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the ban did not finish after the sign-in committed")
	}
}
