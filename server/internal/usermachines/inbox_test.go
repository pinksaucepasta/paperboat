package usermachines

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestInboxObservationValidation(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	valid := InboxObservation{ObservedAt: now, Total: 2, Items: []InboxItem{{ID: "a", Name: "a.txt", Size: 1, ReceivedAt: now}, {ID: "b", Name: "b.txt", Size: 0, ReceivedAt: now}}}
	if err := valid.Validate(now); err != nil {
		t.Fatalf("valid observation rejected: %v", err)
	}
	for name, mutate := range map[string]func(*InboxObservation){
		"no time":         func(o *InboxObservation) { o.ObservedAt = time.Time{} },
		"future":          func(o *InboxObservation) { o.ObservedAt = now.Add(time.Hour) },
		"total below":     func(o *InboxObservation) { o.Total = 1 },
		"too many":        func(o *InboxObservation) { o.Items = make([]InboxItem, MaxInboxItems+1); o.Total = MaxInboxItems + 1 },
		"duplicate id":    func(o *InboxObservation) { o.Items[1].ID = "a" },
		"path separator":  func(o *InboxObservation) { o.Items[0].Name = "../x" },
		"empty name":      func(o *InboxObservation) { o.Items[0].Name = " " },
		"negative size":   func(o *InboxObservation) { o.Items[0].Size = -1 },
		"no received":     func(o *InboxObservation) { o.Items[0].ReceivedAt = time.Time{} },
		"control in name": func(o *InboxObservation) { o.Items[0].Name = "a\nb" },
	} {
		candidate := valid
		candidate.Items = append([]InboxItem(nil), valid.Items...)
		mutate(&candidate)
		if !errors.Is(candidate.Validate(now), ErrInboxObservationInvalid) {
			t.Errorf("%s: want ErrInboxObservationInvalid", name)
		}
	}
}

func TestInboxListingAuthorityAndReplacementOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	prefix := fmt.Sprintf("inbox_%d_", time.Now().UnixNano())
	owner, other, admin, member, team := prefix+"owner", prefix+"other", prefix+"admin", prefix+"member", prefix+"team"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, command := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM paperboat.user_machines WHERE user_id IN($1,$2,$3,$4)`, []any{owner, other, admin, member}},
			{`DELETE FROM paperboat.team_members WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.users WHERE id IN($1,$2,$3,$4)`, []any{owner, other, admin, member}},
		} {
			if _, err := store.SQL().ExecContext(context.Background(), command.query, command.args...); err != nil {
				t.Errorf("inbox fixture cleanup: %v", err)
			}
		}
	})
	for _, actor := range []string{owner, other, admin, member} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, actor, actor+"@example.test")
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'admin',true),($1,$4,1,'member',true)`, team, owner, admin, member)
	personal, shared := prefix+"personal", prefix+"shared"
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,description,platform,architecture,workspace_root,state,seat_state,owner_team_id) VALUES
 ($1,$3,$1||'_env','inbox-personal','','linux','amd64','/workspace','online','occupied',NULL),
 ($2,$3,$2||'_env','inbox-shared','','linux','amd64','/workspace','online','occupied',$4)`, personal, shared, owner, team)

	service := New(store, nil, Policy{}, nil)
	empty, err := service.ListInbox(ctx, owner, personal)
	if err != nil || empty.ObservedAt != nil || empty.Total != 0 || len(empty.Items) != 0 {
		t.Fatalf("before any report: %+v %v", empty, err)
	}
	now := service.now().UTC().Truncate(time.Millisecond)
	first := InboxObservation{ObservedAt: now, Total: 3, Items: []InboxItem{{ID: "i1", Name: "new.txt", Size: 12, ReceivedAt: now}, {ID: "i2", Name: "old.txt", Size: 5, ReceivedAt: now.Add(-time.Hour)}}}
	if err := service.RecordInboxObservation(ctx, personal+"_env", personal, first); err != nil {
		t.Fatal(err)
	}
	got, err := service.ListInbox(ctx, owner, personal)
	if err != nil || got.ObservedAt == nil || !got.ObservedAt.Equal(now) || got.Total != 3 || len(got.Items) != 2 || got.Items[0].Name != "new.txt" || got.Items[0].Size != 12 || got.Items[1].ID != "i2" {
		t.Fatalf("listing = %+v %v", got, err)
	}
	if err := service.RecordInboxObservation(ctx, personal+"_env", personal, InboxObservation{ObservedAt: now.Add(-time.Minute), Total: 0}); !errors.Is(err, ErrInboxObservationStale) {
		t.Fatalf("older observation: %v", err)
	}
	if err := service.RecordInboxObservation(ctx, personal+"_env", shared, first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("environment of another machine: %v", err)
	}
	if err := service.RecordInboxObservation(ctx, personal+"_env", personal, InboxObservation{ObservedAt: now.Add(time.Minute), Total: 0}); err != nil {
		t.Fatal(err)
	}
	got, err = service.ListInbox(ctx, owner, personal)
	if err != nil || got.Total != 0 || len(got.Items) != 0 {
		t.Fatalf("replacement listing = %+v %v", got, err)
	}
	// File names are private: strangers, and team members who do not manage the machine, get 404.
	if _, err := service.ListInbox(ctx, other, personal); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger: %v", err)
	}
	if err := service.RecordInboxObservation(ctx, shared+"_env", shared, first); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListInbox(ctx, member, shared); !errors.Is(err, ErrNotFound) {
		t.Fatalf("team member without management: %v", err)
	}
	for _, manager := range []string{owner, admin} {
		if listing, err := service.ListInbox(ctx, manager, shared); err != nil || len(listing.Items) != 2 {
			t.Fatalf("team manager %s: %+v %v", manager, listing, err)
		}
	}
	if _, err := service.ListInbox(ctx, owner, prefix+"missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing machine: %v", err)
	}
}
