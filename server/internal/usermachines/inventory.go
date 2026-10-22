package usermachines

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

type InventoryOptions struct {
	Limit, Offset   int
	Q, State, Owner string
	Closed          *bool
	// WithoutLiveMetadata skips the terminal catalog's live host enrichment (current directory,
	// foreground process, participants) for callers that need only the stored rows.
	WithoutLiveMetadata bool
}

// ListInventory selects the authorized filtered page before describing runtime access.
func (s *Service) ListInventory(ctx context.Context, account string, opts InventoryOptions) ([]UserMachine, int, error) {
	if opts.Limit < 1 || opts.Limit > 200 || opts.Offset < 0 {
		return nil, 0, ErrInvalidMachineName
	}
	if scope, ok := workspace.FromContext(ctx); ok {
		if err := workspace.Validate(ctx, s.db, account, scope); err != nil {
			return nil, 0, err
		}
	}
	scoped, team := workspaceTeamArgument(ctx)
	var teamID any
	if team.Valid {
		teamID = team.String
	}
	// The candidate clause is a superset of every access rule below (own machines, machines owned by or bound to a
	// team the account is an active member of) and is served by indexes. Without it the access functions ran for
	// every machine in the database: 28 s for one list at 5k machines.
	predicate := ` FROM user_machines m WHERE m.deleted_at IS NULL
 AND (m.user_id=$1 OR m.owner_team_id IN (SELECT tm.team_id FROM team_members tm WHERE tm.account_id=$1 AND tm.active)
  OR m.id IN (SELECT b.resource_id FROM team_members tm JOIN team_resource_bindings b ON b.team_id=tm.team_id AND b.resource_kind='machine' WHERE tm.account_id=$1 AND tm.active))
 AND (
 (NOT $2::boolean AND ((m.owner_team_id IS NULL AND m.user_id=$1) OR machine_management_allowed($1,m.id) OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE machine_capability_allowed($1,m.id,cap))))
 OR ($2::boolean AND $3::text IS NULL AND m.user_id=$1 AND m.owner_team_id IS NULL)
 OR ($2::boolean AND $3::text IS NOT NULL AND EXISTS(SELECT 1 FROM teams t JOIN team_members member USING(team_id) WHERE t.team_id=$3 AND t.deleted_at IS NULL AND NOT t.suspended AND member.account_id=$1 AND member.active)
 AND ((m.owner_team_id=$3 AND machine_management_allowed($1,m.id)) OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE team_machine_capability_allowed($1,$3,m.id,cap)))))
 AND ($4='' OR position(lower($4) in lower(m.alias||' '||m.description||' '||m.id))>0)
 AND ($5='' OR m.state=$5)
 AND ($6='' OR ($6='mine' AND m.user_id=$1 AND m.owner_team_id IS NULL) OR ($6='shared' AND (m.user_id<>$1 OR m.owner_team_id IS NOT NULL)) OR m.user_id=$6)`
	args := []any{account, scoped, teamID, opts.Q, opts.State, opts.Owner}
	// One pass computes the page and the total; only a page past the end (offset > 0, no rows) needs a count.
	// The account's own personal machines short-circuit the access functions with their first branch (own,
	// personal, not deleted), which is what they would return: the functions cost ~1 ms per machine under load.
	total := -1
	rows, err := s.db.SQL().QueryContext(ctx, "SELECT m.id,count(*) OVER ()"+predicate+" ORDER BY lower(m.alias),m.id LIMIT $7 OFFSET $8", append(args, opts.Limit, opts.Offset)...)
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
	if total < 0 && opts.Offset == 0 {
		total = 0
	} else if total < 0 {
		if err := s.db.SQL().QueryRowContext(ctx, "SELECT count(*)"+predicate, args...).Scan(&total); err != nil {
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
	out := make([]TerminalSession, 0, len(ids))
	for _, id := range ids {
		row, e := getTerminalSession(ctx, dbsqlc.New(tx), dbsqlc.GetUserMachineTerminalSessionParams{ID: id, UserMachineID: machineID, UserID: account})
		if e != nil {
			return nil, 0, e
		}
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
