package testfixtures

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestTeamFixtureLifecycleOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, member, stranger := "fx-owner-"+suffix+"@example.test", "fx-member-"+suffix+"@example.test", "fx-stranger-"+suffix+"@example.test"
	for _, email := range []string{owner, member} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,canonical_email,email_verified,display_name,status) VALUES($1,$1,$2,$2,true,'x','active')`, "user_fx_"+strings.Split(email, "@")[0], email); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(Config{DB: store, Audit: audit.NewWriter(store), Token: strings.Repeat("t", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, stranger, nil); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("an account that never signed in: %v", err)
	}
	if _, err := s.CreateTeam(ctx, "not an email", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid email: %v", err)
	}
	team, err := s.CreateTeam(ctx, owner, []string{member})
	if err != nil || team != TeamName(owner) {
		t.Fatalf("create: %q %v", team, err)
	}
	again, err := s.CreateTeam(ctx, strings.ToUpper(owner), []string{member})
	if err != nil || again != team {
		t.Fatalf("create is not idempotent for the same account: %q %v", again, err)
	}
	var paid bool
	var active, owned int
	if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.team_paid_entitled($1), (SELECT count(*) FROM paperboat.team_members WHERE team_id=$1 AND active), (SELECT count(*) FROM paperboat.teams WHERE team_id=$1 AND owner_account='user_fx_fx-owner-`+suffix+`')`, team).Scan(&paid, &active, &owned); err != nil {
		t.Fatal(err)
	}
	if !paid || active != 2 || owned != 1 {
		t.Fatalf("paid=%v active=%d owned=%d, want paid, 2 members, owned by the account", paid, active, owned)
	}
	left, err := s.RemoveTeam(ctx, owner)
	if err != nil || left != 1 {
		t.Fatalf("remove: %d %v, want 1 active member left (the owner)", left, err)
	}
	if left, err = s.RemoveTeam(ctx, owner); err != nil || left != 1 {
		t.Fatalf("remove again: %d %v", left, err)
	}
	var events int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_type='team' AND resource_id=$1 AND actor_type='system' AND event_type IN ('test_fixture_team_created','test_fixture_team_removed')`, team).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 4 {
		t.Fatalf("%d audit events, want 4 (2 creates, 2 removes)", events)
	}
}
