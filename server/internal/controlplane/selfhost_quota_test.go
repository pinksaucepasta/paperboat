package controlplane

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
)

func TestSelfhostAdmissionIgnoresHostedBandwidthExhaustion(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	seedUsageScope(t, store, suffix)
	user, team := "quota_user_"+suffix, "quota_team_"+suffix
	node, install := "node_"+suffix, "quota_install_"+suffix
	runtimeHash := sha256.Sum256([]byte(install))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM usage_accounting_authorities WHERE workspace_id IN ('personal:'||$1,'team:'||$2)`,
			`DELETE FROM billing_workspace_usage_periods WHERE workspace_id IN ('personal:'||$1,'team:'||$2)`,
			`DELETE FROM billing_workspaces WHERE id IN ('personal:'||$1,'team:'||$2)`,
		} {
			store.SQL().ExecContext(ctx, q, user, team)
		}
		store.SQL().ExecContext(ctx, `DELETE FROM selfhost_installations WHERE id=$1`, install)
		store.SQL().ExecContext(ctx, `DELETE FROM teams WHERE team_id=$1`, team)
		store.SQL().ExecContext(ctx, `DELETE FROM users WHERE id=$1`, user)
	})
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, user)
	exec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$5,'ciphertext','quota fixture','tunnel','account',$4,$1,true)`, install, node, make([]byte, 32), user, runtimeHash[:])
	policy := billing.NewWorkspaceService(store, nil, "")
	zero := int64(0)
	if err := policy.InitializeAllowancePolicy(ctx, &zero, &zero); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	b := accountingauthority.Binding{ID: "quota:" + suffix, Node: node, Environment: "env_" + suffix, Route: "route_" + suffix, Revision: 1, User: user, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	for _, payer := range []string{"personal", "team"} {
		b.Team = ""
		workspace := "personal:" + user
		if payer == "team" {
			b.Team = team
			workspace = "team:" + team
		}
		if b.Team == "" {
			exec(`INSERT INTO billing_workspaces(id,user_id) VALUES($1,$2)`, workspace, user)
		} else {
			exec(`INSERT INTO billing_workspaces(id,team_id) VALUES($1,$2)`, workspace, team)
		}
		exec(`INSERT INTO billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,consumed_bytes,observed_bytes) VALUES($1,$2,$3,100,100,100)`, workspace, now.Add(-time.Hour), now.Add(time.Hour))
		for _, ownership := range []string{"account", "team", "global"} {
			scopeID := user
			if ownership == "team" {
				scopeID = team
			}
			if ownership == "global" {
				scopeID = "global"
			}
			exec(`UPDATE selfhost_installations SET scope_kind=$2,scope_id=$3 WHERE id=$1`, install, ownership, scopeID)
			b.ID = "quota:" + suffix + ":" + payer + ":" + ownership
			for _, withPolicy := range []bool{false, true} {
				var err error
				if withPolicy {
					err = accountingauthority.Record(ctx, store, b, policy)
				} else {
					err = accountingauthority.Record(ctx, store, b)
				}
				if ownership == "global" {
					if !errors.Is(err, accountingauthority.ErrQuotaExhausted) {
						t.Fatalf("hosted/global quota policy=%v: %v", withPolicy, err)
					}
				} else if err != nil {
					t.Fatalf("selfhost %s payer %s policy=%v: %v", ownership, payer, withPolicy, err)
				}
			}
		}
		exec(`DELETE FROM selfhost_installations WHERE id=$1`, install)
		if err := accountingauthority.Record(ctx, store, b, policy); !errors.Is(err, accountingauthority.ErrQuotaExhausted) {
			t.Fatalf("ordinary hosted admission: %v", err)
		}
		var consumed, observed int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT consumed_bytes,observed_bytes FROM billing_workspace_usage_periods WHERE workspace_id=$1`, workspace).Scan(&consumed, &observed); err != nil || consumed != 100 || observed != 100 {
			t.Fatalf("bandwidth changed: %d %d %v", consumed, observed, err)
		}
		if payer == "personal" {
			exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$5,'ciphertext','quota fixture','tunnel','account',$4,$1,true)`, install, node, make([]byte, 32), user, runtimeHash[:])
		}
	}
}
