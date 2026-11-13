package peersessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// maximumOfflinePeers bounds the CLI endpoints a host's document names for offline session starts (the most recently
// used sessions of every account that may start a session there), and the team hosts a member's CLI document names.
// At 32 an account's busy endpoints crowded a member's out, and its pb ssh to a team machine was refused
// (scope_rejected). 400 peers at five capabilities stay within half of peernetwork.MaxScopes; the document loops stop
// adding at the bound rather than failing (Tailscale's netmap names every peer the policy allows).
const maximumOfflinePeers = 400

// offlineCapabilities maps the policy capability that decides a session (machine_capability_allowed, the same
// function online descriptors use) to the network scope capability the daemon checks.
var offlineCapabilities = [][2]string{{"terminal", "terminal"}, {"exec", "exec"}, {"managed_ssh", "managed_ssh"}, {"files", "file_transfer"}, {"preview_manage", "inspector"}}

// offlineOwnerOnly lists the network capabilities a team grant never confers offline. Inspecting is decided per preview
// or tunnel (workspace_policy_resource_allowed: its owner, or the personal machine's owner, always; others by an
// "inspect" rule that names the resource), and no machine-level policy capability stands for it, so the nearest one
// (preview_manage, which neither implies nor is implied by inspect) maps only for the host's own account.
var offlineOwnerOnly = []string{"inspector"}

type offlinePeer struct {
	binding      networkBinding
	certExpiry   time.Time
	capabilities []string
	generation   int64
}

// offlineAccessPeers compiles the session rights a host keeps while the control plane is unreachable (PLAN 3b
// offline-capable authority, F21-11): for every active CLI endpoint of the host's active owner or of an active member
// of a non-suspended team the host is shared with, the capabilities that account may start on the host now, decided
// by the functions online descriptors use (machine_capability_allowed for the owner, team_machine_capability_allowed
// per team); nothing for a host whose environment is not active. A capability that needs the control plane (none
// today; per-operation approval or step-up would) is left out. The generation is the greatest of the deciding inputs'
// versions (binding, workspace policy, membership), so the daemon sees any policy change as a new scope.
func offlineAccessPeers(ctx context.Context, tx *sql.Tx, self networkBinding, now time.Time) ([]offlinePeer, error) {
	rows, err := tx.QueryContext(ctx, `WITH host AS (
 SELECT m.id,m.user_id FROM paperboat.user_machines m
 JOIN paperboat.users owner ON owner.id=m.user_id AND owner.status='active'
 JOIN paperboat.control_environments environment ON environment.id=m.environment_id AND environment.desired_state='active' AND environment.revoked_at IS NULL
 WHERE m.id=$1 AND m.user_id=$2 AND m.revoked_at IS NULL AND m.deleted_at IS NULL
), host_teams AS (
 -- A suspended team keeps its online sessions cut by a trigger; an offline right has none behind it, so it is left out.
 SELECT b.team_id,b.generation AS binding_generation,coalesce(policy.version,1) AS policy_version FROM host h
 JOIN paperboat.team_resource_bindings b ON b.resource_kind='machine' AND b.resource_id=h.id AND b.active
 JOIN paperboat.teams t ON t.team_id=b.team_id AND NOT t.suspended AND t.deleted_at IS NULL
 LEFT JOIN paperboat.workspace_policies policy ON policy.team_id=b.team_id
), accounts AS (
 -- The generation moves with every deciding input: the binding, the workspace policy and the membership.
 SELECT h.user_id AS account_id,coalesce((SELECT policy.version FROM paperboat.workspace_policies policy WHERE policy.owner_account=h.user_id),1) AS generation FROM host h
 UNION ALL SELECT member.account_id,greatest(ht.binding_generation,ht.policy_version,member.membership_generation)
 FROM host_teams ht JOIN paperboat.team_members member ON member.team_id=ht.team_id AND member.active
), endpoints AS (
 SELECT c.id,c.user_id,max(a.generation) AS generation FROM paperboat.cli_client_sessions c
 JOIN accounts a ON a.account_id=c.user_id
 JOIN paperboat.users actor ON actor.id=c.user_id AND actor.status='active'
 JOIN paperboat.peer_network_identities n ON n.endpoint_id=c.id AND n.user_id=c.user_id AND n.role='cli' AND n.revoked_at IS NULL
 WHERE c.state='active' AND c.revoked_at IS NULL AND c.api_token_id IS NULL
 GROUP BY c.id,c.user_id,c.last_used_at
 ORDER BY c.last_used_at DESC NULLS LAST,c.id LIMIT $3
), allowed AS MATERIALIZED (
 -- A capability is decided per account, not per endpoint: the policy functions run once per (account, capability)
 -- instead of once per CLI endpoint (a team-shared host took 1.5 s of CPU per document with 66 endpoints).
 SELECT u.user_id,cap.network FROM (SELECT DISTINCT user_id FROM endpoints) u CROSS JOIN host h
 CROSS JOIN unnest($4::text[],$5::text[]) AS cap(policy,network)
 WHERE (u.user_id=h.user_id AND paperboat.machine_capability_allowed(u.user_id,h.id,cap.policy))
  OR (NOT cap.network=ANY($6::text[]) AND EXISTS(SELECT 1 FROM host_teams ht WHERE paperboat.team_machine_capability_allowed(u.user_id,ht.team_id,h.id,cap.policy)))
)
SELECT e.id,e.user_id,array_to_json(array_agg(a.network ORDER BY a.network)),e.generation
FROM endpoints e JOIN allowed a ON a.user_id=e.user_id
GROUP BY e.id,e.user_id,e.generation ORDER BY e.id`, self.EndpointID, self.AccountID, maximumOfflinePeers, policyNames(), networkNames(), offlineOwnerOnly)
	if err != nil {
		return nil, err
	}
	return bindOfflinePeers(ctx, tx, rows, now)
}

func policyNames() []string {
	out := make([]string, len(offlineCapabilities))
	for i, c := range offlineCapabilities {
		out[i] = c[0]
	}
	return out
}

func networkNames() []string {
	out := make([]string, len(offlineCapabilities))
	for i, c := range offlineCapabilities {
		out[i] = c[1]
	}
	return out
}

// teamHostPeers is offlineAccessPeers seen from the member's CLI: the active hosts of the teams the CLI's account is an
// active member of, with the capabilities the member may start there (team_machine_capability_allowed, the decision the
// host's own document applies). SSH to a team machine makes no control-plane request, so no access session puts the host
// in the CLI's document; without these dial scopes the client refuses the peer ("peer network admission refused").
// The generation is the greatest of the deciding inputs' versions, as for the host's side.
func teamHostPeers(ctx context.Context, tx *sql.Tx, self networkBinding, now time.Time) ([]offlinePeer, error) {
	rows, err := tx.QueryContext(ctx, `SELECT n.endpoint_id,m.user_id,array_to_json(array_agg(cap.network ORDER BY cap.network)),
 max(greatest(b.generation,coalesce(policy.version,1),member.membership_generation))
FROM paperboat.team_members member
JOIN paperboat.teams t ON t.team_id=member.team_id AND NOT t.suspended AND t.deleted_at IS NULL
JOIN paperboat.team_resource_bindings b ON b.team_id=member.team_id AND b.resource_kind='machine' AND b.active
JOIN paperboat.user_machines m ON m.id=b.resource_id AND m.user_id<>member.account_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL
JOIN paperboat.users owner ON owner.id=m.user_id AND owner.status='active'
JOIN paperboat.control_environments environment ON environment.id=m.environment_id AND environment.desired_state='active' AND environment.revoked_at IS NULL
JOIN paperboat.peer_network_identities n ON n.machine_id=m.id AND n.user_id=m.user_id AND n.role='machine' AND n.revoked_at IS NULL
LEFT JOIN paperboat.workspace_policies policy ON policy.team_id=member.team_id
CROSS JOIN unnest($2::text[],$3::text[]) AS cap(policy,network)
WHERE member.account_id=$1 AND member.active AND NOT cap.network=ANY($4::text[])
 AND paperboat.team_machine_capability_allowed(member.account_id,member.team_id,m.id,cap.policy)
GROUP BY n.endpoint_id,m.user_id ORDER BY n.endpoint_id LIMIT $5`, self.AccountID, policyNames(), networkNames(), offlineOwnerOnly, maximumOfflinePeers)
	if err != nil {
		return nil, err
	}
	return bindOfflinePeers(ctx, tx, rows, now)
}

// bindOfflinePeers reads (endpoint, account, capabilities, generation) rows and loads each endpoint's network binding.
func bindOfflinePeers(ctx context.Context, tx *sql.Tx, rows *sql.Rows, now time.Time) ([]offlinePeer, error) {
	type candidate struct {
		endpoint, account string
		capabilities      []string
		generation        int64
	}
	var candidates []candidate
	var err error
	for rows.Next() {
		var c candidate
		var raw []byte
		if err = rows.Scan(&c.endpoint, &c.account, &raw, &c.generation); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &c.capabilities); err != nil {
			break
		}
		candidates = append(candidates, c)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]offlinePeer, 0, len(candidates))
	for _, c := range candidates {
		binding, certExpiry, err := loadNetworkBinding(ctx, tx, c.account, c.endpoint, now, false)
		if err == sql.ErrNoRows || err == ErrUnavailable {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, offlinePeer{binding: binding, certExpiry: certExpiry, capabilities: c.capabilities, generation: c.generation})
	}
	return out, nil
}
