package activity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// transactionProbe is the download's destination: every write checks that no backend of the test database
// sits idle in a transaction, which is what an export holding one transaction for the whole download shows.
type transactionProbe struct {
	store  *db.DB
	t      *testing.T
	bytes  strings.Builder
	writes int
	open   int
}

func (p *transactionProbe) Write(b []byte) (int, error) {
	p.writes++
	var idle int
	if err := p.store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state LIKE 'idle in transaction%' AND pid<>pg_backend_pid()`).Scan(&idle); err != nil {
		p.t.Fatal(err)
	}
	if idle > p.open {
		p.open = idle
	}
	return p.bytes.Write(b)
}

// F21-41: a long export reads its events in short per-page transactions, so a slow download pins no pool
// slot or snapshot.
func TestPostgresActivityExportPagesInShortTransactions(t *testing.T) {
	store := openTestStore(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, team := "exp_owner_"+suffix, "exp-team-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, owner)
	exec(`INSERT INTO teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Export team')`, team, owner)
	exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, owner)
	const events = 1200
	exec(`INSERT INTO audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,metadata,created_at)
SELECT 'exp_'||$2||'_'||g,$1,'user','test.event','team',$3,'{}',now()-g*interval '1 second' FROM generate_series(1,$4::int) g`, owner, suffix, team, events)
	service := New(store, config.Default().Business)
	probe := &transactionProbe{store: store, t: t}
	if err := service.Export(ctx, owner, workspace.Scope{ID: team, TeamID: team}, Query{}, "csv", probe); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(probe.bytes.String(), "\n"); got != events+1 {
		t.Fatalf("export has %d lines, want %d events and a header", got, events+1)
	}
	if probe.open != 0 {
		t.Fatalf("a backend sat idle in a transaction during the download (%d)", probe.open)
	}
}

// revokingWriter ends the exporter's membership on its first write, which happens while page 1 is being written.
type revokingWriter struct {
	revoke func()
	once   sync.Once
	bytes  strings.Builder
}

func (r *revokingWriter) Write(b []byte) (int, error) {
	r.once.Do(r.revoke)
	return r.bytes.Write(b)
}

// An export authorizes every page: a member who loses membership mid-export gets no further pages.
func TestPostgresActivityExportStopsWhenMembershipEnds(t *testing.T) {
	store := openTestStore(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, member, team := "expr_owner_"+suffix, "expr_member_"+suffix, "expr-team-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, owner, member)
	exec(`INSERT INTO teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Export team')`, team, owner)
	exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, member)
	exec(`INSERT INTO audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,metadata,created_at)
SELECT 'expr_'||$2||'_'||g,$1,'user','test.event','team',$3,'{}',now()-g*interval '1 second' FROM generate_series(1,1200) g`, owner, suffix, team)
	service := New(store, config.Default().Business)
	w := &revokingWriter{revoke: func() {
		exec(`UPDATE team_members SET active=false WHERE team_id=$1 AND account_id=$2`, team, owner)
	}}
	err := service.Export(ctx, owner, workspace.Scope{ID: team, TeamID: team}, Query{}, "csv", w)
	if !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("export after the membership ended = %v, want a refusal", err)
	}
	if lines := strings.Count(w.bytes.String(), "\n"); lines > exportPageRows+1 {
		t.Fatalf("%d lines written; pages after the membership ended must not be read", lines)
	}
}
