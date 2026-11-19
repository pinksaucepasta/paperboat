package planlimits

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Operator entitlements on a workspace (billing_workspace_grants): extra bandwidth, or an override of one plan limit. The
// effective value of every limit comes from this package: the configured plan value, replaced while an override of that
// kind runs. Bandwidth grants are added to the allowance by the readers of the allowance (billing, the exhaustion gate),
// through the SQL function workspace_granted_bytes.
const (
	// Bandwidth is the grant kind for extra hosted bandwidth, in bytes.
	Bandwidth Kind = "bandwidth"
	// Seats overrides a team's purchased seats (what invitations may fill).
	Seats Kind = "seats"
	// ViewersPerTerminal overrides the viewer cap of one shared terminal.
	ViewersPerTerminal Kind = "viewers_per_terminal"
	// SSO and SCIM grant a team the SAML single sign-on or directory sync add-on without a subscription item (amount 1).
	SSO  Kind = "sso"
	SCIM Kind = "scim"
)

// GrantKinds are the kinds an operator can grant on a workspace, with whether it applies to personal and team workspaces.
var GrantKinds = map[Kind]struct{ Personal, Team bool }{
	Bandwidth:          {true, true},
	Machines:           {true, true},
	Publications:       {true, true},
	BrowserTerminals:   {true, true},
	PublicTCP:          {true, true},
	Seats:              {false, true},
	ViewersPerTerminal: {true, true},
	SSO:                {false, true},
	SCIM:               {false, true},
}

// MaxGrantAmount bounds a grant (bytes or a count), far above any real use and far below int64 overflow.
const MaxGrantAmount int64 = 1 << 50

// ErrGrant means the grant is not valid for the workspace; ErrGrantNotFound that there is no such running grant.
var (
	ErrGrant         = errors.New("invalid entitlement grant")
	ErrGrantNotFound = errors.New("entitlement grant not found")
)

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const runningGrant = `revoked_at IS NULL AND (ends_at IS NULL OR ends_at>now())`

// OverrideTx is the running override of kind on the workspace, newest first when an operator left more than one.
func OverrideTx(ctx context.Context, q queryer, workspaceID string, kind Kind) (value int64, ok bool, err error) {
	err = q.QueryRow(ctx, `SELECT amount FROM billing_workspace_grants WHERE workspace_id=$1 AND kind=$2 AND `+runningGrant+` ORDER BY created_at DESC LIMIT 1`, workspaceID, string(kind)).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return value, err == nil, err
}

// effective is the limit in force: the override while one runs, else the plan's value.
func effective(override int64, ok bool, plan int) (limit int, granted bool) {
	if ok {
		return int(min(override, int64(1<<31-1))), true
	}
	return plan, false
}

// ViewersPerSharedTerminalFor is the viewer cap of a shared terminal of the workspace, with its override applied.
func ViewersPerSharedTerminalFor(ctx context.Context, q queryer, scope Scope) (int, error) {
	value, ok, err := OverrideTx(ctx, q, scope.workspaceID(), ViewersPerTerminal)
	if err != nil {
		return 0, err
	}
	limit, _ := effective(value, ok, ViewersPerSharedTerminal())
	return limit, nil
}

// TeamCeilingFor is a team's limit of kind (its hidden ceiling, or the operator's override).
func TeamCeilingFor(ctx context.Context, q queryer, team string, kind Kind) (int, error) {
	value, ok, err := OverrideTx(ctx, q, Scope{Team: team}.workspaceID(), kind)
	if err != nil {
		return 0, err
	}
	limit, _ := effective(value, ok, TeamCeiling(kind))
	return limit, nil
}

// SeatsOverrideTx is the operator's seat override of a team, when one runs.
func SeatsOverrideTx(ctx context.Context, q queryer, team string) (int, bool, error) {
	value, ok, err := OverrideTx(ctx, q, Scope{Team: team}.workspaceID(), Seats)
	return int(min(value, int64(1<<31-1))), ok, err
}

// AddonsTx says whether the team may use SAML single sign-on and directory sync: it has the paid add-on (or its removal has not
// reached the end of the paid period) while its plan is paid, or an operator granted it.
func AddonsTx(ctx context.Context, q queryer, team string) (sso, scim bool, err error) {
	var addon string
	if err = q.QueryRow(ctx, `SELECT coalesce((SELECT w.addon FROM billing_workspaces w WHERE w.team_id=$1 AND w.user_id IS NULL AND w.addon IS NOT NULL
 AND (w.addon_until IS NULL OR w.addon_until>now()) AND team_paid_entitled($1)),'')`, team).Scan(&addon); err != nil {
		return false, false, err
	}
	sso, scim = addon == "sso" || addon == "sso_scim", addon == "scim" || addon == "sso_scim"
	for kind, set := range map[Kind]*bool{SSO: &sso, SCIM: &scim} {
		if _, granted, e := OverrideTx(ctx, q, "team:"+team, kind); e != nil {
			return false, false, e
		} else if granted {
			*set = true
		}
	}
	return sso, scim, nil
}

// Grant is one row of billing_workspace_grants.
type Grant struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Kind        Kind       `json:"kind"`
	Amount      int64      `json:"amount"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
	Reason      string     `json:"reason"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	Running     bool       `json:"running"`
}

// GrantTx gives the workspace an entitlement: extra bandwidth (added to the grants that run) or an override of one limit
// (replacing the one that ran). The workspace's owner account or team must exist and be live. endsAt nil runs until revoked.
func GrantTx(ctx context.Context, tx *db.Tx, actor, workspaceID string, kind Kind, amount int64, endsAt *time.Time, reason string) (Grant, error) {
	scope, ok := scopeOf(workspaceID)
	if !ok {
		return Grant{}, ErrGrant
	}
	allowed, known := GrantKinds[kind]
	if !known || (scope.Team != "" && !allowed.Team) || (scope.Team == "" && !allowed.Personal) || amount > MaxGrantAmount || amount < 1 {
		return Grant{}, ErrGrant
	}
	if endsAt != nil && !endsAt.After(time.Now()) {
		return Grant{}, ErrGrant
	}
	var live bool
	query, arg := `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active')`, scope.Account
	if scope.Team != "" {
		query, arg = `SELECT EXISTS(SELECT 1 FROM teams WHERE team_id=$1 AND deleted_at IS NULL AND NOT suspended)`, scope.Team
	}
	if err := tx.QueryRow(ctx, query, arg).Scan(&live); err != nil {
		return Grant{}, err
	}
	if !live {
		return Grant{}, ErrGrantNotFound
	}
	// One running override per workspace and kind: a new one replaces the old. Bandwidth grants add up.
	if kind != Bandwidth {
		// Two operators overriding the same limit at once would otherwise both pass the UPDATE and leave two rows running.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "grant:"+workspaceID+":"+string(kind)); err != nil {
			return Grant{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_workspace_grants SET revoked_at=now(),revoked_by=$3 WHERE workspace_id=$1 AND kind=$2 AND `+runningGrant, workspaceID, string(kind), actor); err != nil {
			return Grant{}, err
		}
	}
	g := Grant{ID: "grant_" + uuid.NewString(), WorkspaceID: workspaceID, Kind: kind, Amount: amount, EndsAt: endsAt, Reason: reason, CreatedBy: actor, Running: true}
	err := tx.QueryRow(ctx, `INSERT INTO billing_workspace_grants(id,workspace_id,kind,amount,ends_at,reason,created_by) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`, g.ID, workspaceID, string(kind), amount, endsAt, reason, actor).Scan(&g.CreatedAt)
	return g, err
}

// RevokeTx ends a running grant of the workspace now. A grant that ended or was revoked already is ErrGrantNotFound.
func RevokeTx(ctx context.Context, tx *db.Tx, actor, workspaceID, grantID string) (Grant, error) {
	var g Grant
	var kind string
	err := tx.QueryRow(ctx, `UPDATE billing_workspace_grants SET revoked_at=now(),revoked_by=$3 WHERE id=$2 AND workspace_id=$1 AND `+runningGrant+` RETURNING id,workspace_id,kind,amount,ends_at,reason,created_by,created_at,revoked_at`, workspaceID, grantID, actor).
		Scan(&g.ID, &g.WorkspaceID, &kind, &g.Amount, &g.EndsAt, &g.Reason, &g.CreatedBy, &g.CreatedAt, &g.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrGrantNotFound
	}
	g.Kind = Kind(kind)
	return g, err
}

// GrantsTx lists the workspace's grants, running ones first and then the latest that ended, at most 200.
func GrantsTx(ctx context.Context, tx *db.Tx, workspaceID string) ([]Grant, error) {
	rows, err := tx.Query(ctx, `SELECT id,workspace_id,kind,amount,ends_at,reason,created_by,created_at,revoked_at,(`+runningGrant+`) FROM billing_workspace_grants WHERE workspace_id=$1 ORDER BY (`+runningGrant+`) DESC,created_at DESC LIMIT 200`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		var g Grant
		var kind string
		if err := rows.Scan(&g.ID, &g.WorkspaceID, &kind, &g.Amount, &g.EndsAt, &g.Reason, &g.CreatedBy, &g.CreatedAt, &g.RevokedAt, &g.Running); err != nil {
			return nil, err
		}
		g.Kind = Kind(kind)
		out = append(out, g)
	}
	return out, rows.Err()
}

func scopeOf(workspaceID string) (Scope, bool) {
	switch {
	case len(workspaceID) > len("personal:") && workspaceID[:len("personal:")] == "personal:":
		return Scope{Account: workspaceID[len("personal:"):]}, true
	case len(workspaceID) > len("team:") && workspaceID[:len("team:")] == "team:":
		return Scope{Team: workspaceID[len("team:"):]}, true
	}
	return Scope{}, false
}

// LimitRow is one counted kind of a workspace with its plan value and the override that replaces it; the operator's view.
type LimitRow struct {
	Kind     Kind `json:"kind"`
	Used     int  `json:"used"`
	Plan     int  `json:"plan"`
	Override *int `json:"override,omitempty"`
	// Limit is what is enforced: nil only for a team without an override (advertised unlimited) or a self-hosted public TCP edge.
	Limit *int `json:"limit"`
}

// LimitsTx reports the counted kinds of the workspace with plan value, override and enforced limit.
func LimitsTx(ctx context.Context, tx *db.Tx, workspaceID string) ([]LimitRow, error) {
	scope, ok := scopeOf(workspaceID)
	if !ok {
		return nil, ErrGrant
	}
	plan, err := planTx(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	usage, err := UsageTx(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	set := current.Load()
	out := []LimitRow{}
	for _, kind := range []Kind{Machines, Publications, BrowserTerminals, PublicTCP} {
		row := LimitRow{Kind: kind, Used: usage[kind].Used, Limit: usage[kind].Limit}
		if kind == PublicTCP {
			row.Plan, _, err = publicTCPLimitTx(ctx, tx, set.limits.PublicTCP, scope, plan)
			if err != nil {
				return nil, err
			}
		} else {
			row.Plan = limitFor(set.limits, kind, plan)
		}
		if usage[kind].Granted && usage[kind].Limit != nil {
			v := *usage[kind].Limit
			row.Override = &v
		}
		out = append(out, row)
	}
	return out, nil
}
