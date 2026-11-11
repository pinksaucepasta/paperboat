package tunnelv1

import (
	"context"
	"errors"
	"testing"
)

// A machine moved into a team (pb team machine transfer-to-team) belongs to the team: its owner can no longer create
// tunnels from it until a team machine grant allows them, and the refusal says which team owns it. Moved back, it works.
func TestTunnelCreateFromATeamOwnedMachineNamesTheTeamOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account, host := f.accounts[0], "mch_trk07_primary_"+f.suffix
	team := "team_owned_" + f.suffix
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, account); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET owner_team_id=$1 WHERE id=$2`, team, host); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = f.database.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET owner_team_id=NULL WHERE id=$1`, host)
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
	}()
	err := f.repository.VerifyHost(ctx, account, host)
	var owned *HostTeamOwnedError
	if !errors.As(err, &owned) || owned.Team != team || !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("a team-owned machine verified as %v, want HostTeamOwnedError for %s", err, team)
	}
	if other := f.repository.VerifyHost(ctx, account, "mch_no_such_machine"); !errors.Is(other, ErrHostNotFound) || errors.As(other, &owned) && owned.Team != team {
		t.Fatalf("an unknown machine verified as %v", other)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET owner_team_id=NULL WHERE id=$1`, host); err != nil {
		t.Fatal(err)
	}
	if err := f.repository.VerifyHost(ctx, account, host); err != nil {
		t.Fatalf("the machine moved back to its owner is refused: %v", err)
	}
}
