package previewtunnelstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// A team's binding cap counts resources it still holds. Ended terminal sessions (inactive) and ended
// previews (binding left active) must not block a new preview; a full team gets a typed limit error.
func TestWorkspaceBindingCapCountsOnlyHeldResources(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, team := "cap_owner_"+suffix, "cap-team-"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.Pool().Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, q := range []string{`DELETE FROM paperboat.team_resource_grants WHERE team_id=$1`, `DELETE FROM paperboat.team_resource_bindings WHERE team_id=$1`, `DELETE FROM paperboat.team_members WHERE team_id=$1`, `DELETE FROM paperboat.teams WHERE team_id=$1`} {
			_, _ = database.Pool().Exec(context.Background(), q, team)
		}
	}()
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, owner)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
		SELECT $1,'terminal_session','ended_'||g,$2,false,1 FROM generate_series(1,$3::int) g`, team, owner, teams.MaximumResources)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
		SELECT $1,'preview','gone_'||g,$2,true,1 FROM generate_series(1,$3::int) g`, team, owner, teams.MaximumResources)
	bind := func(id string) error {
		return database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			return BindWorkspacePublisherTx(ctx, tx, team, "preview", id, owner)
		})
	}
	if err := bind("prv_cap_ok_" + suffix); err != nil {
		t.Fatalf("a team holding only ended resources cannot publish: %v", err)
	}
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
		SELECT $1,'terminal_session','live_'||g,$2,true,1 FROM generate_series(1,$3::int) g`, team, owner, teams.MaximumResources)
	var limit *teams.LimitError
	if err := bind("prv_cap_full_" + suffix); !errors.As(err, &limit) || limit.Limit != teams.MaximumResources {
		t.Fatalf("a full team must get a typed limit error, got %v", err)
	}
}
