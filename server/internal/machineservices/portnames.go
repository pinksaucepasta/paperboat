package machineservices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
)

// ErrNameIsMachine refuses a top-level name equal to a machine's name: <name>.<domain> already opens that machine.
var ErrNameIsMachine = errors.New("top-level name is a machine name")

// ErrPortNameNotFound means there is no such name in the selected workspace.
var ErrPortNameNotFound = errors.New("port name not found")

// MaxPortNames bounds the names of one workspace (machine, all-machines and top-level names together).
const MaxPortNames = 512

// workspaceMachines selects the machines a workspace owns ($1 actor, $2 team or ”): the actor's personal machines,
// or the team's machines.
const workspaceMachines = `SELECT m.id,m.alias FROM paperboat.user_machines m WHERE m.deleted_at IS NULL AND m.revoked_at IS NULL
 AND CASE WHEN $2='' THEN m.user_id=$1 AND m.owner_team_id IS NULL ELSE m.owner_team_id=$2 END`

// ListPortNames lists the names of the selected workspace: machine names of its machines, its all-machines names
// and its top-level names (shadowed when a machine of the workspace has that name).
func (s *Service) ListPortNames(ctx context.Context, actor string, scope workspace.Scope) (out []wire.PortNameRecord, resultErr error) {
	rows, e := s.DB.QueryContext(ctx, `WITH ws AS (`+workspaceMachines+`)
SELECT n.id,n.scope,n.name,n.port,coalesce(n.machine_id,''),coalesce(ws.alias,''),
 n.scope='top_level' AND EXISTS(SELECT 1 FROM ws other WHERE other.alias=n.name)
FROM paperboat.machine_port_names n LEFT JOIN ws ON ws.id=n.machine_id
WHERE (n.scope='machine' AND ws.id IS NOT NULL)
 OR (n.scope<>'machine' AND CASE WHEN $2='' THEN n.owner_account_id=$1 ELSE n.owner_team_id=$2 END)
ORDER BY n.scope,ws.alias,n.name LIMIT $3`, actor, scope.TeamID, MaxPortNames)
	if e != nil {
		return nil, e
	}
	defer func() { resultErr = withServiceCleanupFailure(resultErr, rows.Close()) }()
	out = []wire.PortNameRecord{}
	for rows.Next() {
		var r wire.PortNameRecord
		if e = rows.Scan(&r.ID, &r.Scope, &r.Name, &r.Port, &r.MachineID, &r.MachineAlias, &r.Shadowed); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetPortName creates a name, or moves an existing name of the same scope and target to another port. Machine
// managers set machine names; workspace owners and admins set all-machines and top-level names.
func (s *Service) SetPortName(ctx context.Context, actor string, scope workspace.Scope, in wire.PortNameSet) (wire.PortNameRecord, error) {
	var out wire.PortNameRecord
	err := db.RetrySerializable(ctx, func() (e error) {
		out, e = s.setPortNameAttempt(ctx, actor, scope, in)
		return e
	})
	return out, err
}

func (s *Service) setPortNameAttempt(ctx context.Context, actor string, scope workspace.Scope, in wire.PortNameSet) (out wire.PortNameRecord, resultErr error) {
	if !wire.ValidPortName(in.Name) || in.Port < 1 || in.Port > 65535 {
		return out, wire.ErrInvalid
	}
	tx, e := db.BeginSerializable(ctx, s.DB)
	if e != nil {
		return out, e
	}
	defer captureServiceRollback(tx, &resultErr)
	out = wire.PortNameRecord{Scope: in.Scope, Name: in.Name, Port: in.Port, MachineID: in.MachineID}
	switch in.Scope {
	case wire.ScopeMachine:
		if e = tx.QueryRowContext(ctx, `SELECT alias FROM paperboat.user_machines WHERE id=$1 AND deleted_at IS NULL AND paperboat.machine_management_allowed($2,id)`, in.MachineID, actor).Scan(&out.MachineAlias); machineServiceRowMissing(e) {
			return out, ErrDenied
		} else if e != nil {
			return out, e
		}
		e = tx.QueryRowContext(ctx, `INSERT INTO paperboat.machine_port_names(scope,name,port,machine_id,created_by) VALUES ('machine',$1,$2,$3,$4)
 ON CONFLICT (machine_id,name) WHERE scope='machine' DO UPDATE SET port=EXCLUDED.port RETURNING id`, in.Name, in.Port, in.MachineID, actor).Scan(&out.ID)
	case wire.ScopeAllMachines, wire.ScopeTopLevel:
		if e = requireWorkspaceAdmin(ctx, tx, actor, scope); e != nil {
			return out, e
		}
		if in.Scope == wire.ScopeAllMachines {
			if in.MachineID != "" {
				return out, wire.ErrInvalid
			}
		} else {
			if in.Name == "gateway" {
				return out, wire.ErrInvalid // gateway.<domain> is the local browser gateway's own name
			}
			var isMachine bool
			e = tx.QueryRowContext(ctx, `WITH ws AS (`+workspaceMachines+`) SELECT coalesce((SELECT alias FROM ws WHERE id=$3),''),EXISTS(SELECT 1 FROM ws WHERE alias=$4)`, actor, scope.TeamID, in.MachineID, in.Name).Scan(&out.MachineAlias, &isMachine)
			if e != nil {
				return out, e
			}
			if out.MachineAlias == "" {
				return out, ErrDenied
			}
			if isMachine {
				return out, ErrNameIsMachine
			}
		}
		var count int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM paperboat.machine_port_names WHERE CASE WHEN $2='' THEN owner_account_id=$1 ELSE owner_team_id=$2 END`, actor, scope.TeamID).Scan(&count); e != nil {
			return out, e
		}
		if count >= MaxPortNames {
			return out, wire.ErrInvalid
		}
		e = tx.QueryRowContext(ctx, `INSERT INTO paperboat.machine_port_names(scope,name,port,machine_id,owner_account_id,owner_team_id,created_by)
 VALUES ($1,$2,$3,nullif($4,''),CASE WHEN $5='' THEN $6 END,nullif($5,''),$6)
 ON CONFLICT (scope,coalesce(owner_team_id,owner_account_id),name) WHERE scope<>'machine' DO UPDATE SET port=EXCLUDED.port,machine_id=EXCLUDED.machine_id RETURNING id`,
			in.Scope, in.Name, in.Port, in.MachineID, scope.TeamID, actor).Scan(&out.ID)
	default:
		return out, wire.ErrInvalid
	}
	if e != nil {
		return out, e
	}
	if e = publishPortNames(ctx, tx, actor, scope, in.Scope, in.MachineID); e != nil {
		return out, e
	}
	return out, tx.Commit()
}

// DeletePortName removes one name of the selected workspace, under the same permission rule as SetPortName.
func (s *Service) DeletePortName(ctx context.Context, actor string, scope workspace.Scope, id string) error {
	return db.RetrySerializable(ctx, func() error { return s.deletePortNameAttempt(ctx, actor, scope, id) })
}

func (s *Service) deletePortNameAttempt(ctx context.Context, actor string, scope workspace.Scope, id string) (resultErr error) {
	tx, e := db.BeginSerializable(ctx, s.DB)
	if e != nil {
		return e
	}
	defer captureServiceRollback(tx, &resultErr)
	var nameScope, machine string
	e = tx.QueryRowContext(ctx, `WITH ws AS (`+workspaceMachines+`) SELECT n.scope,coalesce(n.machine_id,'') FROM paperboat.machine_port_names n WHERE n.id=$3
 AND (n.scope='machine' AND n.machine_id IN (SELECT id FROM ws) OR n.scope<>'machine' AND CASE WHEN $2='' THEN n.owner_account_id=$1 ELSE n.owner_team_id=$2 END)`, actor, scope.TeamID, id).Scan(&nameScope, &machine)
	if machineServiceRowMissing(e) {
		return ErrPortNameNotFound
	} else if e != nil {
		return e
	}
	if nameScope == wire.ScopeMachine {
		var allowed bool
		if e = tx.QueryRowContext(ctx, `SELECT paperboat.machine_management_allowed($1,$2)`, actor, machine).Scan(&allowed); e != nil {
			return e
		}
		if !allowed {
			return ErrDenied
		}
	} else if e = requireWorkspaceAdmin(ctx, tx, actor, scope); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM paperboat.machine_port_names WHERE id=$1`, id); e != nil {
		return e
	}
	if e = publishPortNames(ctx, tx, actor, scope, nameScope, machine); e != nil {
		return e
	}
	return tx.Commit()
}

// GetFrontDesk returns a machine's front desk (port 80 over http when none is set) to its managers.
func (s *Service) GetFrontDesk(ctx context.Context, actor, machine string) (wire.FrontDesk, error) {
	desk := wire.DefaultFrontDesk
	e := s.DB.QueryRowContext(ctx, `SELECT coalesce(f.port,80),coalesce(f.scheme,'http') FROM paperboat.user_machines m LEFT JOIN paperboat.machine_front_desks f ON f.machine_id=m.id
 WHERE m.id=$1 AND m.deleted_at IS NULL AND paperboat.machine_management_allowed($2,m.id)`, machine, actor).Scan(&desk.Port, &desk.Scheme)
	if machineServiceRowMissing(e) {
		return desk, ErrDenied
	}
	return desk, e
}

// SetFrontDesk sets where an unmatched <name>.<machine>.<domain> goes. Machine managers only.
func (s *Service) SetFrontDesk(ctx context.Context, actor, machine string, desk wire.FrontDesk) error {
	if !wire.ValidFrontDesk(desk) {
		return wire.ErrInvalid
	}
	return db.RetrySerializable(ctx, func() (resultErr error) {
		tx, e := db.BeginSerializable(ctx, s.DB)
		if e != nil {
			return e
		}
		defer captureServiceRollback(tx, &resultErr)
		var allowed bool
		e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.user_machines WHERE id=$1 AND deleted_at IS NULL AND paperboat.machine_management_allowed($2,id))`, machine, actor).Scan(&allowed)
		if e != nil {
			return e
		}
		if !allowed {
			return ErrDenied
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO paperboat.machine_front_desks(machine_id,port,scheme,updated_by) VALUES ($1,$2,$3,$4)
 ON CONFLICT (machine_id) DO UPDATE SET port=EXCLUDED.port,scheme=EXCLUDED.scheme,updated_by=EXCLUDED.updated_by,updated_at=now()`, machine, desk.Port, desk.Scheme, actor); e != nil {
			return e
		}
		if e = publishPortNames(ctx, tx, actor, workspace.Scope{}, wire.ScopeMachine, machine); e != nil {
			return e
		}
		return tx.Commit()
	})
}

// requireWorkspaceAdmin: the personal workspace belongs to the actor; a team's to its owner and admins.
func requireWorkspaceAdmin(ctx context.Context, tx *sql.Tx, actor string, scope workspace.Scope) error {
	if scope.TeamID == "" {
		return nil
	}
	var allowed bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.teams t JOIN paperboat.team_members member USING(team_id) WHERE t.team_id=$2 AND t.deleted_at IS NULL
 AND member.account_id=$1 AND member.active AND (t.owner_account=$1 OR member.role='admin'))`, actor, scope.TeamID).Scan(&allowed); e != nil {
		return e
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

// publishPortNames moves the machine_services topic of everyone who may list the affected machines, so their
// daemons fetch the new names at once: one machine for a machine name or front desk, else the whole workspace.
func publishPortNames(ctx context.Context, tx *sql.Tx, actor string, scope workspace.Scope, nameScope, machine string) error {
	if nameScope == wire.ScopeMachine {
		_, e := tx.ExecContext(ctx, `SELECT paperboat.publish_account_topic(paperboat.machine_audience_accounts($1),'machine_services')`, machine)
		return e
	}
	_, e := tx.ExecContext(ctx, `WITH ws AS (`+workspaceMachines+`)
SELECT paperboat.publish_account_topic(ARRAY(SELECT DISTINCT a FROM ws, unnest(paperboat.machine_audience_accounts(ws.id)) a),'machine_services')`, actor, scope.TeamID)
	return e
}

// attachPortNames adds each listed machine's names and front desk, keeping only names of listed ports: a name
// grants no access. A machine name beats an all-machines name of the same label.
func attachPortNames(ctx context.Context, tx *sql.Tx, items []wire.Item) (resultErr error) {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, len(items))
	index := make(map[string]int, len(items))
	for i := range items {
		ids[i], index[items[i].MachineID] = items[i].MachineID, i
		items[i].PortNames, items[i].TopLevelNames, items[i].FrontDesk = []wire.PortName{}, []wire.PortName{}, wire.DefaultFrontDesk
	}
	encoded, e := json.Marshal(ids)
	if e != nil {
		return e
	}
	rows, e := tx.QueryContext(ctx, `WITH listed AS (SELECT m.id,m.user_id,m.owner_team_id FROM paperboat.user_machines m WHERE m.id IN (SELECT jsonb_array_elements_text($1::jsonb)))
SELECT l.id,n.scope,n.name,n.port FROM listed l JOIN paperboat.machine_port_names n ON n.scope IN ('machine','top_level') AND n.machine_id=l.id
UNION ALL SELECT l.id,n.scope,n.name,n.port FROM listed l JOIN paperboat.machine_port_names n ON n.scope='all_machines'
 AND CASE WHEN l.owner_team_id IS NULL THEN n.owner_account_id=l.user_id ELSE n.owner_team_id=l.owner_team_id END
ORDER BY 1,2 DESC,3`, encoded)
	if e != nil {
		return e
	}
	defer func() { resultErr = withServiceCleanupFailure(resultErr, rows.Close()) }()
	named := make(map[string]map[string]bool, len(items))
	for rows.Next() {
		var machine, nameScope string
		var n wire.PortName
		if e = rows.Scan(&machine, &nameScope, &n.Name, &n.Port); e != nil {
			return e
		}
		item := &items[index[machine]]
		if !slicesContainsPort(item.Services, n.Port) {
			continue
		}
		if nameScope == wire.ScopeTopLevel {
			item.TopLevelNames = append(item.TopLevelNames, n)
			continue
		}
		// Rows of one machine come with 'machine' before 'all_machines' (scope DESC), so the first label wins.
		if named[machine] == nil {
			named[machine] = map[string]bool{}
		}
		if !named[machine][n.Name] {
			named[machine][n.Name] = true
			item.PortNames = append(item.PortNames, n)
		}
	}
	if e = rows.Err(); e != nil {
		return e
	}
	desks, e := tx.QueryContext(ctx, `SELECT machine_id,port,scheme FROM paperboat.machine_front_desks WHERE machine_id IN (SELECT jsonb_array_elements_text($1::jsonb))`, encoded)
	if e != nil {
		return e
	}
	defer func() { resultErr = withServiceCleanupFailure(resultErr, desks.Close()) }()
	for desks.Next() {
		var machine string
		var desk wire.FrontDesk
		if e = desks.Scan(&machine, &desk.Port, &desk.Scheme); e != nil {
			return e
		}
		items[index[machine]].FrontDesk = desk
	}
	return desks.Err()
}

func slicesContainsPort(ports []wire.PublishedPort, port int) bool {
	for _, p := range ports {
		if p.Port == port {
			return true
		}
	}
	return false
}
