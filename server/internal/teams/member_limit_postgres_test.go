package teams

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// F21-170: a team holds MaximumMembers people; the member that would be one over is refused with ErrLimit by the
// same capacity check Create, Join and Invite use, and the member before that is still admitted.
func TestPostgresTeamRefusesTheMemberOverTheLimit(t *testing.T) {
	f := newPolicyFixture(t)
	capacityFor := func(joiner string) error {
		return f.store.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) error { return capacity(ctx, tx, joiner, f.team) })
	}
	var existing int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.team_members WHERE team_id=$1`, f.team).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	prefix := f.team + "_lim"
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_members WHERE account_id LIKE $1`, prefix+"%")
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id LIKE $1`, prefix+"%")
	})
	fill := func(count int) {
		f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT $1||g,$1||g,$1||g||'@example.test','active' FROM generate_series($2::int,$3::int) g`, prefix, 1, count)
		f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) SELECT $1,$2||g,1,'member',true FROM generate_series(1,$3::int) g`, f.team, prefix, count)
	}
	fill(MaximumMembers - 1 - existing)
	if err := capacityFor("someone-new"); err != nil {
		t.Fatalf("with %d members the next one was refused: %v", MaximumMembers-1, err)
	}
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, prefix+"_last")
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, f.team, prefix+"_last")
	if err := capacityFor("someone-new"); !errors.Is(err, ErrLimit) {
		t.Fatalf("with %d members the next one: %v, want ErrLimit", MaximumMembers, err)
	}
}
