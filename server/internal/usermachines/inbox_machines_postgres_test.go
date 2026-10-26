package usermachines

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// The workspace Inbox list walks the machines whose Inbox the actor may manage in id order, at most limit at a
// time; another account's machine is never one of them.
func TestInboxMachinesAreBoundedOrderedAndAuthorizedOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	user, other := "inbox_user_"+s, "inbox_other_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, user, other)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities)
SELECT $1||'_m'||g,$1,$1||'_e'||g,'m-'||g,'linux','amd64','/w','online','occupied',true,ARRAY['file_receive'] FROM generate_series(1,5) g`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
VALUES($1,$2,$1,'theirs','linux','amd64','/w','online','occupied',true)`, other+"_m1", other)
	service := New(store, nil, Policy{}, nil)
	personal := workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
	var walked []string
	after := ""
	for page := 0; page < 10; page++ {
		found, more, err := service.InboxMachines(personal, user, after, "", 2)
		if err != nil || len(found) > 2 {
			t.Fatalf("page %d: %d machines, %v", page, len(found), err)
		}
		for _, m := range found {
			walked = append(walked, m.ID)
			if !m.Receives || m.Alias == "" {
				t.Fatalf("machine %+v", m)
			}
		}
		if !more {
			break
		}
		after = found[len(found)-1].ID
	}
	want := []string{user + "_m1", user + "_m2", user + "_m3", user + "_m4", user + "_m5"}
	if fmt.Sprint(walked) != fmt.Sprint(want) {
		t.Fatalf("walked %v, want %v", walked, want)
	}
	if found, _, err := service.InboxMachines(personal, user, "", other+"_m1", 2); err != nil || len(found) != 0 {
		t.Fatalf("another account's machine: %+v, %v", found, err)
	}
}
