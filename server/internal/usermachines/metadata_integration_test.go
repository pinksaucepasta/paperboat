package usermachines

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

func TestMachineMetadataOwnershipAndNamespace(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner := "meta_owner_" + suffix
	other := "meta_other_" + suffix
	team := "meta-team-" + suffix
	for _, id := range []string{owner, other} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, id); err != nil {
			t.Fatal(err)
		}
	}
	authority := teams.NewService(store)
	current, err := authority.Create(ctx, owner, teams.CreateRequest{TeamID: team, OperationID: "create"})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id, user, alias string) {
		t.Helper()
		_, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root) VALUES($1,$2,$1,$3,'linux','amd64','/workspace')`, id, user, alias)
		if err != nil {
			t.Fatal(err)
		}
	}
	a, b := "meta_a_"+suffix, "meta_b_"+suffix
	insert(a, owner, "office")
	insert(b, other, "office")
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	if _, err := service.Rename(ctx, other, a, "stolen"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider metadata update: %v", err)
	}
	got, err := service.Rename(ctx, owner, a, "office")
	if err != nil || got.ID != a || got.Alias != "office" {
		t.Fatalf("metadata update: %+v %v", got, err)
	}
	if _, err := service.Rename(ctx, owner, a, strings.Repeat("a", 64)); !errors.Is(err, ErrInvalidMachineName) {
		t.Fatalf("alias overflow: %v", err)
	}
	current, err = authority.Machine(ctx, owner, team, teams.MachineRequest{OperationID: "transfer-a", ExpectedGeneration: current.Generation, Action: "transfer_to_team", MachineID: a, Confirmation: a})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET owner_team_id=$2 WHERE id=$1`, b, team)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23505" || pgerr.ConstraintName != "user_machines_owner_alias" {
		t.Fatalf("team collision: %v", err)
	}
	// Personal names can coexist with a team name even for the original issuer.
	c := "meta_c_" + suffix
	insert(c, owner, "office")
	if _, err := authority.Machine(ctx, owner, team, teams.MachineRequest{OperationID: "transfer-c", ExpectedGeneration: current.Generation, Action: "transfer_to_team", MachineID: c, Confirmation: c}); !errors.Is(err, teams.ErrMachineAliasConflict) {
		t.Fatalf("transfer alias conflict: %v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET alias='lab',owner_team_id=$2 WHERE id=$1`, b, team); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Rename(ctx, owner, b, "office"); !errors.Is(err, ErrMachineNameConflict) {
		t.Fatalf("team manager alias collision: %v", err)
	}
	// A removed (revoked) team machine frees its alias: the next machine moved into the team may take the name
	// (C209 repeated runs hit machine_name_conflict on the revoked row of the previous run).
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET state='revoked',revoked_at=now() WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Machine(ctx, owner, team, teams.MachineRequest{OperationID: "transfer-c-again", ExpectedGeneration: current.Generation, Action: "transfer_to_team", MachineID: c, Confirmation: c}); err != nil {
		t.Fatalf("transfer after the alias holder was revoked: %v", err)
	}
}
