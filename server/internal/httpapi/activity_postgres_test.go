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
	f.handler = NewRouter(Options{Config: config.Default(), Auth: accounts, Activity: activity.New(store), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
