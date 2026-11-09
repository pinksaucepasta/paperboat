package usermachines

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
)

// RemovedBy is the only thing a client may wipe local setup on: true only for a machine removed (soft or final
// deleted) that the caller enrolled. A live machine, another account's machine and an unknown id are all false.
func TestRemovedByAnswersOnlyForTheEnrollingOwnersRemovedMachine(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, other := "rb_owner_"+suffix, "rb_other_"+suffix
	live, removedID, env, liveEnv := "rb_live_"+suffix, "rb_removed_"+suffix, "rb_env_"+suffix, "rb_live_env_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{owner, other} {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	}
	exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, env, owner)
	exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, liveEnv, owner)
	exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'gone','linux','amd64','/workspace','offline','released')`, removedID, owner, env)
	// A live machine the enroller cannot see (for example a team machine after leaving the team) is still live.
	exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'alive','linux','amd64','/workspace','online','occupied')`, live, owner, liveEnv)
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureAccess(&recordingIssuer{}, "https://api.example.test")

	check := func(step, user, machine string, want bool) {
		t.Helper()
		got, err := service.RemovedBy(ctx, user, machine)
		if err != nil || got != want {
			t.Fatalf("%s: RemovedBy(%s,%s)=%v err=%v, want %v", step, user, machine, got, err, want)
		}
	}
	check("live machine", owner, live, false)
	check("unknown id", owner, "rb_unknown_"+suffix, false)
	check("before removal", owner, removedID, false)
	if err := service.Delete(ctx, owner, removedID); err != nil {
		t.Fatal(err)
	}
	check("soft-removed by its owner", owner, removedID, true)
	check("soft-removed, other account", other, removedID, false)
	if err := service.RetryPendingRevocations(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM user_machines WHERE id=$1`, removedID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("machine not final-deleted: rows=%d err=%v", rows, err)
	}
	check("final-deleted, its owner", owner, removedID, true)
	check("final-deleted, other account", other, removedID, false)
	check("live machine after the sweep", owner, live, false)
}
