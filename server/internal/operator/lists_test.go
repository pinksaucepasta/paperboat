package operator

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestParseFilterRefusesUnknownKeysAndControlCharacters(t *testing.T) {
	if _, err := parseFilter("q=a&evil=1", "q"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown key err=%v", err)
	}
	if _, err := parseFilter("q=a%0Ab", "q"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("control character err=%v", err)
	}
	if _, err := parseFilter("q="+strings.Repeat("x", 129), "q"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long value err=%v", err)
	}
	if f, err := parseFilter("q=+a+", "q"); err != nil || f["q"] != "a" {
		t.Fatalf("trim f=%v err=%v", f, err)
	}
	if got := contains(`50%_\`); got != `%50\%\_\\%` {
		t.Fatalf("contains escaped wrongly: %q", got)
	}
}

func TestAdminListsSearchTotalsAndAuditSearch(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
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
	t.Cleanup(func() { store.Close() }) // registered first, so it runs after the cleanups that use the store
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	actorID, activeID, bannedID := "usr_adm_op_"+suffix, "usr_adm_a_"+suffix, "usr_adm_b_"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO operator_accounts(id,email,password_hash,status,role) VALUES($1,$2,'unused test login','active','admin')`, actorID, actorID+"@example.test")
	exec(`INSERT INTO users(id,workos_subject,primary_email,display_name,status,role) VALUES($1,$1,$2,'Adm Active','active','user'),($3,$3,$4,'Adm Banned','suspended','user')`, activeID, activeID+"@example.test", bannedID, bannedID+"@example.test")
	exec(`INSERT INTO user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,alias) VALUES($1,$2,$1,'linux','amd64','/workspace','offline','released','adm-box'),($3,$2,$3,'darwin','arm64','/workspace','offline','released','adm-mac')`, "host-adm-l-"+suffix, activeID, "host-adm-m-"+suffix)
	exec(`INSERT INTO audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id) VALUES($1,$2,'admin','adm.test.one','adm_resource',$3),($4,$2,'admin','adm.test.two','adm_resource',$3)`, "aud_adm_1_"+suffix, actorID, suffix, "aud_adm_2_"+suffix)
	t.Cleanup(func() {
		bg := context.Background()
		store.Pool().Exec(bg, `DELETE FROM user_machines WHERE user_id=$1`, activeID)
		store.Pool().Exec(bg, `DELETE FROM users WHERE id IN($1,$2)`, activeID, bannedID)
		store.Pool().Exec(bg, `DELETE FROM operator_accounts WHERE id=$1`, actorID)
	})
	service := NewService(store)
	actor := auth.User{ID: actorID, Role: auth.Role("admin")}
	filter := func(kv ...string) string {
		v := url.Values{}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v.Encode()
	}

	// The overview takes no target: an empty one is valid (it was refused by the generic target check).
	if v, err := service.Read(ctx, actor, "overview", "", "", 10); err != nil || v.(Overview).Hosts < 2 {
		t.Fatalf("overview: %+v err=%v", v, err)
	}
	v, err := service.Read(ctx, actor, "accounts", filter("q", "usr_adm_"+"%"+suffix[:3]), "", 10)
	if err != nil || v.(AccountPage).Total != 0 {
		t.Fatalf("percent in q must be literal: %+v err=%v", v, err)
	}
	v, err = service.Read(ctx, actor, "accounts", filter("q", "_adm_"+"a_"+suffix), "", 10)
	if err != nil || v.(AccountPage).Total != 1 || v.(AccountPage).Accounts[0].ID != activeID {
		t.Fatalf("search by id: %+v err=%v", v, err)
	}
	v, err = service.Read(ctx, actor, "accounts", filter("q", "usr_adm_", "status", "suspended"), "", 10)
	if err != nil || len(v.(AccountPage).Accounts) < 1 {
		t.Fatalf("status filter: %+v err=%v", v, err)
	}
	for _, a := range v.(AccountPage).Accounts {
		if a.Status != "suspended" {
			t.Fatalf("status filter leaked %+v", a)
		}
	}
	if _, err := service.Read(ctx, actor, "accounts", filter("status", "weird"), "", 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status err=%v", err)
	}

	// real total with a one-row page: total stays the full match count and a next cursor exists.
	v, err = service.Read(ctx, actor, "machines", filter("q", activeID), "", 1)
	m := v.(MachinePage)
	if err != nil || m.Total != 2 || len(m.Machines) != 1 || m.NextAfter == "" {
		t.Fatalf("machines page: %+v err=%v", m, err)
	}
	if v, err = service.Read(ctx, actor, "machines", filter("account", activeID), "", 10); err != nil || v.(MachinePage).Total != 2 {
		t.Fatalf("exact owner filter: %+v err=%v", v, err)
	}
	if v, err = service.Read(ctx, actor, "machines", filter("account", activeID[:len(activeID)-1]), "", 10); err != nil || v.(MachinePage).Total != 0 {
		t.Fatalf("an account filter is exact, not a prefix: %+v err=%v", v, err)
	}
	v, err = service.Read(ctx, actor, "machines", filter("q", activeID, "platform", "darwin"), "", 10)
	if err != nil || v.(MachinePage).Total != 1 || v.(MachinePage).Machines[0].Alias != "adm-mac" || v.(MachinePage).Machines[0].AccountID != activeID {
		t.Fatalf("machines platform filter: %+v err=%v", v, err)
	}
	if v, err = service.Read(ctx, actor, "machines", filter("q", activeID, "stale", "true"), "", 10); err != nil || v.(MachinePage).Total != 2 {
		t.Fatalf("never-seen machines are stale: %+v err=%v", v, err)
	}
	machine, err := service.Read(ctx, actor, "machine", "host-adm-l-"+suffix, "", 1)
	if err != nil || machine.(Machine).Alias != "adm-box" || !machine.(Machine).Stale {
		t.Fatalf("machine detail: %+v err=%v", machine, err)
	}
	if _, err := service.Read(ctx, actor, "machine", "host-adm-missing-"+suffix, "", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing machine err=%v", err)
	}

	// audit search: filters, newest first, cursor continues, and role gate.
	v, err = service.Read(ctx, actor, "audit_events", filter("actor", actorID, "resource_type", "adm_resource", "resource_id", suffix), "", 1)
	page := v.(AuditSearchPage)
	if err != nil || page.Total != 2 || len(page.Events) != 1 || page.Events[0].EventType != "adm.test.two" || page.NextAfter == "" {
		t.Fatalf("audit page 1: %+v err=%v", page, err)
	}
	v, err = service.Read(ctx, actor, "audit_events", filter("actor", actorID, "resource_type", "adm_resource", "resource_id", suffix), page.NextAfter, 1)
	if err != nil || len(v.(AuditSearchPage).Events) != 1 || v.(AuditSearchPage).Events[0].EventType != "adm.test.one" || v.(AuditSearchPage).NextAfter != "" {
		t.Fatalf("audit page 2: %+v err=%v", v, err)
	}
	if v, err = service.Read(ctx, actor, "audit_events", filter("event_type", "adm.test.one", "since", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)), "", 5); err != nil || v.(AuditSearchPage).Total != 0 {
		t.Fatalf("since filter: %+v err=%v", v, err)
	}
	if _, err := service.Read(ctx, actor, "audit_events", filter("since", "yesterday"), "", 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad since err=%v", err)
	}
	if _, err := service.Read(ctx, actor, "audit_events", "", "not-a-number", 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad cursor err=%v", err)
	}

	// An operator's live sessions are paged by id; revoked and expired ones are not counted.
	t.Cleanup(func() {
		store.Pool().Exec(context.Background(), `DELETE FROM operator_sessions WHERE operator_id=$1`, actorID)
	})
	for i, state := range []string{"live", "live", "live", "revoked", "expired"} {
		exec(`INSERT INTO operator_sessions(token_hash,operator_id,csrf_hash,expires_at,revoked_at) VALUES(decode(repeat($1,32),'hex'),$2,decode(repeat('00',32),'hex'),CASE WHEN $3='expired' THEN now()-interval '1 hour' ELSE now()+interval '1 hour' END,CASE WHEN $3='revoked' THEN now() END)`, fmt.Sprintf("%02x", i+1), actorID, state)
	}
	sessions, err := service.Read(ctx, actor, "operator_sessions", actorID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessions.(OperatorSessions); got.Total != 3 || len(got.Sessions) != 2 || got.Sessions[0].ID != strings.Repeat("01", 32) || got.NextAfter != strings.Repeat("02", 32) {
		t.Fatalf("first session page = %+v", got)
	}
	rest, err := service.Read(ctx, actor, "operator_sessions", actorID, strings.Repeat("02", 32), 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := rest.(OperatorSessions); len(got.Sessions) != 1 || got.Sessions[0].ID != strings.Repeat("03", 32) || got.NextAfter != "" {
		t.Fatalf("second session page = %+v", got)
	}

	// A team's members are paged with the team: a total, a page and the cursor to the next one.
	teamID := "team_adm_" + suffix
	exec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, teamID, activeID)
	exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, teamID, activeID, bannedID)
	t.Cleanup(func() {
		store.Pool().Exec(context.Background(), `DELETE FROM team_members WHERE team_id=$1`, teamID)
		store.Pool().Exec(context.Background(), `DELETE FROM teams WHERE team_id=$1`, teamID)
	})
	first, err := service.Read(ctx, actor, "team", teamID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if team := first.(Team); team.MembersTotal != 2 || len(team.Members) != 1 || team.Members[0].AccountID != activeID || team.MembersNextAfter != activeID {
		t.Fatalf("first member page = %+v", team)
	}
	second, err := service.Read(ctx, actor, "team", teamID, activeID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if team := second.(Team); team.MembersTotal != 2 || len(team.Members) != 1 || team.Members[0].AccountID != bannedID || team.MembersNextAfter != "" {
		t.Fatalf("second member page = %+v", team)
	}
}

func TestMachinesAndAuditSearchRoles(t *testing.T) {
	for _, c := range []struct {
		role auth.Role
		kind string
		want bool
	}{
		{"support", "machines", true}, {"operations", "machines", true}, {"billing", "machine", true},
		{"support", "audit_events", false}, {"operations", "audit_events", false}, {"admin", "audit_events", true},
	} {
		if got := allowed(c.role, c.kind); got != c.want {
			t.Errorf("allowed(%s,%s)=%v want %v", c.role, c.kind, got, c.want)
		}
	}
}
