package teams

import (
	"context"
	"sync/atomic"
	"testing"
)

// F21-42: the team list reads the same number of statements whether the team has 4 members or 1,000.
func TestPostgresTeamListStatementsDoNotGrowWithMembers(t *testing.T) {
	f := newPolicyFixture(t)
	var statements atomic.Int64
	f.store.SetQueryObserver(func(ctx context.Context) (context.Context, func(string)) {
		statements.Add(1)
		return ctx, func(string) {}
	})
	count := func() (int64, int) {
		statements.Store(0)
		out, err := f.service.List(f.ctx, f.owner)
		if err != nil {
			f.t.Fatal(err)
		}
		return statements.Load(), len(out[0].Members)
	}
	small, _ := count()
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT $1||g,$1||g,$1||g||'@example.test','active' FROM generate_series(1,1000) g`, f.team+"_m")
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) SELECT $1,$2||g,1,'member',true FROM generate_series(1,1000) g`, f.team, f.team+"_m")
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_members WHERE account_id LIKE $1`, f.team+"_m%")
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id LIKE $1`, f.team+"_m%")
	})
	large, members := count()
	if members < 1000 || large != small {
		t.Fatalf("team list with %d members ran %d statements, with 4 members %d", members, large, small)
	}
}
