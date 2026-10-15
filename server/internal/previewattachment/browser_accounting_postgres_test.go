package previewattachment_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat-server/internal/browseringress"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
)

func assertTeamBrowserPreviewAccounting(t *testing.T, database *db.DB, f previewattachment.LazyActivationFixture, repo *previewattachment.SQLRepository, access *browseraccess.Service, now time.Time) {
	t.Helper()
	ctx := t.Context()
	team, member, workspace := seedBrowserAccountingTeam(t, database, f, now)
	attachment := f.Attachment(f.PreviewID(1), f.OperationID(1), f.OwnerSessionID)
	attachment.Endpoint = fmt.Sprintf("https://carrier-epoch-%s-b.preview.example.test", f.Suffix)
	attachment.AccessMode = "team"
	if _, err := repo.CreatePending(ctx, attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE preview_lease_carrier_attachments SET state='ready',edge_ready=true,origin_ready=true,ready_at=now() WHERE account_id=$1 AND preview_id=$2`, f.AccountID, attachment.PreviewID); err != nil {
		t.Fatal(err)
	}
	session := "ses_team_browser_" + f.Suffix
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, session, member, "fixture_hash_team_"+f.Suffix, "csrf_team_"+f.Suffix, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(attachment.Endpoint, "https://")
	begin, err := access.Begin(ctx, browseraccess.BeginRequest{Host: host, ReturnPath: "/app"})
	if err != nil {
		t.Fatal(err)
	}
	issue, err := access.Issue(ctx, browseraccess.IssueRequest{TransactionID: begin.TransactionID, Principal: auth.Session{ID: session, UserID: member}})
	if err != nil {
		t.Fatal(err)
	}
	login, err := access.Redeem(ctx, browseraccess.RedeemRequest{TransactionID: begin.TransactionID, State: begin.State, Handoff: issue.Handoff, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := access.Authorize(ctx, browseraccess.AuthorizeRequest{Token: login.Token, Host: host, ResourceKind: "preview", ResourceID: attachment.PreviewID, RouteID: attachment.RouteID})
	if err != nil {
		t.Fatal(err)
	}
	if authority.AccountID != member || authority.OwnerAccountID != f.AccountID || authority.TeamID != team || authority.MembershipGeneration < 1 {
		t.Fatal("fixture did not authorize a distinct current team member")
	}
	decision, err := (&browseringress.Service{DB: database, Access: access}).Resolve(ctx, authority, f.NodeID, f.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	var actor, payer string
	if err := database.SQL().QueryRowContext(ctx, `SELECT actor_account_id,workspace_id FROM usage_accounting_authorities WHERE authority_id=$1 AND edge_node_id=$2`, "grant:"+decision.GrantID, f.NodeID).Scan(&actor, &payer); err != nil {
		t.Fatal(err)
	}
	if actor != member || payer != workspace {
		t.Fatal("browser usage was not bound to the authenticated team member and team workspace")
	}
}

func seedBrowserAccountingTeam(t *testing.T, database *db.DB, f previewattachment.LazyActivationFixture, now time.Time) (team, member, workspace string) {
	t.Helper()
	team, member = f.SecondPreviewTeam, "usr_browser_member_"+f.Suffix
	workspace = "bw_" + team
	ctx := t.Context()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM usage_accounting_authorities WHERE workspace_id=$1`, []any{workspace}},
			{`DELETE FROM preview_leases WHERE id=$1`, []any{f.PreviewID(1)}},
			{`DELETE FROM billing_workspaces WHERE team_id=$1`, []any{team}},
			{`DELETE FROM team_resource_grants WHERE team_id=$1`, []any{team}},
			{`DELETE FROM team_resource_bindings WHERE team_id=$1`, []any{team}},
			{`DELETE FROM team_members WHERE team_id=$1`, []any{team}},
			{`DELETE FROM browser_machine_credentials WHERE team_id=$1`, []any{team}},
			{`DELETE FROM browser_access_sessions WHERE team_id=$1`, []any{team}},
			{`DELETE FROM browser_access_transactions WHERE team_id=$1`, []any{team}},
			{`DELETE FROM teams WHERE team_id=$1`, []any{team}},
			{`DELETE FROM sessions WHERE user_id=$1`, []any{member}},
			{`DELETE FROM users WHERE id=$1`, []any{member}},
		} {
			if _, err := database.SQL().ExecContext(cleanup, statement.query, statement.args...); err != nil {
				t.Error(err)
			}
		}
	})
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, []any{member}},
		{`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, []any{team, f.AccountID, member}},
		{`INSERT INTO team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'preview',$2,$3)`, []any{team, f.PreviewID(1), f.AccountID}},
		{`INSERT INTO team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,'preview',$3,'use',1,true)`, []any{team, member, f.PreviewID(1)}},
		{`INSERT INTO billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,$3,$4)`, []any{workspace, team, now.Add(-time.Hour), now.Add(time.Hour)}},
		{`UPDATE preview_leases SET access_mode='team' WHERE id=$1`, []any{f.PreviewID(1)}},
	} {
		if _, err := database.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return team, member, workspace
}
