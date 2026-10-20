package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

const terminalRecentOutputWarning = "Attaching includes up to 64 KiB of recent output, which may contain secrets regardless of ENV permissions."
const maximumTerminalCatalog = 512

type SharedTerminalTarget struct {
	Kind              string `json:"kind"`
	ID                string `json:"id"`
	Name              string `json:"name"`
	MachineID         string `json:"machine_id"`
	MachineGeneration int64  `json:"machine_generation"`
	Status            string `json:"status"`
	Reason            string `json:"reason"`
	EnvironmentID     string `json:"-"`
}
type SharedTerminalGrant struct {
	Audience   string `json:"audience"`
	AccountID  string `json:"account_id,omitempty"`
	Role       string `json:"role"`
	Active     bool   `json:"active"`
	Generation uint64 `json:"generation"`
}
type SharedTerminalSharing struct {
	Generation          uint64                `json:"generation"`
	TeamID              string                `json:"team_id"`
	Audience            string                `json:"audience"`
	Grants              []SharedTerminalGrant `json:"grants"`
	RecentOutputWarning string                `json:"recent_output_warning"`
}
type SharedTerminalSession struct {
	CurrentDirectory      string                `json:"current_directory,omitempty"`
	ForegroundProcess     string                `json:"foreground_process,omitempty"`
	ID                    string                `json:"id"`
	Name                  string                `json:"name"`
	Title                 string                `json:"title"`
	Machine               string                `json:"machine"`
	StartedIn             string                `json:"started_in"`
	State                 string                `json:"state"`
	DesiredState          string                `json:"desired_state"`
	RuntimeState          string                `json:"runtime_state"`
	LastActiveAt          *time.Time            `json:"last_active_at"`
	ExpiresAt             *time.Time            `json:"expires_at,omitempty"`
	Target                SharedTerminalTarget  `json:"target"`
	OwnerAccount          string                `json:"owner_account"`
	CanManage             bool                  `json:"can_manage"`
	Role                  string                `json:"role"`
	BrowserOnly           bool                  `json:"browser_only"`
	Sharing               SharedTerminalSharing `json:"sharing"`
	Participants          []SessionParticipant  `json:"participants"`
	ParticipantsAvailable bool                  `json:"participants_available"`
	RuntimeGeneration     uint64                `json:"-"`
}

func (s *Service) TerminalSessionCatalog(ctx context.Context, account string) ([]SharedTerminalSession, error) {
	return s.terminalSessionCatalog(ctx, account, "")
}
func (s *Service) terminalSessionCatalog(ctx context.Context, account, exactSession string) ([]SharedTerminalSession, error) {
	items, _, err := s.terminalSessionInventory(ctx, account, exactSession, InventoryOptions{Limit: maximumTerminalCatalog})
	return items, err
}
func (s *Service) TerminalSessionInventory(ctx context.Context, account string, opts InventoryOptions) ([]SharedTerminalSession, int, error) {
	return s.terminalSessionInventory(ctx, account, "", opts)
}
func (s *Service) terminalSessionInventory(ctx context.Context, account, exactSession string, opts InventoryOptions) ([]SharedTerminalSession, int, error) {
	if opts.Limit < 1 || opts.Limit > maximumTerminalCatalog || opts.Offset < 0 {
		return nil, 0, ErrTerminalSessionInvalidName
	}
	if scope, ok := workspace.FromContext(ctx); ok {
		if err := workspace.Validate(ctx, s.db, account, scope); err != nil {
			return nil, 0, err
		}
	}
	workspaceSet, workspaceTeamID := workspaceTeamArgument(ctx)
	var teamID any
	if workspaceTeamID.Valid {
		teamID = workspaceTeamID.String
	}
	query := `WITH browser_candidates AS (
 SELECT p.terminal_session_id id FROM terminal_browser_access_policies p
 JOIN user_machine_terminal_sessions target ON target.id=p.terminal_session_id
 JOIN users actor ON actor.id=$1 AND actor.status='active'
 WHERE target.owner_account<>$1 AND target.deleted_at IS NULL AND target.desired_state='open'
 AND target.runtime_state NOT IN ('closed','exited','deleted')
 AND (NOT $3::boolean OR ($4::text IS NOT NULL AND p.audience='team' AND p.team_id=$4))
 AND ((actor.email_verified AND p.audience='specific_emails' AND (lower(actor.primary_email)=ANY(p.allowed_emails) OR ('*@'||split_part(lower(actor.primary_email),'@',2))=ANY(p.allowed_emails)))
   OR (p.audience='team' AND team_paid_entitled(p.team_id) AND EXISTS(
    SELECT 1 FROM teams team
    JOIN team_members member ON member.team_id=team.team_id AND member.account_id=$1 AND member.active
    JOIN team_members owner_member ON owner_member.team_id=team.team_id AND owner_member.account_id=target.owner_account AND owner_member.active
    WHERE team.team_id=p.team_id AND team.deleted_at IS NULL AND NOT team.suspended)))

), sessions AS (
 SELECT s.id,s.name,s.launch_cwd,s.desired_state,s.runtime_state,s.last_activity_at,s.closed_at,s.is_default,s.owner_account,s.owner_team_id,'machine'::text kind,m.id target_id,m.alias target_name,m.id machine_id,m.environment_id,m.installation_generation,m.state,m.online,m.configured_capabilities @> ARRAY['terminal_host'] AND m.observed_capabilities @> ARRAY['terminal_host'] terminal_ready FROM user_machine_terminal_sessions s JOIN user_machines m ON m.id=s.user_machine_id WHERE s.deleted_at IS NULL AND m.deleted_at IS NULL)
	 SELECT x.id,x.name,x.launch_cwd,x.desired_state,x.runtime_state,x.last_activity_at,x.closed_at,x.is_default,x.owner_account,x.kind,x.target_id,x.target_name,x.machine_id,x.environment_id,x.installation_generation,x.state,x.online,x.terminal_ready,coalesce(b.team_id,''),coalesce(t.generation,0),coalesce(terminal_session_role($1,x.id),''),x.owner_account=$1,EXISTS(SELECT 1 FROM team_terminal_session_grants ag WHERE ag.team_id=b.team_id AND ag.terminal_session_id=x.id AND ag.audience='all_members' AND ag.active),bc.id IS NOT NULL
	 FROM sessions x LEFT JOIN team_resource_bindings b ON b.resource_kind='terminal_session' AND b.resource_id=x.id AND b.active LEFT JOIN teams t ON t.team_id=b.team_id AND t.deleted_at IS NULL
	 LEFT JOIN browser_candidates bc ON bc.id=x.id
	WHERE ($2='' OR x.id=$2)
	  AND (NOT $3::boolean
	    OR ($4::text IS NULL AND x.owner_account=$1 AND x.owner_team_id IS NULL)
	    OR ($4::text IS NOT NULL AND (
	      (x.owner_account=$1 AND x.owner_team_id=$4)
	      OR (b.team_id=$4 AND (x.owner_account=$1 OR terminal_session_role($1,x.id) IN ('viewer','interactive')))
	      OR bc.id IS NOT NULL)))
	  AND (x.owner_account=$1 OR terminal_session_role($1,x.id) IN ('viewer','interactive') OR bc.id IS NOT NULL)
 AND ($5='' OR position(lower($5) in lower(x.name||' '||x.target_name||' '||x.id))>0)
 AND ($6='' OR CASE WHEN x.desired_state<>'open' OR x.runtime_state IN ('closed','exited','deleted') THEN 'closed' WHEN x.runtime_state='running' THEN 'running' ELSE 'unavailable' END=$6)
 AND ($7='' OR ($7='mine' AND x.owner_account=$1) OR ($7='shared' AND x.owner_account<>$1) OR x.owner_account=$7)
 AND ($8::boolean IS NULL OR (x.desired_state<>'open' OR x.runtime_state IN ('closed','exited','deleted'))=$8)`
	var closed any
	if opts.Closed != nil {
		closed = *opts.Closed
	}
	args := []any{account, exactSession, workspaceSet, teamID, opts.Q, opts.State, opts.Owner, closed}
	// Runtime probes below must not hold a database snapshot open.
	tx, err := s.db.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)
	var total int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM ("+query+") inventory", args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(ctx, query+" ORDER BY lower(x.name),x.id LIMIT $9 OFFSET $10", append(args, opts.Limit, opts.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []SharedTerminalSession{}
	for rows.Next() {
		var v SharedTerminalSession
		var state, role, runtimeState string
		var lastActive sql.NullTime
		var closedAt sql.NullTime
		var isDefault bool
		var online bool
		var allMembers bool
		var observedTerminal bool
		var browserCandidate bool
		if err = rows.Scan(&v.ID, &v.Name, &v.StartedIn, &v.DesiredState, &runtimeState, &lastActive, &closedAt, &isDefault, &v.OwnerAccount, &v.Target.Kind, &v.Target.ID, &v.Target.Name, &v.Target.MachineID, &v.Target.EnvironmentID, &v.Target.MachineGeneration, &state, &online, &observedTerminal, &v.Sharing.TeamID, &v.Sharing.Generation, &role, &v.CanManage, &allMembers, &browserCandidate); err != nil {
			rows.Close()
			return nil, 0, err
		}
		v.Machine = cleanTerminalMetadata(v.Target.Name, 128)
		v.StartedIn = cleanTerminalMetadata(v.StartedIn, 1024)
		v.RuntimeState = runtimeState
		v.State = terminalDisplayState(v.DesiredState, runtimeState, false, 0)
		if lastActive.Valid {
			value := lastActive.Time
			v.LastActiveAt = &value
		}
		if !isDefault {
			v.ExpiresAt = terminalSessionExpiresAt(runtimeState, closedAt)
		}
		v.Role = role
		if v.CanManage {
			v.Role = "owner"
		}
		// Resolve browser candidates after releasing the snapshot connection.
		v.BrowserOnly = v.Role == "" && browserCandidate
		if v.Role == "" && !v.BrowserOnly {
			continue
		}
		v.Target.Status, v.Target.Reason = "offline", "terminal_host_unavailable"
		if online && observedTerminal {
			v.Target.Status, v.Target.Reason = "ready", "ready"
		}
		v.Sharing.RecentOutputWarning = terminalRecentOutputWarning
		v.Sharing.Grants = []SharedTerminalGrant{}
		v.Participants = []SessionParticipant{}
		if v.Sharing.TeamID == "" {
			v.Sharing.Audience = "none"
		} else if allMembers {
			v.Sharing.Audience = "all_members"
		} else {
			v.Sharing.Audience = "selected_members"
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	// Candidate SQL only narrows the audience. Recheck the current stream
	// admission policy before exposing browser-only metadata.
	authorized := out[:0]
	for _, item := range out {
		if item.BrowserOnly {
			decision, err := s.ResolveTerminalBrowserRole(ctx, account, item.ID)
			if err != nil {
				continue
			}
			item.Role = string(decision.Role)
		}
		authorized = append(authorized, item)
	}
	out = authorized
	if exactSession == "" {
		ids := make([]string, len(out))
		for index := range out {
			ids[index] = out[index].ID
		}
		for index, observed := range s.terminalInventoryObservations(ctx, account, ids) {
			if observed != nil {
				out[index] = *observed
			}
		}
	}

	return out, total, nil
}

// Live metadata is optional enrichment of an already-authorized page. The BFF
// allows ten seconds for the whole request; one three-second page budget leaves
// room for authorization/database reads and returns unavailable rows on slow hosts.
const terminalInventoryObservationBudget = 3 * time.Second
const terminalInventoryObservationConcurrency = 4

func (s *Service) terminalInventoryObservations(ctx context.Context, account string, ids []string) []*SharedTerminalSession {
	return boundedTerminalInventoryObservations(ctx, ids, func(ctx context.Context, id string) (SharedTerminalSession, error) {
		return s.TerminalSessionSharing(ctx, account, id)
	})
}
func boundedTerminalInventoryObservations(ctx context.Context, ids []string, observe func(context.Context, string) (SharedTerminalSession, error)) []*SharedTerminalSession {
	out := make([]*SharedTerminalSession, len(ids))
	if len(ids) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, terminalInventoryObservationBudget)
	defer cancel()
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < terminalInventoryObservationConcurrency && worker < len(ids); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				if observed, err := observe(ctx, ids[index]); err == nil {
					out[index] = &observed
				}
			}
		}()
	}
dispatch:
	for index := range ids {
		select {
		case jobs <- index:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
	return out
}

func (s *Service) TerminalSessionSharing(ctx context.Context, account, sessionID string) (SharedTerminalSession, error) {
	items, err := s.terminalSessionCatalog(ctx, account, sessionID)
	if err != nil {
		return SharedTerminalSession{}, err
	}
	var item SharedTerminalSession
	found := false
	for _, v := range items {
		if v.ID == sessionID {
			item = v
			found = true
			break
		}
	}
	if !found {
		return item, ErrTerminalSessionNotFound
	}
	if item.BrowserOnly {
		decision, decisionErr := s.ResolveTerminalBrowserRole(ctx, account, sessionID)
		if decisionErr != nil {
			return SharedTerminalSession{}, ErrTerminalSessionNotFound
		}
		if observed, observeErr := s.SnapshotTerminalForBrowser(ctx, decision); observeErr == nil {
			projectTerminalIdentification(&item, observed.Snapshot)
		} else {
			supportreport.CaptureFromContext(ctx, "paperboat-server", "http_terminal", "service_error", "terminal_host_unavailable", observeErr)
		}
		return item, nil
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT audience,coalesce(account_id,''),role,active,generation FROM team_terminal_session_grants WHERE team_id=$1 AND terminal_session_id=$2 ORDER BY audience,account_id`, item.Sharing.TeamID, sessionID)
	if err != nil {
		return item, err
	}
	defer rows.Close()
	all := false
	for rows.Next() {
		var g SharedTerminalGrant
		if err = rows.Scan(&g.Audience, &g.AccountID, &g.Role, &g.Active, &g.Generation); err != nil {
			return item, errors.Join(err, rows.Close())
		}
		if item.CanManage || g.Audience == "all_members" || g.AccountID == account {
			item.Sharing.Grants = append(item.Sharing.Grants, g)
		}
		if g.Audience == "all_members" && g.Active {
			all = true
		}
	}
	if err = rows.Err(); err != nil {
		return item, errors.Join(err, rows.Close())
	}
	if err = rows.Close(); err != nil {
		return item, err
	}
	if all {
		item.Sharing.Audience = "all_members"
	}
	if item.Target.Status != "ready" || s.controlSigner == nil {
		return item, nil
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, item.Target.EnvironmentID)
	if err != nil {
		return item, nil
	}
	now := s.now().UTC()
	token, err := s.signProcessCredential(ctx, mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: item.OwnerAccount, JTI: newID("token"), IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"}, EnvironmentID: item.Target.EnvironmentID, MachineID: item.Target.MachineID, UserID: item.OwnerAccount, CLIClientSessionID: "control-plane", SessionID: item.ID})
	if err != nil {
		return item, nil
	}
	httpIngressURL, err := s.runtimeIngressURL(ctx, item.Target.EnvironmentID, route.PublicHost)
	if err != nil {
		return item, nil
	}
	snap, err := s.controlRuntime.Terminal(ctx, httpIngressURL, token, "snapshot", item.ID, newID("operation"))
	if err != nil {
		return item, nil
	}
	var closedAt sql.NullTime
	if err = s.db.SQL().QueryRowContext(ctx, `UPDATE user_machine_terminal_sessions SET runtime_state=$2,closed_at=CASE WHEN $2 IN ('closed','exited','deleted') THEN coalesce(closed_at,now()) ELSE NULL END,launch_cwd=coalesce(nullif($3,''),launch_cwd),last_runtime_sync_at=now(),last_runtime_sequence=$4,last_activity_at=CASE WHEN $5::timestamptz IS NULL THEN last_activity_at ELSE greatest(coalesce(last_activity_at,$5),$5) END,updated_at=now() WHERE id=$1 RETURNING closed_at,desired_state`, item.ID, snap.State, cleanTerminalMetadata(snap.CWD, 1024), int64(snap.LatestSequence), snap.LastActiveAt).Scan(&closedAt, &item.DesiredState); err != nil {
		return item, err
	}
	item.ExpiresAt = terminalSessionExpiresAt(snap.State, closedAt)
	projectTerminalIdentification(&item, snap)
	if snap.LastActiveAt != nil {
		value := snap.LastActiveAt.UTC()
		item.LastActiveAt = &value
	}
	item.Participants, err = s.describeParticipants(ctx, account, item.OwnerAccount, item.ID, snap.Participants)
	if err != nil {
		// Names are a courtesy: the live list must not disappear because they could not be resolved.
		supportreport.CaptureFromContext(ctx, "paperboat-server", "http_terminal", "service_error", "terminal_host_unavailable", err)
		item.Participants = make([]SessionParticipant, 0, len(snap.Participants))
		for _, participant := range snap.Participants {
			item.Participants = append(item.Participants, SessionParticipant{Participant: participant, Kind: "member"})
		}
	}
	item.ParticipantsAvailable = true
	item.RuntimeState = snap.State
	item.State = terminalDisplayState(item.DesiredState, snap.State, true, len(item.Participants))
	item.RuntimeGeneration = snap.Generation
	if snap.State == "closed" || snap.State == "exited" || snap.State == "deleted" {
		item.Sharing = SharedTerminalSharing{Audience: "none", Grants: []SharedTerminalGrant{}, RecentOutputWarning: terminalRecentOutputWarning}
		item.Participants = []SessionParticipant{}
	}
	return item, nil
}

func (s *Service) GrantTerminalSessionSharing(ctx context.Context, account, teamID, sessionID string, in teams.TerminalSessionGrantRequest) (SharedTerminalSession, error) {
	if scope, selected := workspace.FromContext(ctx); selected && scope.TeamID != "" && scope.TeamID != teamID {
		return SharedTerminalSession{}, ErrTerminalSessionNotFound
	}
	current, err := s.TerminalSessionSharing(ctx, account, sessionID)
	if err != nil || !current.CanManage || !current.ParticipantsAvailable || current.RuntimeGeneration < 1 {
		if err != nil {
			return SharedTerminalSession{}, err
		}
		return SharedTerminalSession{}, ErrTerminalSessionNotFound
	}
	in.TerminalSessionID = sessionID
	in.RuntimeGeneration = current.RuntimeGeneration
	if _, err := teams.NewService(s.db).GrantTerminalSession(ctx, account, teamID, in); err != nil {
		return SharedTerminalSession{}, err
	}
	return s.TerminalSessionSharing(ctx, account, sessionID)
}
func (s *Service) EndTerminalSessionSharing(ctx context.Context, account, sessionID, operation string, expected uint64) (SharedTerminalSession, error) {
	// Personal owners can manage a Team grant on their Personal terminal, while
	// exact catalog authorization still fences terminals from other workspaces.
	current, err := s.TerminalSessionSharing(ctx, account, sessionID)
	if err != nil {
		return SharedTerminalSession{}, err
	}
	if !current.CanManage {
		return SharedTerminalSession{}, ErrTerminalSessionNotFound
	}

	var team string
	workspaceSet, workspaceTeamID := workspaceTeamArgument(ctx)
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT team_id FROM team_resource_bindings WHERE resource_kind='terminal_session' AND resource_id=$1 AND owner_account=$2 AND (NOT $3::boolean OR $4::text IS NULL OR team_id=$4) ORDER BY active DESC LIMIT 1`, sessionID, account, workspaceSet, workspaceTeamID).Scan(&team); errors.Is(err, sql.ErrNoRows) {
		return SharedTerminalSession{}, ErrTerminalSessionNotFound
	} else if err != nil {
		return SharedTerminalSession{}, err
	}
	if _, err := teams.NewService(s.db).EndTerminalSessionSharing(ctx, account, team, sessionID, operation, expected); err != nil {
		return SharedTerminalSession{}, err
	}
	return s.TerminalSessionSharing(ctx, account, sessionID)
}
func (s *Service) RemoveTerminalParticipant(ctx context.Context, account, sessionID, target, operation string, expected uint64) (SharedTerminalSession, error) {
	// Personal owners can manage a Team grant on their Personal terminal, while
	// exact catalog authorization still fences terminals from other workspaces.
	current, err := s.TerminalSessionSharing(ctx, account, sessionID)
	if err != nil {
		return SharedTerminalSession{}, err
	}
	if !current.CanManage {
		return SharedTerminalSession{}, ErrTerminalSessionNotFound
	}

	// A guest who joined with the join code is a session of this terminal, not an account: removing
	// it deletes the session, which ends its connection at the next ticket check; no team is involved.
	removed, err := s.db.SQL().ExecContext(ctx, `DELETE FROM terminal_guest_sessions WHERE terminal_session_id=$1 AND id=$2`, sessionID, target)
	if err != nil {
		return SharedTerminalSession{}, err
	}
	if n, _ := removed.RowsAffected(); n > 0 {
		return s.TerminalSessionSharing(ctx, account, sessionID)
	}

	var team string
	workspaceSet, workspaceTeamID := workspaceTeamArgument(ctx)
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT team_id FROM team_resource_bindings WHERE resource_kind='terminal_session' AND resource_id=$1 AND active AND owner_account=$2 AND (NOT $3::boolean OR $4::text IS NULL OR team_id=$4)`, sessionID, account, workspaceSet, workspaceTeamID).Scan(&team); err != nil {
		return SharedTerminalSession{}, ErrTerminalSessionNotFound
	}
	if _, err := teams.NewService(s.db).RemoveTerminalParticipant(ctx, account, team, sessionID, target, operation, expected); err != nil {
		return SharedTerminalSession{}, err
	}
	return s.TerminalSessionSharing(ctx, account, sessionID)
}

// Runtime identification is observed live; durable names and launch paths stay distinct.
func projectTerminalIdentification(item *SharedTerminalSession, snapshot helperruntime.Snapshot) {
	item.Title = cleanTerminalMetadata(snapshot.Title, 128)
	item.CurrentDirectory = cleanTerminalMetadata(snapshot.CurrentDirectory, 1024)
	item.ForegroundProcess = cleanTerminalMetadata(snapshot.ForegroundProcess, 128)
}

// A live snapshot never overrides an accepted close request. Readiness is
// distinct from process lifecycle; absent observations cannot promise Running.
func terminalDisplayState(desired, runtime string, observed bool, participants int) string {
	if terminalSessionRuntimeClosed(runtime) {
		return "closed"
	}
	if runtime == "failed" || runtime == "close_failed" {
		return "failed"
	}
	if desired != "open" || runtime == "closing" {
		return "closing"
	}
	if observed && (runtime == "running" || runtime == "attached" || runtime == "detached") {
		if participants > 0 {
			return "attached"
		}
		return "detached"
	}
	return "unavailable"
}
