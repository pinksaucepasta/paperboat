package teamidentity

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// eventsWorkOS reports one event per organization and fails every organization read.
type eventsWorkOS struct {
	fakeWorkOS
	orgs []string
}

func (e *eventsWorkOS) Events(context.Context, string, int) ([]Event, string, error) {
	var out []Event
	for _, o := range e.orgs {
		out = append(out, Event{ID: "ev_" + o, OrganizationID: o})
	}
	return out, "", nil
}

func (e *eventsWorkOS) Organization(context.Context, string) (OrganizationState, error) {
	return OrganizationState{}, errors.New("workos down")
}

// F21-44: events mark their teams dirty in one statement, and a tick with nothing new (stale teams whose
// reads keep failing) writes no row.
func TestPostgresIdentitySyncWritesOncePerChange(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	const teams = 5
	w := &eventsWorkOS{}
	for i := 0; i < teams; i++ {
		team, org := fmt.Sprintf("sw-%s-%d", x, i), fmt.Sprintf("org_sw_%s_%d", x, i)
		w.orgs = append(w.orgs, org)
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, "swo_"+team); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation,display_name,workos_organization_id) VALUES($1,$2,1,'Sync team',$3)`, team, "swo_"+team, org); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_identity_state(team_id,synced_at,dirty) VALUES($1,now(),false)`, team); err != nil {
			t.Fatal(err)
		}
	}
	s := New(store, w, config.Default().Business, config.Default().Mail.InvitationLifetimeSeconds, "https://console.example.test/team", &fakeSeats{})
	if err := s.readEvents(ctx); err != nil {
		t.Fatal(err)
	}
	var distinct, dirty int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(DISTINCT xmin::text), count(*) FILTER (WHERE dirty) FROM paperboat.team_identity_state WHERE team_id LIKE $1`, "sw-"+x+"-%").Scan(&distinct, &dirty); err != nil {
		t.Fatal(err)
	}
	if dirty != teams || distinct != 1 {
		t.Fatalf("%d teams dirty written by %d statements, want %d in 1", dirty, distinct, teams)
	}
	// One tick (up to 5 teams) records each team's failure once.
	for range 2 {
		_ = s.syncTick(ctx)
	}
	rowVersions := func() string {
		var v string
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(string_agg(team_id||xmin::text, ',' ORDER BY team_id),'') FROM paperboat.team_identity_state WHERE team_id LIKE $1`, "sw-"+x+"-%").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.team_identity_state SET synced_at=now()-interval '2 days' WHERE team_id LIKE $1`, "sw-"+x+"-%"); err != nil {
		t.Fatal(err)
	}
	_ = s.syncTick(ctx) // stale teams are marked dirty once, their failures are unchanged
	before := rowVersions()
	_ = s.syncTick(ctx)
	if after := rowVersions(); after != before {
		t.Fatal("a tick with nothing new rewrote team_identity_state rows")
	}
}

// failingOrgsWorkOS fails the organization read of the listed organizations only.
type failingOrgsWorkOS struct {
	fakeWorkOS
	failing map[string]bool
}

func (f *failingOrgsWorkOS) Organization(ctx context.Context, id string) (OrganizationState, error) {
	if f.failing[id] {
		return OrganizationState{}, errors.New("workos down")
	}
	return f.fakeWorkOS.Organization(ctx, id)
}

// F21-42: seven teams whose WorkOS read keeps failing do not hold the five slots of a tick: the eighth, healthy
// team is reconciled by the second tick.
func TestPostgresFailingTeamsDoNotStarveHealthyOnes(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	// Teams left dirty by other tests of the shared test database would take batch slots.
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.team_identity_state SET dirty=false`); err != nil {
		t.Fatal(err)
	}
	w := &failingOrgsWorkOS{failing: map[string]bool{}}
	healthy := "sh-" + x + "-ok"
	for i, team := range []string{"sh-" + x + "-0", "sh-" + x + "-1", "sh-" + x + "-2", "sh-" + x + "-3", "sh-" + x + "-4", "sh-" + x + "-5", "sh-" + x + "-6", healthy} {
		org := "org_" + team
		if i < 7 {
			w.failing[org] = true
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, "swo_"+team); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation,display_name,workos_organization_id) VALUES($1,$2,1,'Sync team',$3)`, team, "swo_"+team, org); err != nil {
			t.Fatal(err)
		}
		// The failing teams were never synced; the healthy one synced an hour ago, so it sorts last.
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_identity_state(team_id,synced_at,dirty) VALUES($1,CASE WHEN $2 THEN now()-interval '1 hour' END,true)`, team, i == 7); err != nil {
			t.Fatal(err)
		}
	}
	s := New(store, w, config.Default().Business, config.Default().Mail.InvitationLifetimeSeconds, "https://console.example.test/team", &fakeSeats{})
	for range 2 {
		_ = s.syncTick(ctx)
	}
	var dirty bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT dirty FROM paperboat.team_identity_state WHERE team_id=$1`, healthy).Scan(&dirty); err != nil || dirty {
		t.Fatalf("healthy team still dirty after two ticks (err %v): failing teams hold the batch", err)
	}
}
