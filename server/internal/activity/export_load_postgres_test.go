//go:build load

package activity

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// lineCounter is the download's destination: it counts lines without keeping them, and every few thousand writes
// checks that no backend sits idle in a transaction and that the pool still answers a query quickly.
type lineCounter struct {
	t       *testing.T
	check   func() (idle int, latency time.Duration)
	lines   int
	writes  int
	idleMax int
	slowest time.Duration
}

func (c *lineCounter) Write(b []byte) (int, error) {
	for _, x := range b {
		if x == '\n' {
			c.lines++
		}
	}
	if c.writes++; c.writes%2000 == 0 {
		idle, latency := c.check()
		c.idleMax, c.slowest = max(c.idleMax, idle), max(c.slowest, latency)
	}
	return len(b), nil
}

// F21-170: an audit export of the 1,000,000-row rowCap streams for as long as it needs without holding a database
// slot: at no sample is a backend idle in a transaction, and a query from another request answers within a
// second throughout. Tag load (about a minute); an isolated database only.
func TestLoadAuditExportOfTheMillionRowCapOnPostgres(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, team := "exp1m_owner_"+suffix, "exp1m-team-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, owner)
	exec(`INSERT INTO teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Export team')`, team, owner)
	exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, owner)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM audit_events WHERE id LIKE $1`, "exp1m_"+suffix+"_%")
	})
	rowCap := config.Default().Business.AuditExportMaxRows
	seeded := time.Now()
	for from := 1; from <= rowCap+1000; from += 100_000 {
		exec(`INSERT INTO audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,metadata,created_at)
SELECT 'exp1m_'||$2||'_'||g,$1,'user','test.event','team',$3,'{}',now()-g*interval '1 millisecond' FROM generate_series($4::int,least($4::int+99999,$5::int)) g`, owner, suffix, team, from, rowCap+1000)
	}
	exec(`ANALYZE audit_events`)
	t.Logf("seeded %d events in %v", rowCap+1000, time.Since(seeded).Round(time.Second))
	counter := &lineCounter{t: t, check: func() (int, time.Duration) {
		var idle int
		started := time.Now()
		if err := store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state LIKE 'idle in transaction%' AND pid<>pg_backend_pid()`).Scan(&idle); err != nil {
			t.Error(err)
		}
		return idle, time.Since(started)
	}}
	started := time.Now()
	if err := New(store, config.Default().Business).Export(ctx, owner, workspace.Scope{ID: team, TeamID: team}, Query{}, "csv", counter); err != nil {
		t.Fatal(err)
	}
	t.Logf("exported %d lines in %v; idle-in-transaction samples max %d; slowest concurrent query %v", counter.lines, time.Since(started).Round(time.Second), counter.idleMax, counter.slowest.Round(time.Millisecond))
	if counter.lines != rowCap+1 {
		t.Fatalf("export has %d lines, want the rowCap of %d events and a header", counter.lines, rowCap)
	}
	if counter.idleMax != 0 {
		t.Fatalf("a backend sat idle in a transaction during the download (%d)", counter.idleMax)
	}
	if counter.slowest > time.Second {
		t.Fatalf("a query from another request waited %v during the export", counter.slowest)
	}
}
