package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/activity"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type activityFixture struct {
	t                           *testing.T
	store                       *db.DB
	writer                      *audit.Writer
	handler                     http.Handler
	tokens                      map[string]string
	owner, other, machine, team string
}

func newActivityFixture(t *testing.T) *activityFixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = db.Migrate(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	f := &activityFixture{t: t, store: store, writer: audit.NewWriter(store), tokens: map[string]string{}, owner: "activity_owner_" + suffix, other: "activity_other_" + suffix, machine: "activity_machine_" + suffix, team: "activity-team-" + suffix}
	f.exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, f.owner, f.other)
	f.exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$1,'activity-machine','linux','amd64','/workspace','online','occupied',true,1)`, f.machine, f.owner)
	accounts := auth.NewService(store, f.writer, nil, []string{"test-activity-key"}, false)
	for _, user := range []string{f.owner, f.other} {
		token := "activity-token-" + user
		hash := sha256.Sum256([]byte(token))
		f.tokens[user] = token
		f.exec(`INSERT INTO sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$1,$2,'test',now()+interval '1 day')`, user, hex.EncodeToString(hash[:]))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := store.SQL().ExecContext(ctx, `DELETE FROM sessions WHERE user_id IN ($1,$2)`, f.owner, f.other); err != nil {
			t.Error(err)
		}
	})
	f.handler = NewRouter(Options{Config: config.Default(), Auth: accounts, Activity: activity.New(store, config.Default().Business), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return f
}
func (f *activityFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(f.t.Context(), query, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *activityFixture) write(action, kind, id string) {
	f.t.Helper()
	if err := f.writer.Write(f.t.Context(), audit.Event{ActorUserID: f.owner, ActorType: audit.ActorUser, EventType: action, ResourceType: kind, ResourceID: id, Metadata: map[string]any{"private_value": "PRIVATE_ACTIVITY_CANARY"}}); err != nil {
		f.t.Fatal(err)
	}
}
func (f *activityFixture) get(user, path string) (int, activity.Page, string) {
	f.t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if user != "" {
		r.AddCookie(&http.Cookie{Name: auth.DevSessionCookieName, Value: f.tokens[user]})
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == auth.DevSessionCookieName {
			f.tokens[user] = cookie.Value
		}
	}
	var response struct{ Data activity.Page }
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			f.t.Fatal(err)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			f.t.Fatal("activity cached")
		}
	}
	return w.Code, response.Data, w.Body.String()
}

// One connected owner exercises the real write, immutable snapshot, HTTP
// projection and current authorization; no precomputed activity rows are seeded.
func TestActivityConnectedWorkspaceHistory(t *testing.T) {
	f := newActivityFixture(t)
	f.write("user_machine.renamed", "user_machine", f.machine)
	f.write("machine.setup", "machine", f.machine)
	code, page, body := f.get(f.owner, "/v1/activity?workspace=personal&limit=1")
	if code != 200 || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("normal activity code=%d items=%d cursor=%t", code, len(page.Items), page.NextCursor != "")
	}
	event := page.Items[0]
	if event.Action != "machine.setup" || event.Target.Name != "activity-machine" || event.Machine == nil || event.Actor.ID != f.owner || strings.Contains(body, "PRIVATE_ACTIVITY_CANARY") {
		t.Fatal("incorrect or unsafe summary")
	}
	code, next, _ := f.get(f.owner, "/v1/activity?workspace=personal&limit=1&cursor="+page.NextCursor)
	if code != 200 || len(next.Items) != 1 || next.Items[0].ID == event.ID || next.NextCursor != "" {
		t.Fatal("page lost/repeated event")
	}
	if code, p, _ := f.get(f.other, "/v1/activity?workspace=personal"); code != 200 || len(p.Items) != 0 {
		t.Fatal("foreign personal activity leaked")
	}
	f.exec(`UPDATE user_machines SET user_id=$2,alias='new-owner-name' WHERE id=$1`, f.machine, f.other)
	if code, p, _ := f.get(f.other, "/v1/activity?workspace=personal"); code != 200 || len(p.Items) != 0 {
		t.Fatal("historical scope followed resource transfer")
	}
	if code, p, _ := f.get(f.owner, "/v1/activity?workspace=personal&area=machine&q=activity&machine_id="+f.machine); code != 200 || len(p.Items) != 2 || p.Items[0].Target.Name != "activity-machine" {
		t.Fatal("snapshot/filter changed")
	}
	f.write("auth.login", "user", f.owner)
	if code, p, _ := f.get(f.owner, "/v1/activity?workspace=personal&area=account"); code != 200 || len(p.Items) != 1 || p.Items[0].Action != "auth.login" {
		t.Fatal("account activity filter lost real sign-in action")
	}
	f.exec(`INSERT INTO teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Activity team')`, f.team, f.owner)
	f.exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'admin',true)`, f.team, f.owner, f.other)
	f.write("team.created", "team", f.team)
	if code, p, _ := f.get(f.other, "/v1/activity?workspace="+f.team); code != 200 || len(p.Items) != 1 || p.Items[0].Target.Name != "Activity team" {
		t.Fatal("team activity unavailable")
	}
	f.exec(`UPDATE team_members SET active=false WHERE team_id=$1 AND account_id=$2`, f.team, f.other)
	if code, _, _ := f.get(f.other, "/v1/activity?workspace="+f.team); code != 403 {
		t.Fatal("revoked membership retained access")
	}
	if code, _, _ := f.get("", "/v1/activity?workspace=personal"); code != 401 {
		t.Fatal("anonymous activity allowed")
	}
	for _, q := range []string{"limit=201", "limit=0", "area=unknown", "q=" + strings.Repeat("x", 201), "cursor=invalid", "area=machine&area=team", "unknown=x"} {
		if code, _, _ := f.get(f.owner, "/v1/activity?workspace=personal&"+q); code != 400 {
			t.Fatalf("invalid query accepted: %s", q)
		}
	}
}

func (f *activityFixture) raw(user, path string) (int, string, http.Header) {
	f.t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.AddCookie(&http.Cookie{Name: auth.DevSessionCookieName, Value: f.tokens[user]})
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w.Code, w.Body.String(), w.Header()
}

// PLAN F19-07..09: the audit log carries result, reason and client, filters by them, exports
// as CSV and JSON for team workspaces, records the export, and only audit.read roles see it.
func TestActivityAuditLogFiltersExportAndRoles(t *testing.T) {
	f := newActivityFixture(t)
	f.exec(`INSERT INTO teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Audit team')`, f.team, f.owner)
	f.exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'auditor',true)`, f.team, f.owner, f.other)
	// Written under an API-token session: the event names the token (F19-07 actor.token_id).
	ctx := audit.WithAPIToken(audit.WithClient(t.Context(), audit.Client{IP: "203.0.113.7", UserAgent: "pb/test"}), "apitok_activity_test")
	if err := f.writer.Write(ctx, audit.Event{ActorUserID: f.owner, ActorType: audit.ActorUser, EventType: "team.sso_required", ResourceType: "team", ResourceID: f.team, Result: audit.ResultDenied, Reason: "team_sso_required"}); err != nil {
		t.Fatal(err)
	}
	f.write("team.renamed", "team", f.team)
	code, page, _ := f.get(f.other, "/v1/activity?workspace="+f.team+"&result=denied")
	if code != 200 || len(page.Items) != 1 {
		t.Fatalf("auditor denied filter code=%d items=%d", code, len(page.Items))
	}
	e := page.Items[0]
	if e.Result != "denied" || e.Reason != "team_sso_required" || e.IP != "203.0.113.7" || e.UserAgent != "pb/test" || e.Actor.Email != f.owner+"@example.test" || e.Workspace != "team:"+f.team ||
		e.Actor.Kind != "api_token" || e.Actor.TokenID != "apitok_activity_test" {
		t.Fatalf("event = %+v", e)
	}
	if code, p, _ := f.get(f.other, "/v1/activity?workspace="+f.team+"&action=team&actor="+f.owner+"@example.test&since=2000-01-01T00:00:00Z"); code != 200 || len(p.Items) != 2 {
		t.Fatalf("action prefix + actor email filter code=%d items=%d", code, len(p.Items))
	}
	if code, p, _ := f.get(f.other, "/v1/activity?workspace="+f.team+"&until=2000-01-01T00:00:00Z"); code != 200 || len(p.Items) != 0 {
		t.Fatal("until filter ignored")
	}
	code, body, header := f.raw(f.other, "/v1/activity/export?workspace="+f.team+"&format=csv")
	if code != 200 || !strings.HasPrefix(body, "id,occurred_at,workspace,action,result") || strings.Count(body, "\n") != 3 || !strings.Contains(header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("csv export code=%d body=%q", code, body)
	}
	code, body, _ = f.raw(f.other, "/v1/activity/export?workspace="+f.team+"&format=json&result=denied")
	var exported []activity.Event
	if code != 200 || json.Unmarshal([]byte(body), &exported) != nil || len(exported) != 1 || exported[0].Reason != "team_sso_required" {
		t.Fatalf("json export code=%d body=%q", code, body)
	}
	if code, p, _ := f.get(f.owner, "/v1/activity?workspace="+f.team+"&action=team.activity_exported"); code != 200 || len(p.Items) != 2 || p.Items[0].Details["format"] != "json" {
		t.Fatalf("exports not recorded: code=%d items=%+v", code, p.Items)
	}
	if code, _, _ := f.raw(f.owner, "/v1/activity/export?workspace=personal&format=csv"); code != 400 {
		t.Fatalf("personal export code=%d", code)
	}
	for _, q := range []string{"format=xml", "format=csv&result=maybe", "format=csv&since=yesterday"} {
		if code, _, _ := f.raw(f.other, "/v1/activity/export?workspace="+f.team+"&"+q); code != 400 {
			t.Fatalf("invalid export accepted: %s", q)
		}
	}
	f.exec(`UPDATE team_members SET role='billing' WHERE team_id=$1 AND account_id=$2`, f.team, f.other)
	if code, _, _ := f.get(f.other, "/v1/activity?workspace="+f.team); code != 403 {
		t.Fatalf("billing admin read the audit log: %d", code)
	}
	if code, _, _ := f.raw(f.other, "/v1/activity/export?workspace="+f.team+"&format=csv"); code != 403 {
		t.Fatalf("billing admin exported the audit log: %d", code)
	}
}
