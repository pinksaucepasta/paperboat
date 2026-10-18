package peersessions

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type quotaByWorkspace map[string]bool

func (q quotaByWorkspace) CheckWorkspaceBandwidthTx(_ context.Context, _ *db.Tx, workspace string, _ time.Time) (bool, error) {
	exhausted, known := q[workspace]
	if !known {
		return false, fmt.Errorf("unexpected workspace %q", workspace)
	}
	return exhausted, nil
}

type grantSigner struct{}

func (grantSigner) SignNetworkConfiguration(map[string]any) (string, error) { return "config", nil }
func (grantSigner) SignRegionalCandidates(map[string]any) (string, error)   { return "candidates", nil }
func (grantSigner) SignRelayGrant(map[string]any) (string, error)           { return "grant", nil }

// F13-03: native relay admission. Hosted relay grants are withheld only when every workspace
// the account can use is out of allowance; a typed reason is reported so the client can say why.
func TestRelayGrantsAreWithheldOnlyWhenEveryWorkspaceIsExhaustedPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	x := fmt.Sprint(time.Now().UnixNano())
	user, team, node := "relay_quota_u_"+x, "relay-quota-t-"+x, "relay_quota_n_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspaces WHERE id IN ($1,$2)`, "personal:"+user, "team:"+team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_members WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, user)
	now := time.Now().UTC()
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids) VALUES($1,'default','v1',$1,'relay.example.test',443,444,'ready',true,$2,1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100,20,$2,$3,ARRAY[$4])`, node, now, now.Add(time.Minute), user)

	service := &NetworkService{store: store, signer: grantSigner{}, issuer: "https://issuer.example.test", now: time.Now}
	denied := func(q quotaByWorkspace) bool {
		t.Helper()
		service.quota = q
		got, err := service.relayQuotaDenied(ctx, user, now)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	personal, teamWorkspace := "personal:"+user, "team:"+team
	if denied(quotaByWorkspace{personal: false, teamWorkspace: false}) {
		t.Fatal("denied while both workspaces have allowance")
	}
	if denied(quotaByWorkspace{personal: true, teamWorkspace: false}) {
		t.Fatal("denied while the team workspace still has allowance")
	}
	if denied(quotaByWorkspace{personal: false, teamWorkspace: true}) {
		t.Fatal("denied while the personal workspace still has allowance")
	}
	if !denied(quotaByWorkspace{personal: true, teamWorkspace: true}) {
		t.Fatal("not denied although every workspace is exhausted")
	}
	service.quota = nil
	if got, err := service.relayQuotaDenied(ctx, user, now); err != nil || got {
		t.Fatalf("denied without a configured quota: %v %v", got, err)
	}

	grants := func(quotaDenied bool) (int, bool) {
		t.Helper()
		var count int
		var withheld bool
		tx, err := store.SQL().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		granted, held, err := service.relayGrants(ctx, tx, networkBinding{AccountID: user, EndpointID: "endpoint_" + x}, nil, 1, now, now.Add(time.Minute), quotaDenied)
		if err != nil {
			t.Fatal(err)
		}
		count, withheld = len(granted), held
		return count, withheld
	}
	if n, held := grants(false); n != 1 || held {
		t.Fatalf("with allowance: %d grants, withheld=%v, want 1/false", n, held)
	}
	if n, held := grants(true); n != 0 || !held {
		t.Fatalf("exhausted: %d grants, withheld=%v, want 0/true", n, held)
	}
}

type failingQuota struct{}

func (failingQuota) CheckWorkspaceBandwidthTx(context.Context, *db.Tx, string, time.Time) (bool, error) {
	return false, fmt.Errorf("billing policy unavailable")
}

// A billing failure must not take peer-network configuration down with it.
func TestRelayQuotaFailureNeverBlocksNetworkConfigurationPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	user := fmt.Sprintf("relay_quota_fail_%d", time.Now().UnixNano())
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "personal:"+user)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	service := &NetworkService{store: store, quota: failingQuota{}, now: time.Now}
	if _, err := service.relayQuotaDenied(ctx, user, time.Now()); err == nil {
		t.Fatal("the failing checker did not fail the raw check")
	}
	if service.relayQuotaWithheld(ctx, user, time.Now()) {
		t.Fatal("a billing failure withheld relay grants")
	}
}
