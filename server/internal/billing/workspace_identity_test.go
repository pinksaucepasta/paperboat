package billing_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

func TestWorkspaceIdentityPreservesExistingOwnerRowOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, member, outsider := "usr_workspace_owner_"+suffix, "usr_workspace_member_"+suffix, "usr_workspace_outsider_"+suffix
	team := "workspace-identity-" + suffix
	for _, user := range []string{owner, member, outsider} {
		insertUser(t, store, user, user+"@example.test")
	}
	if _, err := teams.NewService(store).Create(ctx, owner, teams.CreateRequest{OperationID: "create_workspace_" + suffix, TeamID: team}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, member); err != nil {
		t.Fatal(err)
	}
	teamRow, personalRow := "existing-team-workspace-"+suffix, "existing-personal-workspace-"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,user_id,state,product_code,quantity,revision) VALUES($1,$2,NULL,'active','existing-product',2,7),($3,NULL,$4,'active','personal-product',1,9)`, teamRow, team, personalRow, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$2)`, "workspace-edge-"+suffix, "workspace-epoch-"+suffix); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM paperboat.usage_accounting_authorities WHERE workspace_id IN ($1,$2)`, []any{teamRow, personalRow}},
			{`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, []any{"workspace-edge-" + suffix}},
			{`DELETE FROM paperboat.billing_workspaces WHERE team_id=$1 OR user_id IN ($2,$3,$4)`, []any{team, owner, member, outsider}},
			{`DELETE FROM paperboat.team_members WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.users WHERE id IN ($1,$2,$3)`, []any{owner, member, outsider}},
		} {
			if _, err := store.SQL().ExecContext(context.Background(), statement.query, statement.args...); err != nil {
				t.Error(err)
			}
		}
	})
	resolve := func(account, selected string, manage bool) (billing.Workspace, error) {
		var value billing.Workspace
		err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var err error
			value, err = billing.WorkspaceTx(ctx, tx, account, selected, manage)
			return err
		})
		return value, err
	}
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, account := range []string{owner, member} {
		wg.Add(1)
		go func(account string) {
			defer wg.Done()
			value, err := resolve(account, team, false)
			if err != nil {
				failures <- err
				return
			}
			if value.ID != teamRow || value.ProductCode != "existing-product" || value.Quantity != 2 || value.Revision != 7 || value.TeamID != team {
				failures <- fmt.Errorf("team workspace entitlement or identity changed: %+v", value)
			}
		}(account)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	personal, err := resolve(owner, "", true)
	if err != nil || personal.ID != personalRow || personal.ProductCode != "personal-product" || personal.Revision != 9 {
		t.Fatalf("personal workspace=%+v err=%v", personal, err)
	}
	if _, err := resolve(outsider, team, false); !errors.Is(err, billing.ErrWorkspaceForbidden) {
		t.Fatalf("nonmember workspace access=%v", err)
	}
	if _, err := resolve(member, team, true); !errors.Is(err, billing.ErrWorkspaceForbidden) {
		t.Fatalf("member billing management=%v", err)
	}
	var count int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.billing_workspaces WHERE team_id=$1`, team).Scan(&count); err != nil || count != 1 {
		t.Fatalf("team workspace count=%d err=%v", count, err)
	}
	binding := accountingauthority.Binding{ID: "workspace-authority-" + suffix, Node: "workspace-edge-" + suffix, Environment: "workspace-env-" + suffix, Route: "workspace-route-" + suffix, User: owner, Team: team, Revision: 1, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}
	if err := accountingauthority.Record(ctx, store, binding); err != nil {
		t.Fatalf("record existing team payer authority: %v", err)
	}
	var payer string
	if err := store.SQL().QueryRowContext(ctx, `SELECT workspace_id FROM paperboat.usage_accounting_authorities WHERE authority_id=$1`, binding.ID).Scan(&payer); err != nil || payer != teamRow {
		t.Fatalf("retained team payer=%s err=%v", payer, err)
	}
	binding.Team = ""
	if err := accountingauthority.Record(ctx, store, binding); !errors.Is(err, accountingauthority.ErrUnavailable) {
		t.Fatalf("retained authority payer changed: %v", err)
	}
	binding.ID = "personal-authority-" + suffix
	if err := accountingauthority.Record(ctx, store, binding); err != nil {
		t.Fatalf("record existing personal payer authority: %v", err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT workspace_id FROM paperboat.usage_accounting_authorities WHERE authority_id=$1`, binding.ID).Scan(&payer); err != nil || payer != personalRow {
		t.Fatalf("retained personal payer=%s err=%v", payer, err)
	}

}
