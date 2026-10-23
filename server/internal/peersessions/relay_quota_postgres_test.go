package peersessions

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

type grantSigner struct{}

func (grantSigner) SignNetworkConfiguration(map[string]any) (string, error) { return "config", nil }
func (grantSigner) SignRegionalCandidates(map[string]any) (string, error)   { return "candidates", nil }
func (grantSigner) SignRelayGrant(map[string]any) (string, error)           { return "grant", nil }

// F13-03: hosted relays are granted per workspace: with the gate configured, a peer whose
// connection's workspace is out of allowance gets no hosted relay, even when the account belongs
// to another workspace with allowance left; without ConfigureQuota nothing is withheld.
func TestRelayGrantsAreWithheldOnlyWhenTheGateDeniesPostgres(t *testing.T) {
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
	user, node := "relay_quota_u_"+x, "relay_quota_n_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, "personal:"+user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "personal:"+user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,state) VALUES($1,$2,'free')`, "personal:"+user, user)
	now := time.Now().UTC()
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids) VALUES($1,'default','v1',$1,'relay.example.test',443,444,'ready',true,$2,1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100,20,$2,$3,ARRAY[$4])`, node, now, now.Add(time.Minute), user)

	team, teamMachine := "relay_quota_t_"+x, "relay_quota_tm_"+x
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.user_machines WHERE id=$1`, teamMachine)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_members WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
	})
	// The account is also a member of a team with allowance left: that must not keep hosted
	// relays for its capped personal workspace (decisions are per workspace, not per account).
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,owner_team_id) VALUES($1,$2,$1,'relay-quota-tm','linux','amd64','/w','online','occupied',$3)`, teamMachine, user, team)
	self := networkBinding{AccountID: user, EndpointID: "endpoint_" + user, MachineID: "relay_quota_self_" + x}
	personalPeer := networkPeer{Identity: networkBinding{AccountID: user, EndpointID: "endpoint_p_" + user, MachineID: "relay_quota_pp_" + x}}
	teamPeer := networkPeer{Identity: networkBinding{AccountID: user, EndpointID: "endpoint_t_" + user, MachineID: teamMachine}}

	service := &NetworkService{store: store, signer: grantSigner{}, issuer: "https://issuer.example.test", now: time.Now}
	billable := func(peers ...networkPeer) []networkPeer {
		t.Helper()
		tx, err := store.SQL().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		out, err := service.billableRelayPeers(ctx, tx, self, peers, now)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	exec(`INSERT INTO paperboat.billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,consumed_bytes,paid,budget_start) VALUES($1,$2,$3,100,100,false,$2)`, "personal:"+user, now.Add(-time.Hour), now.AddDate(0, 1, 0))
	if got := billable(personalPeer, teamPeer); len(got) != 2 {
		t.Fatalf("gate not configured: %d billable peers, want 2", len(got))
	}
	service.ConfigureQuota("https://console.example.test/billing")
	service.quotaGate.SetLookupTimeout(5 * time.Second) // this test database is slower than production
	got := billable(personalPeer, teamPeer)
	if len(got) != 1 || got[0].Identity.MachineID != teamMachine {
		t.Fatalf("capped personal workspace, team with allowance: billable=%v, want only the team peer", got)
	}
}
