package workspace

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// Workspace validation runs before nearly every workspace-scoped request, and the team reads behind the console's
// team pages poll. They took the server-wide team lock, so every account's requests queued behind any team change
// or team read in progress (a 780 ms team read at seeded scale). While another transaction holds the lock, each
// read must still answer at once.
func TestWorkspaceAndTeamReadsDoNotWaitForTheTeamLockOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	owner, team := "wl_owner_"+suffix, "wl-team-"+suffix
	execute := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	execute(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@workspace.test','active')`, owner)
	execute(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	execute(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, owner)

	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := teams.Lock(ctx, tx); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("lock holder: %v", err)
		}
	}()

	workspaces, teamService := New(store), teams.NewService(store)
	reads := map[string]func(context.Context) error{
		"workspace.Validate personal": func(ctx context.Context) error {
			return workspaces.Validate(ctx, owner, Scope{ID: "personal"})
		},
		"workspace.Validate team": func(ctx context.Context) error {
			return workspaces.Validate(ctx, owner, Scope{ID: team, TeamID: team})
		},
		"workspace.List": func(ctx context.Context) error { _, err := workspaces.List(ctx, owner); return err },
		"teams.Get":      func(ctx context.Context) error { _, err := teamService.Get(ctx, owner, team); return err },
		"teams.List":     func(ctx context.Context) error { _, err := teamService.List(ctx, owner); return err },
		"teams.Activity": func(ctx context.Context) error { _, err := teamService.Activity(ctx, owner, team, "", 0); return err },
	}
	for name, read := range reads {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		started := time.Now()
		err := read(readCtx)
		cancel()
		if err != nil {
			t.Errorf("%s while another transaction holds the team lock: %v after %s", name, err, time.Since(started).Round(time.Millisecond))
		}
	}
}
