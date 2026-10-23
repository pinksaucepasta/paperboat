package usermachines

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/machineevents"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

type machineCredentialKind string

const (
	machineCredentialSigned   machineCredentialKind = "control_plane_signed"
	machineCredentialExternal machineCredentialKind = "external_issuer"
)

// ErrAmbiguousTeamAccess requires the caller to choose a team before an
// access session can be billed and fenced to team authority.
var ErrAmbiguousTeamAccess = errors.New("multiple teams authorize machine access; select a team")

// The account's own live personal machines are answered by the first branch of machine_management_allowed /
// machine_capability_allowed (own, personal, not revoked or deleted) without calling them: in Personal no other
// branch can apply, and elsewhere that branch already decides true. Eight function calls per row made a page of
// 100 machines cost seconds under load.
func (s *Service) describeMachineAccess(ctx context.Context, account string, machines ...*UserMachine) error {
	if len(machines) == 0 {
		return nil
	}
	ids := make([]string, 0, len(machines))
	byID := make(map[string]*UserMachine, len(machines))
	for _, machine := range machines {
		ids = append(ids, machine.ID)
		byID[machine.ID] = machine
	}
	scope, scoped := workspace.FromContext(ctx)
	var teamID any
	if scoped && scope.TeamID != "" {
		teamID = scope.TeamID
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT m.id,
 CASE WHEN NOT $3::boolean THEN (m.owner_team_id IS NULL AND m.user_id=$1 AND m.deleted_at IS NULL) OR machine_management_allowed($1,m.id)
      WHEN $4::text IS NULL THEN m.user_id=$1 AND m.owner_team_id IS NULL
      ELSE coalesce(m.owner_team_id=$4 AND machine_management_allowed($1,m.id),false) END,
 EXISTS(SELECT 1 FROM team_resource_bindings b WHERE b.resource_kind='machine' AND b.resource_id=m.id AND b.active AND (NOT $3::boolean OR $4::text IS NULL OR b.team_id=$4)),
 coalesce((SELECT json_agg(capability) FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access']) capability WHERE
   CASE WHEN NOT $3::boolean THEN (m.owner_team_id IS NULL AND m.user_id=$1 AND m.revoked_at IS NULL AND m.deleted_at IS NULL) OR machine_capability_allowed($1,m.id,capability)
        WHEN $4::text IS NULL THEN m.user_id=$1 AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL
        ELSE team_machine_capability_allowed($1,$4,m.id,capability) END),'[]'::json),
 coalesce(l.address_slot, 0), EXISTS(SELECT 1 FROM paperboat.peer_network_identities n JOIN paperboat.peer_endpoint_certificates c ON c.fingerprint=n.quic_certificate_fingerprint AND c.revoked_at IS NULL AND c.expires_at>now() WHERE n.machine_id=m.id AND n.machine_generation=m.installation_generation AND n.revoked_at IS NULL)
 FROM user_machines m LEFT JOIN LATERAL paperboat.ensure_control_sync_lease(m.id) l ON true WHERE m.id=ANY($2::text[])`, account, ids, scoped, teamID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var manage, shared bool
		var permissions []byte
		var slot int
		var machineApproved bool
		if err := rows.Scan(&id, &manage, &shared, &permissions, &slot, &machineApproved); err != nil {
			return errors.Join(err, rows.Close())
		}
		machine := byID[id]
		machine.CanManage, machine.Shared = manage, shared
		machine.NetworkApproved = machineApproved
		if slot > 0 {
			machine.AssignedIP = fmt.Sprintf("127.100.%d.%d", slot/256, slot%256)
		}
		if err := json.Unmarshal(permissions, &machine.Permissions); err != nil {
			return errors.Join(err, rows.Close())
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	return s.describeTags(ctx, account, teamID, ids, byID)
}

// describeTags sets each machine's tags from the selected workspace's policy file (the team's,
// or the account's personal one): the stored tags rows, one query for the page.
func (s *Service) describeTags(ctx context.Context, account string, teamID any, ids []string, byID map[string]*UserMachine) error {
	for _, m := range byID {
		m.Tags = []string{}
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT t.machine_id,t.tag FROM workspace_policy_tags t JOIN workspace_policies p ON p.id=t.policy_id
	 WHERE CASE WHEN $2::text IS NULL THEN p.owner_account=$1 ELSE p.team_id=$2 END AND t.machine_id=ANY($3::text[]) ORDER BY t.machine_id,t.tag`, account, teamID, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return errors.Join(err, rows.Close())
		}
		if m := byID[id]; m != nil {
			m.Tags = append(m.Tags, tag)
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// Persist authority under the same lock as team mutations. A permission withdrawn
// while a descriptor is prepared must never escape as an active access session.
func (s *Service) createAccessSession(ctx context.Context, in dbsqlc.CreateUserMachineAccessSessionParams, kind machineCredentialKind) error {
	if kind != machineCredentialSigned && kind != machineCredentialExternal {
		return errors.New("user-machine credential kind is invalid")
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		scope, scoped := workspace.FromContext(ctx)
		if scoped {
			if err := workspace.ValidateTx(ctx, tx, in.UserID, scope); err != nil {
				return err
			}
		}
		if len(in.Capabilities) == 0 {
			return ErrMachineCapabilityUnavailable
		}
		machine, err := s.machineForCapability(ctx, tx.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: in.UserMachineID, UserID: in.UserID, Capability: in.Capabilities[0]})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if machine.State != "online" || !machine.Online {
			return ErrMachineOffline
		}
		for _, capability := range in.Capabilities {
			incoming := ""
			switch capability {
			case "terminal", "exec":
				incoming = "terminal_host"
			case "managed_ssh":
				incoming = "ssh_host"
			case "files":
				incoming = "file_receive"
			}
			if incoming != "" && (!slices.Contains(machine.ConfiguredCapabilities, incoming) || !slices.Contains(machine.ObservedCapabilities, incoming)) {
				return ErrMachineCapabilityUnavailable
			}
		}
		var personal bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_machines WHERE id=$1 AND user_id=$2 AND owner_team_id IS NULL AND revoked_at IS NULL AND deleted_at IS NULL)`, in.UserMachineID, in.UserID).Scan(&personal); err != nil {
			return err
		}
		if scoped && scope.TeamID == "" {
			if !personal {
				return ErrNotFound
			}
			in.TeamID = sql.NullString{}
		} else if scoped {
			var allowed bool
			if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM unnest($3::text[]) capability WHERE NOT team_machine_capability_allowed($2,$4,$1,capability))`, in.UserMachineID, in.UserID, in.Capabilities, scope.TeamID).Scan(&allowed); err != nil {
				return err
			}
			if !allowed {
				return ErrNotFound
			}
			in.TeamID = sql.NullString{String: scope.TeamID, Valid: true}
		} else if !personal {
			rows, err := tx.Query(ctx, `SELECT b.team_id FROM team_resource_bindings b WHERE b.resource_kind='machine' AND b.resource_id=$1 AND NOT EXISTS(SELECT 1 FROM unnest($3::text[]) capability WHERE NOT team_machine_capability_allowed($2,b.team_id,$1,capability)) ORDER BY b.team_id LIMIT 2`, in.UserMachineID, in.UserID, in.Capabilities)
			if err != nil {
				return err
			}
			var authorizedTeams []string
			for rows.Next() {
				var team string
				if err = rows.Scan(&team); err != nil {
					rows.Close()
					return err
				}
				authorizedTeams = append(authorizedTeams, team)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			switch len(authorizedTeams) {
			case 0:
				return ErrNotFound
			case 1:
				in.TeamID = sql.NullString{String: authorizedTeams[0], Valid: true}
			default:
				return ErrAmbiguousTeamAccess
			}
		}
		in.CredentialKind = string(kind)
		if err := tx.Queries().CreateUserMachineAccessSession(ctx, in); err != nil {
			return err
		}
		if err := s.recordAccessStartedTx(ctx, tx, in); err != nil {
			return err
		}
		if !in.TeamID.Valid {
			return nil // the account's own machines are always in each other's peer network
		}
		// Team access can come from another account, new to the host's signed peer network: wake
		// its refresh now instead of leaving the connection to wait for the periodic one.
		return machineevents.Publish(ctx, tx, []string{in.UserMachineID}, machineevents.TopicPeerNetwork)
	})
}

func workspaceTeamArgument(ctx context.Context) (bool, sql.NullString) {
	scope, ok := workspace.FromContext(ctx)
	if !ok {
		return false, sql.NullString{}
	}
	teamID := sql.NullString{}
	if scope.TeamID != "" {
		teamID = sql.NullString{String: scope.TeamID, Valid: true}
	}
	return true, teamID
}

// transferSessionHost binds a transfer-destination lookup to the terminal
// session's persisted workspace. An explicit selector must match that owner;
// without one, the resource's owner_team_id still selects the authority used
// for this exact operation.
func (s *Service) transferSessionHost(ctx context.Context, userID, sessionID string) (context.Context, dbsqlc.UserMachine, error) {
	requested, selected := workspace.FromContext(ctx)
	boundContext := ctx
	var host dbsqlc.UserMachine
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		var ownerTeam sql.NullString
		var machineID string
		if err := tx.QueryRow(ctx, `SELECT s.owner_team_id,m.id
			FROM user_machine_terminal_sessions s JOIN user_machines m ON m.id=s.user_machine_id
			WHERE s.id=$1 AND s.owner_account=$2 AND s.deleted_at IS NULL AND m.deleted_at IS NULL`, sessionID, userID).Scan(&ownerTeam, &machineID); errors.Is(err, sql.ErrNoRows) {
			return ErrTerminalSessionNotFound
		} else if err != nil {
			return err
		}
		stored := workspace.Scope{ID: "personal"}
		if ownerTeam.Valid {
			stored = workspace.Scope{ID: ownerTeam.String, TeamID: ownerTeam.String}
		}
		if selected && requested != stored {
			return ErrTerminalSessionNotFound
		}
		if !selected {
			if err := workspace.ValidateTx(ctx, tx, userID, stored); err != nil {
				return ErrTerminalSessionNotFound
			}
			boundContext = workspace.WithContext(ctx, stored)
		} else {
			if err := workspace.ValidateTx(ctx, tx, userID, requested); err != nil {
				return ErrTerminalSessionNotFound
			}
			boundContext = ctx
		}
		var err error
		host, err = s.machineForCapability(boundContext, tx.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: machineID, UserID: userID, Capability: "terminal"})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTerminalSessionNotFound
		}
		return err
	})
	if err != nil {
		return nil, dbsqlc.UserMachine{}, err
	}
	return boundContext, host, nil
}

func (s *Service) transferDestinationMachine(ctx context.Context, userID, machineID string) (UserMachine, bool, error) {
	row, err := s.machineForCapability(ctx, s.db.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: machineID, UserID: userID, Capability: "files"})
	if errors.Is(err, sql.ErrNoRows) {
		return UserMachine{}, false, nil
	}
	if err != nil {
		return UserMachine{}, false, err
	}
	if row.State == "revoked" || row.State == "disconnected" || row.State == "deleted" || row.SeatState != "occupied" {
		return UserMachine{}, false, nil
	}
	return mapMachine(row), true, nil
}

// lockTransferSession applies the selected scope (or the scope derived from
// the persisted terminal session) while holding the team mutation lock.
func lockTransferSession(ctx context.Context, tx *db.Tx, userID, sessionID string) (string, error) {
	if err := teams.Lock(ctx, tx); err != nil {
		return "", err
	}
	scope, ok := workspace.FromContext(ctx)
	if !ok {
		return "", ErrTerminalSessionNotFound
	}
	if err := workspace.ValidateTx(ctx, tx, userID, scope); err != nil {
		return "", ErrTerminalSessionNotFound
	}
	_, teamID := workspaceTeamArgument(ctx)
	var machineID string
	if err := tx.QueryRow(ctx, `SELECT user_machine_id FROM user_machine_terminal_sessions
		WHERE id=$1 AND owner_account=$2 AND owner_team_id IS NOT DISTINCT FROM $3::text
		AND deleted_at IS NULL FOR UPDATE`, sessionID, userID, teamID).Scan(&machineID); errors.Is(err, sql.ErrNoRows) {
		return "", ErrTerminalSessionNotFound
	} else if err != nil {
		return "", err
	}
	if _, err := getTerminalSession(ctx, tx.Queries(), dbsqlc.GetUserMachineTerminalSessionParams{
		ID: sessionID, UserMachineID: machineID, UserID: userID,
	}); errors.Is(err, sql.ErrNoRows) {
		return "", ErrTerminalSessionNotFound
	} else if err != nil {
		return "", err
	}
	return machineID, nil
}

// getMachineForCapability is the one machine authorization of terminal, exec, managed SSH and
// files. A request made with an API token is also held to the token's scope (PLAN F18-06).
func getMachineForCapability(ctx context.Context, q *dbsqlc.Queries, in dbsqlc.GetUserMachineForCapabilityParams) (dbsqlc.UserMachine, error) {
	var row dbsqlc.UserMachine
	var err error
	workspaceSet, teamID := workspaceTeamArgument(ctx)
	if !workspaceSet {
		row, err = q.GetUserMachineForCapability(ctx, in)
	} else {
		row, err = q.GetUserMachineForCapabilityInWorkspace(ctx, dbsqlc.GetUserMachineForCapabilityInWorkspaceParams{
			ID: in.ID, UserID: in.UserID, Capability: in.Capability, WorkspaceSet: true, WorkspaceTeamID: teamID,
		})
	}
	if err != nil {
		return row, err
	}
	if token, ok := auth.APITokenFromContext(ctx); ok {
		allowed, err := q.APITokenScopeAllows(ctx, dbsqlc.APITokenScopeAllowsParams{TokenID: token, MachineID: row.ID, Capability: in.Capability})
		if err != nil {
			return dbsqlc.UserMachine{}, err
		}
		if !allowed {
			return dbsqlc.UserMachine{}, auth.ErrOutsideTokenScope
		}
	}
	return row, nil
}

func getTerminalSession(ctx context.Context, q *dbsqlc.Queries, in dbsqlc.GetUserMachineTerminalSessionParams) (dbsqlc.UserMachineTerminalSession, error) {
	workspaceSet, teamID := workspaceTeamArgument(ctx)
	if !workspaceSet {
		return q.GetUserMachineTerminalSession(ctx, in)
	}
	return q.GetUserMachineTerminalSessionInWorkspace(ctx, dbsqlc.GetUserMachineTerminalSessionInWorkspaceParams{
		ID: in.ID, UserMachineID: in.UserMachineID, UserID: in.UserID, WorkspaceTeamID: teamID,
	})
}

func getTerminalSessionByIdempotencyKey(ctx context.Context, q *dbsqlc.Queries, in dbsqlc.GetUserMachineTerminalSessionByIdempotencyKeyParams) (dbsqlc.UserMachineTerminalSession, error) {
	workspaceSet, teamID := workspaceTeamArgument(ctx)
	if !workspaceSet {
		return q.GetUserMachineTerminalSessionByIdempotencyKey(ctx, in)
	}
	return q.GetUserMachineTerminalSessionByIdempotencyKeyInWorkspace(ctx, dbsqlc.GetUserMachineTerminalSessionByIdempotencyKeyInWorkspaceParams{
		UserMachineID: in.UserMachineID, UserID: in.UserID, IdempotencyKey: in.IdempotencyKey, WorkspaceTeamID: teamID,
	})
}

func lockTerminalSessions(ctx context.Context, q *dbsqlc.Queries, in dbsqlc.LockUserMachineTerminalSessionsParams) (string, error) {
	workspaceSet, teamID := workspaceTeamArgument(ctx)
	if !workspaceSet {
		return q.LockUserMachineTerminalSessions(ctx, in)
	}
	return q.LockUserMachineTerminalSessionsInWorkspace(ctx, dbsqlc.LockUserMachineTerminalSessionsInWorkspaceParams{
		UserMachineID: in.UserMachineID, UserID: in.UserID, WorkspaceTeamID: teamID,
	})
}
