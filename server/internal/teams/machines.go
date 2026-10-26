package teams

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// MachineGrant describes the team's existing resource binding.
// Administrative roles govern mutations; they never imply use capabilities.
type MachineGrant struct {
	MachineID    string   `json:"machine_id"`
	Audience     string   `json:"audience"`
	AccountID    string   `json:"account_id,omitempty"`
	Capabilities []string `json:"capabilities"`
	Generation   uint64   `json:"generation"`
	Active       bool     `json:"active"`
}
type MachineBinding struct {
	MachineID              string   `json:"machine_id"`
	OwnerAccount           string   `json:"owner_account"`
	OwnerTeamID            string   `json:"owner_team_id,omitempty"`
	Alias                  string   `json:"alias"`
	State                  string   `json:"state"`
	Online                 bool     `json:"online"`
	ConfiguredCapabilities []string `json:"configured_capabilities"`
	Generation             uint64   `json:"generation"`
	Active                 bool     `json:"active"`
}
type MachineRequest struct {
	OperationID        string `json:"operation_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	MachineID          string `json:"machine_id"`
	Action             string `json:"action"`
	Confirmation       string `json:"confirmation,omitempty"`
}
type MachineGrantRequest struct {
	OperationID        string   `json:"operation_id"`
	ExpectedGeneration uint64   `json:"expected_generation"`
	MachineID          string   `json:"machine_id"`
	Audience           string   `json:"audience"`
	AccountID          string   `json:"account_id,omitempty"`
	Capabilities       []string `json:"capabilities"`
	Active             bool     `json:"active"`
}

// MachineCapabilities is every capability an owner can grant on a shared machine. The CLI offers
// the same set; TestCLIOffersExactlyTheServersMachineCapabilities keeps the two from drifting.
var MachineCapabilities = []string{"terminal", "exec", "managed_ssh", "files", "preview_manage", "tunnel_manage", "private_access"}

func ValidMachineCapability(capability string) bool {
	return slices.Contains(MachineCapabilities, capability)
}
func validMachineGrant(r MachineGrantRequest) bool {
	if !validID(r.MachineID) || len(r.Capabilities) < 1 || len(r.Capabilities) > len(MachineCapabilities) {
		return false
	}
	if r.Audience != "all_members" && r.Audience != "selected_member" || r.Audience == "all_members" && r.AccountID != "" || r.Audience == "selected_member" && !validID(r.AccountID) {
		return false
	}
	seen := map[string]bool{}
	for _, c := range r.Capabilities {
		if !ValidMachineCapability(c) || seen[c] {
			return false
		}
		seen[c] = true
	}
	return true
}

// AuthorizeMachineTx requires Lock first and binds a decision to one ownership
// path. An empty team means personal ownership, never an arbitrary team fallback.
func AuthorizeMachineTx(ctx context.Context, tx *db.Tx, account, team, machine, capability string) (Decision, error) {
	d := Decision{AccountID: account, TeamID: team, ResourceKind: "machine", ResourceID: machine, Permission: capability}
	if !ValidMachineCapability(capability) || !validID(account) || !validID(machine) {
		return d, ErrInvalid
	}
	var allowed bool
	if team == "" {
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_machines WHERE id=$1 AND user_id=$2 AND owner_team_id IS NULL AND revoked_at IS NULL AND deleted_at IS NULL)`, machine, account).Scan(&allowed)
		if err != nil {
			return d, err
		}
		if !allowed {
			return d, ErrForbidden
		}
		return d, nil
	}
	err := tx.QueryRow(ctx, `SELECT team_machine_capability_allowed($1,$2,$3,$4)`, account, team, machine, capability).Scan(&allowed)
	if err != nil {
		return d, err
	}
	if !allowed {
		return d, ErrForbidden
	}
	// The grant generation is the team policy's version: a saved policy fences decisions as a
	// grant change did.
	err = tx.QueryRow(ctx, `SELECT t.generation,m.membership_generation,CASE WHEN t.owner_account=$1 THEN 'owner' ELSE m.role END,b.generation,coalesce((SELECT p.version FROM workspace_policies p WHERE p.team_id=t.team_id),1) FROM teams t JOIN team_members m USING(team_id) JOIN team_resource_bindings b USING(team_id) WHERE t.team_id=$2 AND m.account_id=$1 AND b.resource_kind='machine' AND b.resource_id=$3`, account, team, machine).Scan(&d.TeamGeneration, &d.MembershipGeneration, &d.Role, &d.BindingGeneration, &d.GrantGeneration)
	return d, err
}

func (s *Service) GrantMachine(ctx context.Context, account, team string, r MachineGrantRequest) (Team, error) {
	if !validMachineGrant(r) {
		return Team{}, ErrInvalid
	}
	r.Capabilities = slices.Clone(r.Capabilities)
	slices.Sort(r.Capabilities)
	return s.mutate(ctx, account, team, r.OperationID, "machine_grant", r.ExpectedGeneration, r, func(ctx context.Context, tx *db.Tx, t Team, actor Member) error {
		owner, ownerTeam, err := machineOwnerTx(ctx, tx, r.MachineID)
		if err != nil {
			return err
		}
		if !Allowed(actor.Role, PermissionSettingsManage) && !(ownerTeam == "" && owner == account) {
			return ErrForbidden
		}
		if ownerTeam != "" && ownerTeam != team {
			return ErrForbidden
		}
		if r.Audience == "selected_member" {
			if _, ok := member(t, r.AccountID); !ok {
				return ErrForbidden
			}
		}
		var bound bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM team_resource_bindings WHERE team_id=$1 AND resource_kind='machine' AND resource_id=$2 AND active AND owner_account=$3)`, team, r.MachineID, owner).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return ErrForbidden
		}
		return ApplyMachineAccessTx(ctx, tx, s.policy, team, account, r)
	})
}
func machineOwnerTx(ctx context.Context, tx *db.Tx, machine string) (string, string, error) {
	var owner, team string
	err := tx.QueryRow(ctx, `SELECT user_id,coalesce(owner_team_id,'') FROM user_machines WHERE id=$1 AND revoked_at IS NULL AND deleted_at IS NULL FOR UPDATE`, machine).Scan(&owner, &team)
	if err == pgx.ErrNoRows {
		err = ErrNotFound
	}
	return owner, team, err
}
func (s *Service) Machine(ctx context.Context, account, team string, r MachineRequest) (Team, error) {
	if !validID(r.MachineID) {
		return Team{}, ErrInvalid
	}
	kind := "machine_" + r.Action
	if r.Action == "share" {
		// A share that has to wait for approval is its own kind of event in the team's history.
		if admission, err := s.AdmissionFor(ctx, account, team); err == nil && admission == AdmissionPending {
			kind = "machine_request"
		}
	}
	return s.mutate(ctx, account, team, r.OperationID, kind, r.ExpectedGeneration, r, func(ctx context.Context, tx *db.Tx, t Team, actor Member) error {
		owner, ownerTeam, err := machineOwnerTx(ctx, tx, r.MachineID)
		if err != nil {
			return err
		}
		switch r.Action {
		case "share":
			if owner != account || ownerTeam != "" {
				return ErrForbidden
			}
			if (admissionFor(t, actor) == AdmissionPending) != (kind == "machine_request") {
				return ErrConflict
			}
			if kind == "machine_request" {
				return requestMachineTx(ctx, tx, team, r.MachineID, account)
			}
			return bindMachineTx(ctx, tx, team, r.MachineID, owner)
		case "approve", "decline":
			if !Allowed(actor.Role, PermissionSettingsManage) || ownerTeam != "" {
				return ErrForbidden
			}
			requestedBy, err := requestRowTx(ctx, tx, team, r.MachineID)
			if err != nil {
				return err
			}
			if r.Action == "decline" {
				_, err = tx.Exec(ctx, `UPDATE team_machine_requests SET state='declined',decided_by=$3,decided_at=now() WHERE team_id=$1 AND machine_id=$2`, team, r.MachineID, account)
				return err
			}
			if requestedBy != owner {
				return ErrConflict
			}
			if err = bindMachineTx(ctx, tx, team, r.MachineID, owner); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `DELETE FROM team_machine_requests WHERE team_id=$1 AND machine_id=$2`, team, r.MachineID)
			return err
		case "unshare":
			if ownerTeam != "" || owner != account && !Allowed(actor.Role, PermissionSettingsManage) {
				return ErrForbidden
			}
			if _, err = tx.Exec(ctx, `DELETE FROM team_machine_requests WHERE team_id=$1 AND machine_id=$2`, team, r.MachineID); err != nil {
				return err
			}
			return unbindMachineTx(ctx, tx, team, r.MachineID)
		case "transfer_to_team":
			if ownerTeam != "" || owner != account || !Allowed(actor.Role, PermissionSettingsManage) {
				return ErrForbidden
			}
			if r.Confirmation != r.MachineID {
				return ErrInvalid
			}
			var published bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM preview_leases WHERE owner_machine_id=$1 AND access_mode='public' AND terminal_state='active') OR EXISTS(SELECT 1 FROM tunnels t WHERE t.access_mode='public' AND t.deleted_at IS NULL AND (t.created_by_host_id=$1 OR EXISTS(SELECT 1 FROM tunnel_connectors c WHERE c.tunnel_id=t.id AND c.host_id=$1 AND c.revoked_at IS NULL)))`, r.MachineID).Scan(&published); err != nil {
				return err
			}
			if published {
				return ErrMachinePublication
			}
			// A machine moving into the team counts against the team's machine ceiling.
			if err = planlimits.CheckTx(ctx, tx, planlimits.Scope{Team: team}, planlimits.Machines); err != nil {
				return err
			}
			if err = bindMachineTx(ctx, tx, team, r.MachineID, owner); err != nil {
				return err
			}
			// Other personal shares cannot silently become grants over team property: those teams
			// lose the machine and their policies forget it.
			var others []string
			if err = tx.QueryRow(ctx, `SELECT coalesce(array_agg(team_id),'{}') FROM team_resource_bindings WHERE resource_kind='machine' AND resource_id=$1 AND team_id<>$2 AND active`, r.MachineID, team).Scan(&others); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE teams SET generation=generation+1 WHERE team_id=ANY($1)`, others); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE team_resource_bindings SET active=false,generation=generation+1 WHERE resource_kind='machine' AND resource_id=$1 AND team_id<>$2 AND active`, r.MachineID, team); err != nil {
				return err
			}
			for _, other := range others {
				if err = refreshPolicyTx(ctx, tx, Workspace{TeamID: other}, account); err != nil {
					return err
				}
			}
			_, err = tx.Exec(ctx, `UPDATE user_machines SET owner_team_id=$2,version=version+1,updated_at=now() WHERE id=$1`, r.MachineID, team)
			var conflict *pgconn.PgError
			if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == "user_machines_owner_alias" {
				return ErrMachineAliasConflict
			}
			return err
		case "remove":
			if ownerTeam != team || !Allowed(actor.Role, PermissionSettingsManage) {
				return ErrForbidden
			}
			if r.Confirmation != r.MachineID {
				return ErrInvalid
			}
			if err = unbindMachineTx(ctx, tx, team, r.MachineID); err != nil {
				return err
			}
			return removeTeamMachineTx(ctx, tx, team, r.MachineID)
		default:
			return ErrInvalid
		}
	})
}

// ShareEnrolledMachineTx puts a machine its owner just enrolled into one of the owner's teams, the
// way the team's share action would: right away, or as a request an owner or admin must accept when
// the team asks for approval. It runs inside the enrollment transaction so a machine never enrolls
// half-placed, and returns what happened (AdmissionAccepted or AdmissionPending).
func ShareEnrolledMachineTx(ctx context.Context, tx *db.Tx, account, team, machine string) (string, error) {
	if !validID(team) || !validID(machine) {
		return "", ErrInvalid
	}
	if err := Lock(ctx, tx); err != nil {
		return "", err
	}
	t, err := ReadTx(ctx, tx, team)
	if err != nil {
		return "", err
	}
	if t.Deleted {
		return "", ErrForbidden
	}
	if t.Suspended {
		return "", ErrSuspended
	}
	actor, ok := member(t, account)
	if !ok {
		return "", ErrForbidden
	}
	admission, kind := admissionFor(t, actor), "machine_share"
	if admission == AdmissionPending {
		kind = "machine_request"
		err = requestMachineTx(ctx, tx, team, machine, account)
	} else {
		err = bindMachineTx(ctx, tx, team, machine, account)
	}
	if err != nil {
		return "", err
	}
	if err = advance(ctx, tx, team); err != nil {
		return "", err
	}
	return admission, AuditTx(ctx, tx, account, team, kind, "enroll:"+machine, map[string]any{"machine_id": machine})
}
func bindMachineTx(ctx context.Context, tx *db.Tx, team, machine, owner string) error {
	var count int
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE active),coalesce(bool_or(resource_kind='machine' AND resource_id=$2),false) FROM team_resource_bindings WHERE team_id=$1`, team, machine).Scan(&count, &exists); err != nil {
		return err
	}
	if !exists && count >= MaximumResources {
		return ErrLimit
	}
	_, err := tx.Exec(ctx, `INSERT INTO team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'machine',$2,$3,true,1) ON CONFLICT(team_id,resource_kind,resource_id) DO UPDATE SET active=true,generation=team_resource_bindings.generation+1`, team, machine, owner)
	return err
}
func unbindMachineTx(ctx context.Context, tx *db.Tx, team, machine string) error {
	if _, err := tx.Exec(ctx, `UPDATE team_resource_bindings SET active=false,generation=generation+1 WHERE team_id=$1 AND resource_kind='machine' AND resource_id=$2 AND active`, team, machine); err != nil {
		return err
	}
	// The policy forgets the machine: sharing it again starts with no access, as before.
	return refreshPolicyTx(ctx, tx, Workspace{TeamID: team}, "")
}

func readMachinesTx(ctx context.Context, tx *db.Tx, team string, out *Team) error {
	out.Machines = []MachineBinding{}
	rows, err := tx.Query(ctx, `SELECT m.id,CASE WHEN m.owner_team_id IS NULL THEN m.user_id ELSE '' END,coalesce(m.owner_team_id,''),m.alias,m.state,m.online,m.configured_capabilities,b.generation,b.active FROM team_resource_bindings b JOIN user_machines m ON m.id=b.resource_id WHERE b.team_id=$1 AND b.resource_kind='machine' ORDER BY m.id`, team)
	if err != nil {
		return err
	}
	for rows.Next() {
		var b MachineBinding
		if err = rows.Scan(&b.MachineID, &b.OwnerAccount, &b.OwnerTeamID, &b.Alias, &b.State, &b.Online, &b.ConfiguredCapabilities, &b.Generation, &b.Active); err != nil {
			rows.Close()
			return err
		}
		out.Machines = append(out.Machines, b)
	}
	err = rows.Err()
	rows.Close()
	return err
}

// The same credential, ENV and runtime cleanup serves personal and team removal.
func removeTeamMachineTx(ctx context.Context, tx *db.Tx, team, machine string) error {
	var issuer, environment string
	if err := tx.QueryRow(ctx, `SELECT user_id,environment_id FROM user_machines WHERE id=$1 AND owner_team_id=$2 FOR UPDATE`, machine, team).Scan(&issuer, &environment); err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := db.FenceRemovedMachineVaultTx(ctx, tx, issuer, machine); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE user_machines SET revoked_at=coalesce(revoked_at,$3),state='revoked',seat_state='released',online=false,version=version+1,updated_at=$3 WHERE id=$1 AND owner_team_id=$2`, machine, team, now); err != nil {
		return err
	}
	if err := db.CleanupMachineMachineTx(ctx, tx, issuer, machine, now); err != nil {
		return err
	}
	return db.RevokeMachineEnvironmentTx(ctx, tx, environment, now)
}
func removeTeamMachinesTx(ctx context.Context, tx *db.Tx, team string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM user_machines WHERE owner_team_id=$1 AND revoked_at IS NULL AND deleted_at IS NULL ORDER BY id`, team)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = removeTeamMachineTx(ctx, tx, team, id); err != nil {
			return err
		}
	}
	return nil
}
