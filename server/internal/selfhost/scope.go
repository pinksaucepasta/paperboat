package selfhost

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

type Scope struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}
type Installation struct {
	InstallationID  string `json:"installation_id"`
	NodeID          string `json:"node_id"`
	EndpointHost    string `json:"endpoint_host"`
	Name            string `json:"name"`
	Capability      string `json:"capability"`
	Ready           bool   `json:"ready"`
	ScopeKind       string `json:"scope_kind"`
	ScopeID         string `json:"scope_id"`
	TrustNotice     string `json:"trust_notice"`
	EnrollmentState string `json:"enrollment_state"`
}
type Policy struct {
	Mode            string   `json:"mode"`
	InstallationIDs []string `json:"installation_ids"`
}

var ErrLiveEdgeModeChange = fmt.Errorf("%w: stop active workspace previews and tunnels before changing edge ownership; configure customer DNS and certificates before restarting them", ErrInvalid)

var ErrIncompatibleTopology = errors.New("self-hosted-only HTTP publications require a verified custom domain")

func (s *Service) authorizeScope(ctx context.Context, actor string, scope Scope, write, operator bool) (Scope, error) {
	if scope.Kind == "" {
		scope.Kind = "account"
	}
	if scope.Kind == "account" && scope.ID == "" {
		scope.ID = actor
	}
	switch scope.Kind {
	case "account":
		if scope.ID != actor || actor == "" {
			return scope, ErrUnauthorized
		}
	case "team":
		var allowed bool
		err := s.database.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.teams t JOIN paperboat.team_members m USING(team_id) WHERE t.team_id=$1 AND t.deleted_at IS NULL AND m.account_id=$2 AND m.active AND (NOT $3 OR t.owner_account=$2 OR m.role='admin'))`, scope.ID, actor, write).Scan(&allowed)
		if err != nil {
			return scope, err
		}
		if !allowed {
			return scope, ErrUnauthorized
		}
	case "global":
		if !operator {
			return scope, ErrUnauthorized
		}
		scope.ID = "global"
	default:
		return scope, ErrInvalid
	}
	return scope, nil
}
func (s *Service) List(ctx context.Context, actor string) ([]Installation, error) {
	return s.ListScoped(ctx, actor, Scope{}, false)
}
func (s *Service) ListScoped(ctx context.Context, actor string, scope Scope, operator bool) (out []Installation, returnErr error) {
	filtered := scope.Kind != ""
	if filtered {
		var err error
		scope, err = s.authorizeScope(ctx, actor, scope, false, operator)
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.database.SQL().QueryContext(ctx, `SELECT i.id,i.node_id,n.endpoint_host,i.name,i.capability,i.scope_kind,i.scope_id,i.claim_complete,coalesce(i.claim_complete AND n.ready AND n.state='ready' AND n.last_heartbeat_at>now()-interval '15 seconds' AND(i.capability='relay' OR n.public_ingress_verified_at IS NOT NULL),false) FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id WHERE i.state='active' AND ($1 OR paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,$2)) AND(NOT $3 OR(i.scope_kind=$4 AND i.scope_id=$5)) ORDER BY i.name,i.capability,i.id LIMIT 257`, operator, actor, filtered, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	out = []Installation{}
	for rows.Next() {
		var i Installation
		var complete bool
		if err = rows.Scan(&i.InstallationID, &i.NodeID, &i.EndpointHost, &i.Name, &i.Capability, &i.ScopeKind, &i.ScopeID, &complete, &i.Ready); err != nil {
			return nil, withDatabaseCleanupFailure("list_installations", err, rows.Close())
		}
		i.EnrollmentState = "pending"
		if complete {
			i.EnrollmentState = "enrolled"
		}
		i.TrustNotice = "Native private traffic remains end-to-end encrypted."
		if i.Capability == "tunnel" {
			i.TrustNotice = "Browser HTTP TLS terminates on this server; origin TLS pass-through remains origin-terminated."
		}
		out = append(out, i)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if err = withDatabaseCleanupFailure("list_installations", rowsErr, closeErr); err != nil {
		return nil, err
	}
	if len(out) > 256 {
		return nil, ErrLimit
	}
	return out, nil
}
func (s *Service) Pool(ctx context.Context, actor, capability string) (Policy, error) {
	return s.PoolScoped(ctx, actor, capability, Scope{})
}
func (s *Service) PoolScoped(ctx context.Context, actor, capability string, scope Scope) (Policy, error) {
	if capability == "tunnel" && scope.Kind == "" {
		if ws, ok := workspace.FromContext(ctx); ok && ws.TeamID != "" {
			scope = Scope{Kind: "team", ID: ws.TeamID}
		}
	}
	scope, err := s.authorizeScope(ctx, actor, scope, false, false)
	if err != nil {
		return Policy{}, err
	}
	if scope.Kind != "account" && (scope.Kind != "team" || capability != "tunnel") {
		return Policy{}, fmt.Errorf("%w: relay pools belong to Personal; browser edge pools belong to Personal or a Team", ErrInvalid)
	}
	if capability != "relay" && capability != "tunnel" {
		return Policy{}, ErrInvalid
	}
	out := Policy{Mode: "mixed", InstallationIDs: []string{}}
	if capability == "tunnel" {
		out.Mode = "managed-only"
	}
	var raw []byte
	err = s.database.SQL().QueryRowContext(ctx, `SELECT mode,array_to_json(installation_ids) FROM paperboat.selfhost_pool_policies WHERE scope_kind=$1 AND scope_id=$2 AND capability=$3`, scope.Kind, scope.ID, capability).Scan(&out.Mode, &raw)
	if err == sql.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out.InstallationIDs)
	return out, err
}
func (s *Service) SetPool(ctx context.Context, actor, capability string, p Policy) (Policy, error) {
	return s.SetPoolScoped(ctx, actor, capability, p, Scope{})
}
func (s *Service) SetPoolScoped(ctx context.Context, actor, capability string, p Policy, scope Scope) (result Policy, returnErr error) {
	if capability == "tunnel" && scope.Kind == "" {
		if ws, ok := workspace.FromContext(ctx); ok && ws.TeamID != "" {
			scope = Scope{Kind: "team", ID: ws.TeamID}
		}
	}
	scope, err := s.authorizeScope(ctx, actor, scope, true, false)
	if err != nil {
		return p, err
	}
	if scope.Kind != "account" && (scope.Kind != "team" || capability != "tunnel") {
		return p, fmt.Errorf("%w: relay pools belong to Personal; browser edge pools belong to Personal or a Team", ErrInvalid)
	}
	validMode := capability == "relay" && (p.Mode == "mixed" || p.Mode == "self-hosted-only") || capability == "tunnel" && (p.Mode == "managed-only" || p.Mode == "self-hosted-only")
	if !validMode || len(p.InstallationIDs) > 32 || p.Mode == "self-hosted-only" && len(p.InstallationIDs) == 0 || p.Mode == "managed-only" && len(p.InstallationIDs) != 0 {
		return p, ErrInvalid
	}
	if p.InstallationIDs == nil {
		p.InstallationIDs = []string{}
	}
	seen := map[string]bool{}
	for _, id := range p.InstallationIDs {
		if id == "" || seen[id] {
			return p, ErrInvalid
		}
		seen[id] = true
	}
	raw, _ := json.Marshal(p.InstallationIDs)
	tx, err := s.database.SQL().BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer func() { returnErr = withTransactionRollbackFailure("set_pool", returnErr, tx.Rollback()) }()
	if err = lockTeamWrite(ctx, tx, actor, scope); err != nil {
		return p, err
	}
	if capability == "tunnel" {
		// Serialize policy writes and refuse implicit migration of live publications.
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('browser-edge-policy:'||$1||':'||$2,0))`, scope.Kind, scope.ID); err != nil {
			return p, err
		}
		var mode string
		var live bool
		var team any
		if scope.Kind == "team" {
			team = scope.ID
		}
		if err = tx.QueryRowContext(ctx, `SELECT paperboat.workspace_edge_mode($1,$2)`, actor, team).Scan(&mode); err != nil {
			return p, err
		}
		if mode != p.Mode {
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.preview_leases WHERE ($2::text IS NOT NULL OR account_id=$1) AND owner_team_id IS NOT DISTINCT FROM $2::text AND terminal_state='active' AND lease_deadline>now()) OR EXISTS(SELECT 1 FROM paperboat.tunnels t WHERE ($2::text IS NOT NULL OR t.account_id=$1) AND paperboat.publication_workspace_team('tunnel',t.id) IS NOT DISTINCT FROM $2::text AND desired_state='active' AND deleted_at IS NULL)`, actor, team).Scan(&live)
			if err != nil {
				return p, err
			}
			if live {
				return p, ErrLiveEdgeModeChange
			}
		}
	}
	var count int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM paperboat.selfhost_installations WHERE id IN(SELECT jsonb_array_elements_text($1::jsonb)) AND state='active' AND claim_complete AND scope_kind<>'global' AND capability=$2 AND ((scope_kind=$3 AND scope_id=$4) OR($3='account' AND paperboat.selfhost_scope_visible(scope_kind,scope_id,$4)))`, string(raw), capability, scope.Kind, scope.ID).Scan(&count)
	if err != nil {
		return p, err
	}
	if count != len(p.InstallationIDs) {
		return p, ErrUnauthorized
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES($1,$2,$3,$4,ARRAY(SELECT jsonb_array_elements_text($5::jsonb))) ON CONFLICT(scope_kind,scope_id,capability) DO UPDATE SET mode=excluded.mode,installation_ids=excluded.installation_ids`, scope.Kind, scope.ID, capability, p.Mode, string(raw))
	if err != nil {
		return p, err
	}
	if err = revokeIneligible(ctx, tx, scope); err != nil {
		return p, err
	}
	return p, tx.Commit()
}
func revokeIneligible(ctx context.Context, tx *sql.Tx, scope Scope) error {
	_, err := tx.ExecContext(ctx, `UPDATE paperboat.peer_network_identities SET relay_revocation_generation=relay_revocation_generation+1 WHERE paperboat.selfhost_scope_visible($1,$2,user_id)`, scope.Kind, scope.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE paperboat.tunnel_edge_route_assignments SET state='draining',observed_state='draining',updated_at=now() WHERE state IN('staged','active') AND paperboat.selfhost_scope_visible($1,$2,account_id) AND NOT paperboat.node_route_eligible(edge_node_id,account_id,route_id)`, scope.Kind, scope.ID)
	return err
}
func (s *Service) RemoveScoped(ctx context.Context, actor, id string, operator bool) error {
	var scope Scope
	err := s.database.SQL().QueryRowContext(ctx, `SELECT scope_kind,scope_id FROM paperboat.selfhost_installations WHERE id=$1`, id).Scan(&scope.Kind, &scope.ID)
	if err != nil {
		return err
	}
	if _, err = s.authorizeScope(ctx, actor, scope, true, operator); err != nil {
		return err
	}
	return s.remove(ctx, id, actor, scope)
}
func (s *Service) remove(ctx context.Context, id, actor string, authorizedScope Scope) (returnErr error) {
	tx, err := s.database.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { returnErr = withTransactionRollbackFailure("remove_installation", returnErr, tx.Rollback()) }()
	if err = lockTeamWrite(ctx, tx, actor, authorizedScope); err != nil {
		return err
	}
	var node string
	var scope Scope
	err = tx.QueryRowContext(ctx, `UPDATE paperboat.selfhost_installations SET state='removed',claim_complete=false WHERE id=$1 RETURNING node_id,scope_kind,scope_id`, id).Scan(&node, &scope.Kind, &scope.ID)
	if err != nil {
		return err
	}
	// Removing a pairing withdraws only its scope. Retire the physical node
	// and its signing keys only when no active installation still owns it.
	{
		result, retireErr := tx.ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET state='retired',ready=false,allowed_account_ids='{}',drain_deadline=now(),registry_expires_at=now(),node_generation=node_generation+1 WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM paperboat.selfhost_installations WHERE node_id=$1 AND state='active')`, node)
		if retireErr != nil {
			return retireErr
		}
		changed, retireErr := result.RowsAffected()
		if retireErr != nil {
			return retireErr
		}
		if changed > 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE paperboat.control_usage_verification_keys SET revoked_at=coalesce(revoked_at,$2) WHERE edge_node_id=$1`, node, time.Now()); err != nil {
				return err
			}
		}
	}
	if err = revokeIneligible(ctx, tx, scope); err != nil {
		return err
	}
	return tx.Commit()
}

func lockTeamWrite(ctx context.Context, tx *sql.Tx, actor string, scope Scope) error {
	if scope.Kind != "team" {
		return nil
	}
	var role, owner string
	err := tx.QueryRowContext(ctx, `SELECT m.role,t.owner_account FROM paperboat.teams t JOIN paperboat.team_members m USING(team_id) WHERE t.team_id=$1 AND m.account_id=$2 AND m.active AND t.deleted_at IS NULL FOR SHARE OF t,m`, scope.ID, actor).Scan(&role, &owner)
	if err == sql.ErrNoRows || err == nil && role != "admin" && owner != actor {
		return ErrUnauthorized
	}
	return err
}
