package controlplane

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type scopedTestDebiter struct{}

func (scopedTestDebiter) DebitWorkspaceBandwidthTx(ctx context.Context, tx *db.Tx, workspace string, n int64, at time.Time) (int64, bool, error) {
	var exhausted bool
	err := tx.QueryRow(ctx, `UPDATE billing_workspace_usage_periods SET consumed_bytes=consumed_bytes+$2 WHERE workspace_id=$1 AND period_start<=$3 AND period_end>$3 RETURNING coalesce(consumed_bytes>=included_bytes,false)`, workspace, n, at).Scan(&exhausted)
	return n, exhausted, err
}

func TestScopedUsageSharedRouteDelayedCredentialAndReplay(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	seedUsageScope(t, store, suffix)
	now := time.Now().UTC().Truncate(time.Microsecond)
	user := "scope_user_" + suffix
	teams := []string{"scope_a_" + suffix, "scope_b_" + suffix}
	t.Cleanup(func() {
		cleanup := []struct {
			q string
			a []any
		}{
			{`DELETE FROM usage_accounting_authorities WHERE environment_id=$1`, []any{"env_" + suffix}},
			{`DELETE FROM browser_machine_credentials WHERE route_id=$1`, []any{"route_" + suffix}},
			{`DELETE FROM billing_workspace_usage_periods WHERE workspace_id IN ($1,$2)`, []any{"team:" + teams[0], "team:" + teams[1]}},
			{`DELETE FROM billing_workspaces WHERE id IN ($1,$2)`, []any{"team:" + teams[0], "team:" + teams[1]}},
			{`DELETE FROM teams WHERE team_id IN ($1,$2)`, []any{teams[0], teams[1]}},
			{`DELETE FROM users WHERE id=$1`, []any{user}},
		}
		for _, c := range cleanup {
			if _, err := store.SQL().ExecContext(ctx, c.q, c.a...); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	for i, team := range teams {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user); err != nil {
			t.Fatal(err)
		}
		grant := fmt.Sprintf("credential_%d_%s", i, suffix)
		hash := sha256.Sum256([]byte(grant))
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO browser_machine_credentials(credential_id,token_hash,account_id,hostname,resource_kind,resource_id,route_id,action,resource_generation,owner_account_id,access_mode,team_id,team_generation,membership_generation,binding_generation,grant_generation,issued_at,expires_at) VALUES($1,$2,$3,'scope.invalid.test','tunnel','shared-resource',$4,'use',1,$3,'team',$5,1,1,1,1,$6,$7)`, grant, hash[:], user, "route_"+suffix, team, now.Add(-time.Minute), now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		binding := accountingauthority.Binding{ID: "grant:" + grant, Node: "node_" + suffix, Environment: "env_" + suffix, Route: "route_" + suffix, Revision: 1, User: user, Team: team, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)}
		if err := accountingauthority.Record(ctx, store, binding); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes) VALUES($1,$2,$3,100)`, "team:"+team, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		// Model the browser credential cleanup after expiry: its accounting binding
		// deliberately survives this real credential deletion.
		if _, err := store.SQL().ExecContext(ctx, `DELETE FROM browser_machine_credentials WHERE credential_id=$1 AND expires_at<$2`, grant, now.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		report := usageReport(fmt.Sprintf("scope_op_%d_%s", i, suffix), "scope_epoch_"+suffix, 100, now)
		report.AuthorityID, report.EdgeNodeID, report.EnvironmentID, report.RouteID = binding.ID, binding.Node, binding.Environment, binding.Route
		receipt, err := ReconcileScopedUsage(ctx, store, report, now.Add(2*time.Hour), scopedTestDebiter{})
		if err != nil || receipt.DeltaBytes != 100 || !receipt.QuotaExhausted {
			t.Fatalf("delayed team %d receipt=%+v err=%v", i, receipt, err)
		}
		receipt, err = ReconcileScopedUsage(ctx, store, report, now.Add(3*time.Hour), scopedTestDebiter{})
		if err != nil || !receipt.Duplicate || !receipt.QuotaExhausted {
			t.Fatalf("retry=%+v err=%v", receipt, err)
		}
		changed := report
		changed.AuthorityID = "grant:wrong-team"
		if _, err = ReconcileScopedUsage(ctx, store, changed, now.Add(3*time.Hour), scopedTestDebiter{}); !errors.Is(err, ErrUsageOperationConflict) {
			t.Fatalf("replay payer changed: %v", err)
		}
		lower := report
		lower.OperationID += "_lower"
		lower.Bytes = 50
		if r, e := ReconcileScopedUsage(ctx, store, lower, now.Add(3*time.Hour), scopedTestDebiter{}); e != nil || r.DeltaBytes != 0 {
			t.Fatalf("out of order=%+v %v", r, e)
		}
		var consumed int64
		if err = store.SQL().QueryRowContext(ctx, `SELECT consumed_bytes FROM billing_workspace_usage_periods WHERE workspace_id=$1`, "team:"+team).Scan(&consumed); err != nil || consumed != 100 {
			t.Fatalf("team debit=%d err=%v", consumed, err)
		}
		if _, err = store.SQL().ExecContext(ctx, `UPDATE billing_workspace_usage_periods SET included_bytes=consumed_bytes WHERE workspace_id=$1`, "team:"+team); err != nil {
			t.Fatal(err)
		}
		quotaBinding := binding
		quotaBinding.ID += "_quota"
		quotaBinding.IssuedAt = now
		if err = accountingauthority.Record(ctx, store, quotaBinding); !errors.Is(err, accountingauthority.ErrQuotaExhausted) {
			t.Fatalf("exhausted admission=%v", err)
		}
		if _, err = store.SQL().ExecContext(ctx, `UPDATE billing_workspace_usage_periods SET included_bytes=1000 WHERE workspace_id=$1`, "team:"+team); err != nil {
			t.Fatal(err)
		}
		expired := report
		expired.OperationID += "_expired"
		expired.Bytes = 150
		// Mapping may already have been pruned: nonbillable disposition never
		// guesses a payer and needs only the authenticated signed report upstream.
		if _, err = store.SQL().ExecContext(ctx, `DELETE FROM usage_accounting_authorities WHERE authority_id=$1`, binding.ID); err != nil {
			t.Fatal(err)
		}
		late, err := ReconcileScopedUsage(ctx, store, expired, now.Add(accountingauthority.DeliveryWindow+time.Hour), scopedTestDebiter{})
		if err != nil || late.Disposition != "expired_not_billed" || late.DeltaBytes != 50 {
			t.Fatalf("expired disposition=%+v %v", late, err)
		}
		replay, err := ReconcileScopedUsage(ctx, store, expired, now.Add(accountingauthority.DeliveryWindow+2*time.Hour), scopedTestDebiter{})
		if err != nil || !replay.Duplicate || replay.Disposition != late.Disposition {
			t.Fatalf("expired replay=%+v %v", replay, err)
		}
		if err = store.SQL().QueryRowContext(ctx, `SELECT consumed_bytes FROM billing_workspace_usage_periods WHERE workspace_id=$1`, "team:"+team).Scan(&consumed); err != nil || consumed != 100 {
			t.Fatalf("expired report was billed: %d %v", consumed, err)
		}

		if i == len(teams)-1 {
			if _, err = store.SQL().ExecContext(ctx, `INSERT INTO usage_accounting_authorities(authority_id,edge_node_id,environment_id,route_id,route_revision,workspace_id,admitted_at,admission_expires_at,retain_until) SELECT 'capacity:'||n,$1,$2,$3,1,$4,$5,$6,$7 FROM generate_series(1,$8) n`, binding.Node, binding.Environment, binding.Route, "team:"+team, now, now.Add(time.Minute), now.Add(2*accountingauthority.DeliveryWindow+time.Minute), accountingauthority.MaximumPerNode); err != nil {
				t.Fatal(err)
			}
			capped := binding
			capped.ID = "grant:new-at-capacity"
			if err = accountingauthority.Record(ctx, store, capped); !errors.Is(err, accountingauthority.ErrUnavailable) {
				t.Fatalf("unbounded admission: %v", err)
			}
			capped.IssuedAt = now.Add(2*accountingauthority.DeliveryWindow + 2*time.Minute)
			capped.ExpiresAt = capped.IssuedAt.Add(time.Minute)
			if err = accountingauthority.Record(ctx, store, capped); err != nil {
				t.Fatalf("expired capacity did not recover: %v", err)
			}
		}

	}
}
