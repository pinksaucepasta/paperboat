package usermachines

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

type InventoryOptions struct {
	Limit, Offset   int
	Q, State, Owner string
	Closed          *bool
	MachineID       string // the terminal catalog's machine filter
	// After is the machine list's keyset cursor: the page starts after this (lowercased alias, id).
	After *InventoryPosition
	// Platform (linux, darwin, windows) and Tag (a workspace policy tag, without "tag:") narrow the machine list.
	Platform, Tag string
	// WithoutLiveMetadata skips the terminal catalog's live host enrichment (current directory,
	// foreground process, participants) for callers that need only the stored rows.
	WithoutLiveMetadata bool
}

// InventoryPosition is a machine list keyset position: the sort key (lowercased alias) and the id.
type InventoryPosition struct{ Key, ID string }

// ListInventory selects the authorized filtered page before describing runtime access; more reports whether
// machines follow the page (by offset or after a cursor).
func (s *Service) ListInventory(ctx context.Context, account string, opts InventoryOptions) (items []UserMachine, total int, more bool, err error) {
	if opts.Limit < 1 || opts.Limit > 200 || opts.Offset < 0 || (opts.After != nil && opts.Offset != 0) {
		return nil, 0, false, ErrInvalidMachineName
	}
	scope, scoped := workspace.FromContext(ctx)
	if scoped {
		if err := workspace.Validate(ctx, s.db, account, scope); err != nil {
			return nil, 0, false, err
		}
	}
	items, total, err = s.inventoryFlights.do(ctx, inventoryKey{account: account, scoped: scoped, scope: scope, opts: opts.key()}, func(ctx context.Context) ([]UserMachine, int, error) {
		return s.listInventory(ctx, account, opts)
	})
	if err != nil {
		return nil, 0, false, err
	}
	// listInventory reads one machine past the page to tell whether more follow.
	if len(items) > opts.Limit {
		return items[:opts.Limit], total, true, nil
	}
	return items, total, false, nil
}

// listInventory computes one inventory page; ListInventory shares it between identical requests.
func (s *Service) listInventory(ctx context.Context, account string, opts InventoryOptions) ([]UserMachine, int, error) {
	with, predicate, args := inventoryFilter(ctx, account, opts, false)
	// One pass computes the page and the total; only a page past the end (offset > 0, no rows) needs a count.
	// The account's own personal machines short-circuit the access functions with their first branch (own,
	// personal, not deleted), which is what they would return: the functions cost ~1 ms per machine under load.
	total := -1
	// The window counts the whole filtered set before the cursor narrows it, so the total is the same on every page.
	var afterKey, afterID any
	if opts.After != nil {
		afterKey, afterID = opts.After.Key, opts.After.ID
	}
	n := len(args)
	page := fmt.Sprintf(`) AS x WHERE $%d::text IS NULL OR (x.k,x.id) > ($%d::text,$%d::text) ORDER BY x.k,x.id LIMIT $%d OFFSET $%d`, n+1, n+1, n+2, n+3, n+4)
	rows, err := s.db.SQL().QueryContext(ctx, with+`SELECT x.id,x.total FROM (SELECT m.id,lower(m.alias) AS k,count(*) OVER () AS total`+predicate+page,
		append(args, afterKey, afterID, opts.Limit+1, opts.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id, &total); err != nil {
			return nil, 0, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, 0, err
	}
	if total < 0 && opts.Offset == 0 && opts.After == nil {
		total = 0
	} else if total < 0 {
		if err := s.db.SQL().QueryRowContext(ctx, with+"SELECT count(*)"+predicate, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	// One query for the rows and one for their access description, not a Get (two queries, ~15 function calls
	// per machine) for each of up to 200 machines: an account with ~50 machines paid seconds per list.
	loaded, err := s.db.Queries().ListUserMachinesByIDs(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	byID := make(map[string]dbsqlc.UserMachine, len(loaded))
	for _, row := range loaded {
		byID[row.ID] = row
	}
	out := make([]UserMachine, 0, len(ids))
	for _, id := range ids {
		row, ok := byID[id]
		if !ok {
			return nil, 0, fmt.Errorf("machine inventory changed: %w", ErrNotFound)
		}
		out = append(out, mapMachine(row))
	}
	items := make([]*UserMachine, len(out))
	for i := range out {
		items[i] = &out[i]
	}
	if err := s.describeMachineAccess(ctx, account, items...); err != nil {
		return nil, 0, err
	}
	if err := s.describeManagedSSH(ctx, account, items...); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (s *Service) ListTerminalSessionInventory(ctx context.Context, account, machineID string, opts InventoryOptions) ([]TerminalSession, int, error) {
	if opts.Limit < 1 || opts.Limit > 200 || opts.Offset < 0 {
		return nil, 0, ErrTerminalSessionInvalidName
	}
	machine, err := s.Get(ctx, account, machineID)
	if err != nil {
		return nil, 0, err
	}
	// Each listed session is loaded through the terminal capability check; a machine the
	// caller may not use for terminals (revoked, capability off) lists none instead of
	// selecting ids it then cannot load (a revoked machine's daemon got 500s polling this).
	var terminalAllowed bool
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT paperboat.machine_capability_allowed($1,$2,'terminal')`, account, machineID).Scan(&terminalAllowed); err != nil {
		return nil, 0, err
	}
	if !terminalAllowed {
		return []TerminalSession{}, 0, nil
	}
	scoped, team := workspaceTeamArgument(ctx)
	var teamID any
	if team.Valid {
		teamID = team.String
	}
	var closed any
	if opts.Closed != nil {
		closed = *opts.Closed
	}
	predicate := ` FROM user_machine_terminal_sessions t WHERE t.user_machine_id=$1 AND t.owner_account=$2 AND t.deleted_at IS NULL
 AND (NOT $3::boolean OR t.owner_team_id IS NOT DISTINCT FROM $4::text)
 AND ($5='' OR position(lower($5) in lower(t.name||' '||t.id))>0)
 AND ($6='' OR CASE WHEN t.desired_state<>'open' OR t.runtime_state IN ('closed','exited','deleted') THEN 'closed' WHEN t.runtime_state='running' THEN 'running' ELSE 'unavailable' END=$6)
 AND ($7='' OR $7='mine' OR t.owner_account=$7)
 AND ($8::boolean IS NULL OR (t.desired_state<>'open' OR t.runtime_state IN ('closed','exited','deleted'))=$8)`
	args := []any{machineID, account, scoped, teamID, opts.Q, opts.State, opts.Owner, closed}
	// Count, selected IDs and durable records belong to one inventory snapshot.
	tx, err := s.db.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)
	var total int
	if err = tx.QueryRow(ctx, "SELECT count(*)"+predicate, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(ctx, "SELECT t.id"+predicate+" ORDER BY t.is_default DESC,t.last_activity_at DESC NULLS LAST,t.id LIMIT $9 OFFSET $10", append(args, opts.Limit, opts.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	records, err := listTerminalSessions(ctx, dbsqlc.New(tx), ids, machineID, account)
	if err != nil {
		return nil, 0, err
	}
	out := make([]TerminalSession, 0, len(records))
	for _, row := range records {
		item := mapTerminalSession(row)
		item.Machine = machine.Alias
		out = append(out, item)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	for index, live := range s.terminalInventoryObservations(ctx, account, ids) {
		if live == nil {
			continue
		}
		item := &out[index]
		item.Title, item.StartedIn, item.State, item.LastActiveAt, item.ExpiresAt = live.Title, live.StartedIn, live.State, live.LastActiveAt, live.ExpiresAt
		item.CurrentDirectory, item.ForegroundProcess = live.CurrentDirectory, live.ForegroundProcess
		if live.ParticipantsAvailable {
			count := len(live.Participants)
			item.AttachedCount = &count
		}
	}
	return out, total, nil
}

// inventoryFilter is the machine list's authorization and filters ($1..$6) as a WITH clause and a FROM/WHERE
// predicate over user_machines m. withRevoked keeps revoked and disconnected machines without a state filter.
func inventoryFilter(ctx context.Context, account string, opts InventoryOptions, withRevoked bool) (with, predicate string, args []any) {
	scoped, team := workspaceTeamArgument(ctx)
	var teamID any
	if team.Valid {
		teamID = team.String
	}
	// The candidates are a set-based superset of every access rule below: own personal machines, machines owned by a
	// team the account owns or administers (management), and team machines that a policy allow rule for one of the
	// six capabilities may give the account (any role, account or group subject; denies left to the functions).
	// The access functions then decide each candidate. Without candidates they ran for every machine in the database
	// (28 s at 5k machines); with all of a team's machines as candidates, the ones the account cannot use failed
	// seven function calls each (1.5-4 s per list on production, the largest database load).
	with = `WITH rules AS (
 SELECT tm.team_id,r.policy_id,r.target_kind,r.target_id FROM team_members tm
 JOIN workspace_policies p ON p.team_id=tm.team_id
 JOIN workspace_policy_rules r ON r.policy_id=p.id AND NOT r.deny AND r.action IN ('terminal','exec','managed_ssh','files','preview_manage','tunnel_manage')
 WHERE tm.account_id=$1 AND tm.active AND (r.subject_kind IN ('all','role') OR (r.subject_kind='account' AND r.subject_id=$1)
  OR (r.subject_kind='group' AND EXISTS(SELECT 1 FROM workspace_policy_groups g WHERE g.policy_id=r.policy_id AND g.group_name=r.subject_id AND g.account_id=$1)))
), candidates AS (
 SELECT m.id FROM user_machines m WHERE m.user_id=$1 AND m.owner_team_id IS NULL
 UNION SELECT m.id FROM teams t JOIN team_members tm ON tm.team_id=t.team_id AND tm.account_id=$1 AND tm.active AND (t.owner_account=$1 OR tm.role='admin')
  JOIN user_machines m ON m.owner_team_id=t.team_id
 UNION SELECT b.resource_id FROM rules r JOIN team_resource_bindings b ON b.team_id=r.team_id AND b.resource_kind='machine' AND b.active AND b.resource_id=r.target_id WHERE r.target_kind='machine'
 UNION SELECT b.resource_id FROM rules r JOIN workspace_policy_tags tg ON tg.policy_id=r.policy_id AND tg.tag=r.target_id
  JOIN team_resource_bindings b ON b.team_id=r.team_id AND b.resource_kind='machine' AND b.active AND b.resource_id=tg.machine_id WHERE r.target_kind='tag'
 UNION SELECT b.resource_id FROM rules r JOIN team_resource_bindings b ON b.team_id=r.team_id AND b.resource_kind='machine' AND b.active WHERE r.target_kind='all_machines'
) `
	predicate = ` FROM user_machines m WHERE m.deleted_at IS NULL AND m.id IN (SELECT id FROM candidates)
 AND (
 (NOT $2::boolean AND ((m.owner_team_id IS NULL AND m.user_id=$1) OR machine_management_allowed($1,m.id) OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE machine_capability_allowed($1,m.id,cap))))
 OR ($2::boolean AND $3::text IS NULL AND m.user_id=$1 AND m.owner_team_id IS NULL)
 OR ($2::boolean AND $3::text IS NOT NULL AND EXISTS(SELECT 1 FROM teams t JOIN team_members member USING(team_id) WHERE t.team_id=$3 AND t.deleted_at IS NULL AND NOT t.suspended AND member.account_id=$1 AND member.active)
 AND ((m.owner_team_id=$3 AND machine_management_allowed($1,m.id)) OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE team_machine_capability_allowed($1,$3,m.id,cap)))))
 AND ($4='' OR position(lower($4) in lower(m.alias||' '||m.id||' '||coalesce((SELECT '127.100.'||(l.address_slot/256)||'.'||(l.address_slot%256)
  FROM control_sync_leases l WHERE l.account_id=$1 AND l.machine_id=m.id),'')))>0 OR lower(m.alias)=split_part(lower($4),'.',1))
 AND ($5='' OR ($5='revoked' AND (m.revoked_at IS NOT NULL OR m.state IN ('revoked','disconnected'))) OR ($5<>'revoked' AND m.state=$5))`
	if !withRevoked {
		// Without a state filter the list is the machines in use: revoked and disconnected ones (revoked_at set)
		// are listed only when asked for by state.
		predicate += ` AND ($5<>'' OR (m.revoked_at IS NULL AND m.state NOT IN ('revoked','disconnected')))`
	}
	predicate += `
 AND ($6='' OR ($6='mine' AND m.user_id=$1 AND m.owner_team_id IS NULL) OR ($6='shared' AND (m.user_id<>$1 OR m.owner_team_id IS NOT NULL)) OR m.user_id=$6)
 AND ($7='' OR m.platform=$7)
 AND ($8='' OR EXISTS(SELECT 1 FROM workspace_policy_tags tg JOIN workspace_policies p ON p.id=tg.policy_id
  WHERE tg.machine_id=m.id AND tg.tag=$8 AND (($3::text IS NOT NULL AND p.team_id=$3)
   OR ($3::text IS NULL AND p.team_id IS NULL AND p.owner_account=$1)
   OR (NOT $2::boolean AND p.team_id IN (SELECT team_id FROM team_members WHERE account_id=$1 AND active)))))`
	return with, predicate, []any{account, scoped, teamID, opts.Q, opts.State, opts.Owner, opts.Platform, opts.Tag}
}
