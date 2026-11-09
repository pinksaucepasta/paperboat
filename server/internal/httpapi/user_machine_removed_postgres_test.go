package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// GET /v1/machines/{id} says 410 user_machine_removed only for a machine removed from the caller's own enrollment.
// Every "not found" cause that may still be a live machine (invisible to the caller, another account's, unknown)
// stays 404 user_machine_not_found, because clients wipe local setup on the removed code alone.
func TestUserMachineGetSeparatesRemovedFromNotFoundPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated VPS PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, other := "rm_owner_"+suffix, "rm_other_"+suffix
	removed, live := "rm_removed_"+suffix, "rm_live_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{owner, other} {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	}
	for _, id := range []string{removed, live} {
		exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, id, owner)
	}
	exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,deleted_at) VALUES($1,$2,$1,'gone','linux','amd64','/workspace','offline','released',now())`, removed, owner)
	exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,'alive','linux','amd64','/workspace','online','occupied')`, live, owner)
	service := usermachines.New(store, audit.NewWriter(store), usermachines.Policy{}, nil)
	get := func(user, machine string) (int, string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/machines/"+machine, nil)
		request.SetPathValue("machine_id", machine)
		request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: user}}))
		recorder := httptest.NewRecorder()
		userMachineGet(service).ServeHTTP(recorder, request)
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return recorder.Code, body.Error.Code
	}
	for step, c := range map[string]struct {
		user, machine, code string
		status              int
	}{
		"removed by its owner":                {owner, removed, "user_machine_removed", http.StatusGone},
		"removed, asked by another":           {other, removed, "user_machine_not_found", http.StatusNotFound},
		"live, asked by one who can't see it": {other, live, "user_machine_not_found", http.StatusNotFound},
		"unknown id":                          {owner, "rm_unknown_" + suffix, "user_machine_not_found", http.StatusNotFound},
	} {
		if status, code := get(c.user, c.machine); status != c.status || code != c.code {
			t.Fatalf("%s: status=%d code=%q, want %d %q", step, status, code, c.status, c.code)
		}
	}
}
