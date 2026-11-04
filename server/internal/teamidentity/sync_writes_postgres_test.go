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
	const teams = 12
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
	// Tick twice: the first records each failure, the second must write nothing.
	_ = s.syncTick(ctx)
	_ = s.syncTick(ctx)
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
	_ = s.syncTick(ctx) // stale teams are marked dirty once
	before := rowVersions()
	_ = s.syncTick(ctx)
	if after := rowVersions(); after != before {
		t.Fatal("a tick with nothing new rewrote team_identity_state rows")
	}
}
