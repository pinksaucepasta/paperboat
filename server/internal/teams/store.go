package teams

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/server/internal/config"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

type Service struct {
	db          *db.DB
	mail        config.TransactionalMail
	mailEnabled bool
	policy      config.WorkspacePolicy
}

func NewService(store *db.DB) *Service {
	d := config.Default()
	return &Service{db: store, mail: d.Mail, policy: d.WorkspacePolicy}
}

// ConfigurePolicy supplies the workspace policy limits before serving requests.
func (s *Service) ConfigurePolicy(p config.WorkspacePolicy) { s.policy = p }

// ConfigureMail supplies deployment policy before serving requests.
func (s *Service) ConfigureMail(policy config.TransactionalMail, enabled bool) {
	s.mail = policy
	s.mailEnabled = enabled
}

// Lock serializes team-touching writes; call it first in the transaction (see db.LockTeamWrites).
func Lock(ctx context.Context, tx *db.Tx) error {
	return db.LockTeamWrites(ctx, tx)
}
func AuditTx(ctx context.Context, tx *db.Tx, account, team, action, operation string, metadata map[string]any) error {
	metadata, err := activityMetadata(metadata, true)
	if err != nil {
		return err
	}
	return audit.NewWriter(nil).WriteTx(ctx, tx, audit.Event{ActorUserID: account, ActorType: audit.ActorUser, EventType: "team." + action, ResourceType: "team", ResourceID: team, IdempotencyKey: account + ":" + operation, Metadata: metadata})
}

// ReadTx reads a team and locks its row, for a change that depends on what it read.
func ReadTx(ctx context.Context, tx *db.Tx, id string) (Team, error) {
	return readTx(ctx, tx, id, " FOR UPDATE")
}

// readSnapshotTx reads a team without locking it, inside a read-only snapshot.
func readSnapshotTx(ctx context.Context, tx *db.Tx, id string) (Team, error) {
	return readTx(ctx, tx, id, "")
}

func readTx(ctx context.Context, tx *db.Tx, id, lock string) (Team, error) {
	out := Team{TeamID: id, Members: []Member{}, Grants: []Grant{}, Machines: []MachineBinding{}, MachineGrants: []MachineGrant{}, MachineRequests: []MachineAdmission{}, TerminalSessionGrants: []TerminalSessionGrant{}}
	err := tx.QueryRow(ctx, `SELECT sso_required,approve_new_machines,coalesce((SELECT p.allow_private_access FROM workspace_policies p WHERE p.team_id=$1),false),coalesce(display_name,team_id),coalesce(display_name,team_id),owner_account,generation,deleted_at IS NOT NULL,suspended,CASE WHEN deleted_at IS NOT NULL THEN 'deleted' WHEN suspended THEN 'suspended' ELSE coalesce((SELECT CASE WHEN rotation_required THEN 'rotation_pending' ELSE 'ready' END FROM environment_vault_teams WHERE team_id=$1),'not_initialized') END FROM teams WHERE team_id=$1`+lock, id).Scan(&out.SSORequired, &out.ApproveNewMachines, &out.AllowPrivateAccess, &out.Name, &out.DisplayName, &out.OwnerAccount, &out.Generation, &out.Deleted, &out.Suspended, &out.ENVStatus)
	if err != nil {
		return out, mapTeamReadNotFound(err)
	}
	rows, err := tx.Query(ctx, `SELECT m.account_id,m.membership_generation,CASE WHEN m.account_id=$2 THEN 'owner' ELSE m.role END,m.active,u.primary_email,u.display_name,m.joined_at,m.directory_user_id IS NOT NULL,m.sso_exempt FROM team_members m JOIN users u ON u.id=m.account_id WHERE m.team_id=$1 ORDER BY m.account_id`, id, out.OwnerAccount)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var m Member
		if err = rows.Scan(&m.AccountID, &m.MembershipGeneration, &m.Role, &m.Active, &m.Email, &m.DisplayName, &m.JoinedAt, &m.DirectoryManaged, &m.SSOExempt); err != nil {
			rows.Close()
			return out, err
		}
		out.Members = append(out.Members, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	// The team ENV's and inspect/replay grants are the policy's (policyGrantsTx, below); their
	// old rows stay unread.
	rows, err = tx.Query(ctx, `SELECT account_id,resource_kind,resource_id,permission,generation,active FROM team_resource_grants WHERE team_id=$1 AND resource_kind<>'env' AND permission NOT IN ('inspect','replay') ORDER BY resource_kind,resource_id,account_id`, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var g Grant
		if err = rows.Scan(&g.AccountID, &g.ResourceKind, &g.ResourceID, &g.Permission, &g.Generation, &g.Active); err != nil {
			return out, err
		}
		out.Grants = append(out.Grants, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	teamPolicy, err := teamPolicyTx(ctx, tx, id)
	if err != nil {
		return out, err
	}
	out.Grants = append(out.Grants, policyGrants(teamPolicy, id)...)
	rows, err = tx.Query(ctx, `SELECT terminal_session_id,audience,coalesce(account_id,''),role,generation,active FROM team_terminal_session_grants WHERE team_id=$1 ORDER BY terminal_session_id,audience,account_id`, id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var g TerminalSessionGrant
		if err = rows.Scan(&g.TerminalSessionID, &g.Audience, &g.AccountID, &g.Role, &g.Generation, &g.Active); err != nil {
			rows.Close()
			return out, err
		}
		out.TerminalSessionGrants = append(out.TerminalSessionGrants, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = readMachinesTx(ctx, tx, id, &out); err != nil {
		return out, err
	}
	out.MachineGrants = machineGrants(teamPolicy)
	err = readMachineRequestsTx(ctx, tx, id, &out)
	return out, err
}

func mapTeamReadNotFound(err error) error {
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	return err
}

// AuthorizeTx requires Lock first. Decisions describe current authoritative state;
// consumers must revalidate generations before forwarding or using cached access.
func AuthorizeTx(ctx context.Context, tx *db.Tx, account, team, kind, id, permission string) (Decision, error) {
	var accountActive bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active')`, account).Scan(&accountActive); err != nil {
		return Decision{}, err
	}
	if !accountActive {
		return Decision{}, ErrForbidden
	}
	if kind == "machine" {
		return AuthorizeMachineTx(ctx, tx, account, team, id, permission)
	}
	var d Decision
	var paid bool
	if err := tx.QueryRow(ctx, `SELECT team_paid_entitled($1)`, team).Scan(&paid); err != nil {
		return d, err
	}
	if !paid {
		return d, ErrForbidden
	}
	// Only the caller's membership and this resource's rows decide; the team row is locked as ReadTx locks it.
	d = Decision{TeamID: team, AccountID: account, ResourceKind: kind, ResourceID: id, Permission: permission}
	var member bool
	err := tx.QueryRow(ctx, `SELECT t.generation,coalesce(m.membership_generation,0),coalesce(CASE WHEN t.owner_account=$2 THEN 'owner' ELSE m.role END,''),
 t.deleted_at IS NULL AND NOT t.suspended AND coalesce(m.active,false)
 FROM teams t LEFT JOIN team_members m ON m.team_id=t.team_id AND m.account_id=$2 WHERE t.team_id=$1 FOR UPDATE OF t`, team, account).Scan(&d.TeamGeneration, &d.MembershipGeneration, &d.Role, &member)
	if err != nil {
		return Decision{}, mapTeamReadNotFound(err)
	}
	if !member {
		return Decision{}, ErrForbidden
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT active,generation FROM team_resource_bindings WHERE team_id=$1 AND resource_kind=$2 AND resource_id=$3`, team, kind, id).Scan(&active, &d.BindingGeneration)
	if err == pgx.ErrNoRows || err == nil && !active {
		return d, ErrForbidden
	}
	if err != nil {
		return d, err
	}
	if kind == "preview" || kind == "tunnel" || kind == "lazy_policy" {
		var current bool
		query := `SELECT EXISTS(SELECT 1 FROM tunnels r JOIN team_resource_bindings b ON b.resource_id=r.id AND b.owner_account=r.account_id WHERE b.team_id=$1 AND b.resource_kind='tunnel' AND r.id=$2 AND r.deleted_at IS NULL)`
		if kind == "preview" {
			query = `SELECT EXISTS(SELECT 1 FROM preview_leases r JOIN team_resource_bindings b ON b.resource_id=r.id AND b.owner_account=r.account_id WHERE b.team_id=$1 AND b.resource_kind='preview' AND r.id=$2 AND r.terminal_state='active' AND r.lease_deadline>now())`
		}
		if kind == "lazy_policy" {
			query = `SELECT EXISTS(SELECT 1 FROM lazy_access_policies r JOIN team_resource_bindings b ON b.resource_id=r.id AND b.owner_account=r.account_id WHERE b.team_id=$1 AND b.resource_kind='lazy_policy' AND r.id=$2 AND r.deleted_at IS NULL AND r.expires_at>now())`
		}
		if err = tx.QueryRow(ctx, query, team, id).Scan(&current); err != nil {
			return d, err
		}
		if !current {
			return d, ErrForbidden
		}
	}
	if kind == "env" || policyResourceAction(kind, permission) {
		// The team ENV and preview/tunnel inspect/replay follow the team policy; a policy save
		// advances GrantGeneration, which fences what was authorized under the old version.
		var allowed bool
		var version uint64
		allowed = kind == "env" && id == team
		q, args := `SELECT team_env_permission($1,$2)='write' OR (team_env_permission($1,$2)='read' AND $3::text='read')`, []any{account, team, permission}
		if kind != "env" {
			allowed = true
			q, args = `SELECT workspace_policy_resource_allowed($1,$2,$4,$5,$3)`, []any{account, team, permission, kind, id}
		}
		var decided bool
		if err = tx.QueryRow(ctx, q+`,coalesce((SELECT version FROM workspace_policies WHERE team_id=$2),0)`, args...).Scan(&decided, &version); err != nil {
			return d, err
		}
		allowed = allowed && decided
		if !allowed {
			return d, ErrForbidden
		}
		d.GrantGeneration = version
		return d, nil
	}
	// The member's own grants on this resource (policy grants are only the env and inspect/replay, decided above).
	rows, err := tx.Query(ctx, `SELECT permission,generation FROM team_resource_grants
 WHERE team_id=$1 AND account_id=$2 AND resource_kind=$3 AND resource_id=$4 AND active AND permission NOT IN ('inspect','replay')
 ORDER BY permission`, team, account, kind, id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var granted string
		var generation uint64
		if err = rows.Scan(&granted, &generation); err != nil {
			return d, err
		}
		if permits(kind, granted, permission) {
			d.GrantGeneration = generation
			return d, nil
		}
	}
	if err = rows.Err(); err != nil {
		return d, err
	}
	return d, ErrForbidden
}
func (s *Service) Authorize(ctx context.Context, account, team, kind, id, permission string) (Decision, error) {
	var out Decision
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := Lock(ctx, tx); err != nil {
			return err
		}
		var err error
		out, err = AuthorizeTx(ctx, tx, account, team, kind, id, permission)
		return err
	})
	return out, err
}
func FenceENVTx(ctx context.Context, tx *db.Tx, team string) error {
	_, err := tx.Exec(ctx, `UPDATE environment_vault_teams SET rotation_required=true WHERE team_id=$1`, team)
	if err != nil {
		return err
	}
	return InvalidateENVTx(ctx, tx, team)
}
func InvalidateENVTx(ctx context.Context, tx *db.Tx, team string) error {
	_, err := tx.Exec(ctx, `UPDATE environment_vault_layer_fences SET generation=generation+1 WHERE owner_kind='team' AND owner_id=$1`, team)
	return err
}

// RemoveMemberTx revokes immediately, without requiring keys to finish rekey.
// The caller authorizes the target and advances the team's generation atomically.
func RemoveMemberTx(ctx context.Context, tx *db.Tx, team, account string) error {
	for _, q := range []string{`UPDATE team_resource_bindings SET active=false,generation=generation+1 WHERE team_id=$1 AND resource_kind='terminal_session' AND owner_account=$2 AND active`, `UPDATE team_terminal_session_grants SET active=false,generation=generation+1 WHERE team_id=$1 AND account_id=$2 AND active`, `UPDATE team_resource_bindings b SET active=false,generation=b.generation+1 FROM user_machines m WHERE b.team_id=$1 AND b.resource_kind='machine' AND b.resource_id=m.id AND m.user_id=$2 AND m.owner_team_id IS NULL AND b.active`, `UPDATE team_members SET active=false,membership_generation=membership_generation+1 WHERE team_id=$1 AND account_id=$2 AND active`, `UPDATE team_resource_grants SET active=false,generation=generation+1 WHERE team_id=$1 AND account_id=$2 AND active`, `UPDATE environment_vault_team_members SET grant_epoch=0 WHERE team_id=$1 AND account_id=$2`, `DELETE FROM environment_vault_team_grants WHERE team_id=$1 AND account_id=$2`, `UPDATE team_invitations SET cancelled_at=now() WHERE team_id=$1 AND (recipient_account=$2 OR created_by=$2) AND accepted_at IS NULL AND cancelled_at IS NULL`, `DELETE FROM team_machine_requests WHERE team_id=$1 AND requested_by=$2`} {
		if _, err := tx.Exec(ctx, q, team, account); err != nil {
			return err
		}
	}
	if err := revokeDepartingResourcesTx(ctx, tx, team, account); err != nil {
		return err
	}
	// The member and the personal machines they shared leave the policy for good.
	if err := refreshPolicyTx(ctx, tx, Workspace{TeamID: team}, ""); err != nil {
		return err
	}

	// Shared host layers can be received without an individual Team vault key.
	return FenceENVTx(ctx, tx, team)
}
func replay(ctx context.Context, tx *db.Tx, account, op, kind string, request, out any) (bool, []byte, error) {
	if !validID(account) || !validID(op) {
		return false, nil, ErrInvalid
	}
	raw, err := json.Marshal([]any{kind, request})
	if err != nil {
		return false, nil, err
	}
	hash := sha256.Sum256(raw)
	var digest, data []byte
	err = tx.QueryRow(ctx, `SELECT request_digest,result FROM team_operations WHERE account_id=$1 AND operation_id=$2`, account, op).Scan(&digest, &data)
	if err == pgx.ErrNoRows {
		return false, hash[:], nil
	}
	if err != nil {
		return false, nil, err
	}
	if !bytes.Equal(hash[:], digest) {
		return false, nil, ErrConflict
	}
	if err = json.Unmarshal(data, out); err != nil {
		return false, nil, err
	}
	return true, hash[:], nil
}
func save(ctx context.Context, tx *db.Tx, account, op string, digest []byte, out any) error {
	// Store a bounded receipt; retries read current authorized membership/grants.
	if t, ok := out.(Team); ok {
		t.Members = []Member{}
		t.Grants = []Grant{}
		t.Machines = []MachineBinding{}
		t.MachineGrants = []MachineGrant{}
		t.TerminalSessionGrants = []TerminalSessionGrant{}
		out = t
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if len(raw) > MaximumReceiptBytes {
		return ErrLimit
	}
	if _, err = tx.Exec(ctx, `INSERT INTO team_operations(account_id,operation_id,request_digest,result,created_at) VALUES($1,$2,$3,$4,clock_timestamp())`, account, op, digest, raw); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM team_operations WHERE account_id=$1 AND operation_id IN(SELECT operation_id FROM team_operations WHERE account_id=$1 ORDER BY created_at DESC,operation_id DESC OFFSET $2)`, account, MaximumOperationReceipts); err != nil {
		return err
	}
	team := ""
	switch v := out.(type) {
	case Team:
		team = v.TeamID
	case Invitation:
		team = v.TeamID
	}
	if team != "" {
		return pruneInvitationsTx(ctx, tx, team)
	}
	return nil
}
func advance(ctx context.Context, tx *db.Tx, team string) error {
	_, err := tx.Exec(ctx, `UPDATE teams SET generation=generation+1 WHERE team_id=$1`, team)
	return err
}
func check(t Team, account string, expected uint64) (Member, error) {
	m, ok := member(t, account)
	if t.Deleted || !ok {
		return m, ErrForbidden
	}
	if expected != t.Generation || expected == 0 || expected >= MaximumCounter {
		return m, ErrConflict
	}
	return m, nil
}

// Get and List read one snapshot without the server-wide team lock: a read decides no write, and holding the lock
// made every team change and workspace-scoped request on the server wait for the read.
func (s *Service) Get(ctx context.Context, account, team string) (Team, error) {
	var out Team
	err := s.db.InReadOnlySnapshot(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		out, err = readSnapshotTx(ctx, tx, team)
		if err != nil {
			return err
		}
		if _, ok := currentMember(out, account); !ok || out.Deleted {
			return ErrForbidden
		}
		return nil
	})
	if err != nil || !out.SSORequired {
		return out, err
	}
	// The SSO check records a bypass when it allows one, so it writes in its own transaction.
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error { return SSOGuardTx(ctx, tx, team, account) })
	return out, err
}
func (s *Service) List(ctx context.Context, account string) ([]Team, error) {
	out := []Team{}
	err := s.db.InReadOnlySnapshot(ctx, func(ctx context.Context, tx *db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT t.team_id FROM teams t JOIN team_members m USING(team_id) WHERE m.account_id=$1 AND m.active AND t.deleted_at IS NULL ORDER BY t.team_id LIMIT 129`, account)
		if err != nil {
			return err
		}
		ids := []string{}
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
		if len(ids) > MaximumMembers {
			return ErrLimit
		}
		for _, id := range ids {
			t, err := readSnapshotTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if t.SSORequired {
				var ok bool
				if err = tx.QueryRow(ctx, `SELECT team_member_sso_ok($1,$2)`, id, account).Scan(&ok); err != nil {
					return err
				}
				t.SSOSignedIn = &ok
				if !ok {
					// Without a recent SSO sign-in the team shows only that it needs one.
					t = Team{TeamID: t.TeamID, Name: t.Name, DisplayName: t.DisplayName, Generation: t.Generation, SSORequired: true, SSOSignedIn: &ok,
						Members: []Member{}, Grants: []Grant{}, Machines: []MachineBinding{}, MachineGrants: []MachineGrant{}, MachineRequests: []MachineAdmission{}, TerminalSessionGrants: []TerminalSessionGrant{}}
				}
			}
			out = append(out, t)
		}
		return nil
	})
	return out, err
}

func pruneInvitationsTx(ctx context.Context, tx *db.Tx, team string) error {
	_, err := tx.Exec(ctx, `DELETE FROM team_invitations WHERE team_id=$1 AND invitation_id IN(SELECT invitation_id FROM team_invitations WHERE team_id=$1 AND (accepted_at IS NOT NULL OR cancelled_at IS NOT NULL) ORDER BY COALESCE(accepted_at,cancelled_at) DESC,invitation_id DESC OFFSET $2)`, team, MaximumInvitationHistory)
	return err
}
