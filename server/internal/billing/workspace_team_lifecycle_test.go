package billing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

func TestTeamDeletionCannotOrphanRenewingSubscription(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner := "usr_bill_delete_" + suffix
	team := "billing-delete-" + suffix
	insertUser(t, store, owner, "delete-"+suffix+"@example.test")
	service := teams.NewService(store)
	created, err := service.Create(ctx, owner, teams.CreateRequest{TeamID: team, OperationID: "create_" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, query := range []string{`DELETE FROM paperboat.billing_workspaces WHERE team_id=$1`, `DELETE FROM paperboat.team_members WHERE team_id=$1`, `DELETE FROM paperboat.teams WHERE team_id=$1`} {
			if _, err := store.SQL().ExecContext(context.Background(), query, team); err != nil {
				t.Error(err)
			}
		}
		// The test database is task-owned; account audit receipts are retained until its cleanup.
	})
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,provider_subscription_id) VALUES($1,$2,'active',$3)`, "team:"+team, team, "sub_"+suffix); err != nil {
		t.Fatal(err)
	}
	_, err = service.Mutate(ctx, owner, team, teams.MutationRequest{OperationID: "delete_" + suffix, ExpectedGeneration: created.Generation, Action: "delete", Confirmation: team})
	if !errors.Is(err, teams.ErrBillingActive) {
		t.Fatalf("delete renewing subscription: %v", err)
	}
	current, err := service.Get(ctx, owner, team)
	if err != nil || current.Deleted {
		t.Fatalf("team changed after denial: %v", err)
	}
}
