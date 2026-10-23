package operator

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var (
	ErrForbidden = errors.New("operator read forbidden")
	ErrInvalid   = errors.New("invalid operator read")
	ErrNotFound  = errors.New("operator resource not found")
)

type operatorReadFailure struct{ cause error }

func (*operatorReadFailure) Error() string   { return "operator read failed" }
func (e *operatorReadFailure) Unwrap() error { return e.cause }

type Service struct {
	store          *db.DB
	audit          *audit.Writer
	hostStaleAfter time.Duration
	nodeStaleAfter time.Duration
}

func NewService(store *db.DB) *Service {
	return &Service{store: store, audit: audit.NewWriter(store), hostStaleAfter: config.Default().UserMachines.OfflineAfter, nodeStaleAfter: controlplane.ControlTunnelNodeStaleAfter()}
}

type Account struct {
	ID                 string    `json:"id"`
	PrimaryEmail       string    `json:"primary_email"`
	DisplayName        string    `json:"display_name"`
	Status             string    `json:"status"`
	Version            int64     `json:"version"`
	BillingWorkspaceID string    `json:"billing_workspace_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

type TeamMember struct {
	AccountID  string `json:"account_id"`
	Role       string `json:"role"`
	Active     bool   `json:"active"`
	Generation int64  `json:"generation"`
}

type Team struct {
	ID                 string       `json:"id"`
	OwnerAccount       string       `json:"owner_account"`
	Generation         int64        `json:"generation"`
	Suspended          bool         `json:"suspended"`
	DeletedAt          *time.Time   `json:"deleted_at,omitempty"`
	Members            []TeamMember `json:"members"`
	MembersTruncated   bool         `json:"members_truncated"`
	MembersNextAfter   string       `json:"members_next_after,omitempty"`
	BillingWorkspaceID string       `json:"billing_workspace_id,omitempty"`
	BillingState       string       `json:"billing_state,omitempty"`
}

func (s *Service) SetHostStaleAfter(value time.Duration) error {
	if value <= 0 {
		return ErrInvalid
	}
	s.hostStaleAfter = value
	return nil
}

type Billing struct {
	ID                string     `json:"id"`
	Scope             string     `json:"scope"`
	ScopeID           string     `json:"scope_id"`
	ProductCode       string     `json:"product_code"`
	State             string     `json:"state"`
	Quantity          int32      `json:"quantity"`
	PeriodStart       *time.Time `json:"period_start,omitempty"`
	PeriodEnd         *time.Time `json:"period_end,omitempty"`
	CancelAtPeriodEnd bool       `json:"cancel_at_period_end"`
	Revision          int64      `json:"revision"`
	ObservedAt        time.Time  `json:"observed_at"`
}

type ControlOperation struct {
	ID            string     `json:"id"`
	Type          string     `json:"type"`
	State         string     `json:"state"`
	Attempts      int32      `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	UncertainAt   *time.Time `json:"uncertain_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type BillingOperation struct {
	ID                  string     `json:"id"`
	WorkspaceID         string     `json:"workspace_id"`
	ActorUserID         string     `json:"actor_user_id"`
	Kind                string     `json:"kind"`
	State               string     `json:"state"`
	ProviderAttemptedAt *time.Time `json:"provider_attempted_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type Infrastructure struct {
	Kind           string     `json:"kind"`
	ID             string     `json:"id"`
	State          string     `json:"state"`
	DesiredState   string     `json:"desired_state,omitempty"`
	ObservedStatus string     `json:"observed_status"`
	Ready          bool       `json:"ready"`
	ObservedAt     *time.Time `json:"observed_at,omitempty"`
	Stale          bool       `json:"stale"`
	Revision       int64      `json:"revision"`
}

type InfrastructurePage struct {
	Items     []Infrastructure `json:"items"`
	NextAfter string           `json:"next_after,omitempty"`
}

type AuditEvent struct {
	ID           string    `json:"id"`
	ActorUserID  string    `json:"actor_user_id,omitempty"`
	ActorType    string    `json:"actor_type"`
	EventType    string    `json:"event_type"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	CreatedAt    time.Time `json:"created_at"`
}

type AuditPage struct {
	Events    []AuditEvent `json:"events"`
	NextAfter string       `json:"next_after,omitempty"`
}

type AccountPage struct {
	Accounts  []Account `json:"accounts"`
	NextAfter string    `json:"next_after,omitempty"`
}
type TeamSummary struct {
	ID                 string `json:"id"`
	OwnerAccount       string `json:"owner_account"`
	Generation         int64  `json:"generation"`
	Suspended          bool   `json:"suspended"`
	BillingWorkspaceID string `json:"billing_workspace_id,omitempty"`
	BillingState       string `json:"billing_state,omitempty"`
}
type TeamPage struct {
	Teams     []TeamSummary `json:"teams"`
	NextAfter string        `json:"next_after,omitempty"`
}
type BillingPage struct {
	Workspaces []Billing `json:"workspaces"`
	NextAfter  string    `json:"next_after,omitempty"`
}
type BillingOperationPage struct {
	Operations []BillingOperation `json:"operations"`
	NextAfter  string             `json:"next_after,omitempty"`
}
type ControlOperationPage struct {
	Operations []ControlOperation `json:"operations"`
	NextAfter  string             `json:"next_after,omitempty"`
}
type StateCount struct {
	State string `json:"state"`
	Count int64  `json:"count"`
}
type Overview struct {
	Accounts          []StateCount `json:"accounts"`
	BillingWorkspaces []StateCount `json:"billing_workspaces"`
	ControlOperations []StateCount `json:"control_operations"`
	Hosts             int64        `json:"hosts"`
	HostsReady        int64        `json:"hosts_ready"`
	HostsStale        int64        `json:"hosts_stale"`
	HostsObservedAt   *time.Time   `json:"hosts_observed_at,omitempty"`
	Relays            int64        `json:"relays"`
	RelaysReady       int64        `json:"relays_ready"`
	RelaysStale       int64        `json:"relays_stale"`
	RelaysObservedAt  *time.Time   `json:"relays_observed_at,omitempty"`
}

type ObservationLayer struct {
	States           []StateCount `json:"states"`
	Unobserved       int64        `json:"unobserved"`
	LatestObservedAt *time.Time   `json:"latest_observed_at,omitempty"`
}

type TunnelStatus struct {
	ID                    string           `json:"id"`
	DesiredState          string           `json:"desired_state"`
	Generation            int64            `json:"generation"`
	SummaryCode           string           `json:"summary_code"`
	SummaryTransitionedAt time.Time        `json:"summary_transitioned_at"`
	ConnectorSessions     ObservationLayer `json:"connector_sessions"`
	EdgeAssignments       ObservationLayer `json:"edge_assignments"`
	DNSPublications       ObservationLayer `json:"dns_publications"`
	Certificates          ObservationLayer `json:"certificates"`
}

func (s *Service) Read(ctx context.Context, actor auth.User, kind, target, after string, limit int) (any, error) {
	kind, target, after = strings.TrimSpace(kind), strings.TrimSpace(target), strings.TrimSpace(after)
	if s == nil || s.store == nil || actor.ID == "" {
		return nil, ErrForbidden
	}
	if !validKind(kind) {
		return nil, ErrInvalid
	}
	if limit < 1 || limit > 100 || len(after) > 128 || (after != "" && !safeID(after)) {
		return nil, ErrInvalid
	}
	if !pageKind(kind) && kind != "team" && after != "" {
		return nil, ErrInvalid
	}
	if len(target) > 256 || (!pageKind(kind) && kind != "overview" && target == "") || (!pageKind(kind) && kind != "infrastructure" && kind != "audit" && (len(target) > 128 || !safeID(target))) {
		return nil, ErrInvalid
	}

	var value any
	var readErr error
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var currentRole, status string
		if err := tx.QueryRow(ctx, `SELECT role,status FROM operator_accounts WHERE id=$1`, actor.ID).Scan(&currentRole, &status); err != nil {
			return operatorReadAuthorizationError(err)
		}
		if status != "active" || !allowed(auth.Role(currentRole), kind) {
			return ErrForbidden
		}
		value, readErr = s.readTx(ctx, tx, kind, target, after, limit)
		if readErr != nil && readErr != ErrNotFound {
			return readErr
		}
		outcome := "available"
		if readErr == ErrNotFound {
			outcome = "unavailable"
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{
			ActorUserID: actor.ID, ActorType: audit.ActorAdmin,
			EventType: "operator.read", ResourceType: kind, ResourceID: target,
			Metadata: map[string]any{"outcome": outcome},
		})
	})
	if err != nil {
		return nil, &operatorReadFailure{cause: err}
	}
	if readErr != nil {
		return nil, readErr
	}
	return value, nil
}

func allowed(role auth.Role, kind string) bool {
	switch string(role) {
	case "admin":
		return validKind(kind)
	case "support":
		return kind == "account" || kind == "team" || kind == "billing" || kind == "accounts" || kind == "teams" || kind == "workspaces"
	case "billing":
		return kind == "account" || kind == "team" || kind == "billing" || kind == "billing_operation" || kind == "workspaces" || kind == "billing_operations"
	case "operations":
		return kind == "infrastructure" || kind == "control_operation" || kind == "control_operations" || kind == "tunnel_status"
	default:
		return false
	}
}

func validKind(kind string) bool {
	return kind == "account" || kind == "team" || kind == "billing" || kind == "infrastructure" || kind == "control_operation" || kind == "billing_operation" || kind == "audit" || kind == "accounts" || kind == "teams" || kind == "workspaces" || kind == "billing_operations" || kind == "control_operations" || kind == "overview" || kind == "tunnel_status"
}

func pageKind(kind string) bool {
	return kind == "infrastructure" || kind == "audit" || kind == "accounts" || kind == "teams" || kind == "workspaces" || kind == "billing_operations" || kind == "control_operations"
}

func safeID(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.:/", r)) {
			return false
		}
	}
	return true
}

func operatorReadAuthorizationError(err error) error {
	if directNoRows(err) {
		return ErrForbidden
	}
	return err
}

func directNoRows(err error) bool {
	return err == pgx.ErrNoRows || err == sql.ErrNoRows
}

func closeRowsError(rows pgx.Rows, operationErr error) error {
	rows.Close()
	return errors.Join(operationErr, rows.Err())
}

func (s *Service) readTx(ctx context.Context, tx *db.Tx, kind, target, after string, limit int) (any, error) {
	switch kind {
	case "account":
		return readAccount(ctx, tx, target)
	case "team":
		return readTeam(ctx, tx, target, after, limit)
	case "billing":
		return readBilling(ctx, tx, target)
	case "control_operation":
		return readControlOperation(ctx, tx, target)
	case "billing_operation":
		return readBillingOperation(ctx, tx, target)
	case "infrastructure":
		return s.readInfrastructure(ctx, tx, target, after, limit)
	case "audit":
		return readAudit(ctx, tx, target, after, limit)
	case "accounts":
		return readAccounts(ctx, tx, target, after, limit)
	case "teams":
		return readTeams(ctx, tx, target, after, limit)
	case "workspaces":
		return readWorkspaces(ctx, tx, target, after, limit)
	case "billing_operations":
		return readBillingOperations(ctx, tx, target, after, limit)
	case "control_operations":
		return readControlOperations(ctx, tx, target, after, limit)
	case "overview":
		return s.readOverview(ctx, tx, target)
	case "tunnel_status":
		return readTunnelStatus(ctx, tx, target)
	default:
		return nil, ErrInvalid
	}
}

func readAccount(ctx context.Context, tx *db.Tx, id string) (Account, error) {
	var v Account
	err := tx.QueryRow(ctx, `SELECT u.id,u.primary_email,u.display_name,u.status,u.version,coalesce(w.id,''),u.created_at FROM users u LEFT JOIN billing_workspaces w ON w.user_id=u.id WHERE u.id=$1`, id).Scan(&v.ID, &v.PrimaryEmail, &v.DisplayName, &v.Status, &v.Version, &v.BillingWorkspaceID, &v.CreatedAt)
	return v, scanErr(err)
}

func readTeam(ctx context.Context, tx *db.Tx, id, after string, limit int) (_ Team, resultErr error) {
	var v Team
	var deleted sql.NullTime
	err := tx.QueryRow(ctx, `SELECT t.team_id,t.owner_account,t.generation,t.suspended,t.deleted_at,coalesce(w.id,''),coalesce(w.state,'') FROM teams t LEFT JOIN billing_workspaces w ON w.team_id=t.team_id WHERE t.team_id=$1`, id).Scan(&v.ID, &v.OwnerAccount, &v.Generation, &v.Suspended, &deleted, &v.BillingWorkspaceID, &v.BillingState)
	if err != nil {
		return v, scanErr(err)
	}
	if deleted.Valid {
		v.DeletedAt = &deleted.Time
	}
	rows, err := tx.Query(ctx, `SELECT account_id,role,active,membership_generation FROM team_members WHERE team_id=$1 AND account_id>$2 ORDER BY account_id LIMIT $3`, id, after, limit+1)
	if err != nil {
		return v, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	for rows.Next() {
		var m TeamMember
		if err := rows.Scan(&m.AccountID, &m.Role, &m.Active, &m.Generation); err != nil {
			return v, err
		}
		if len(v.Members) < limit {
			v.Members = append(v.Members, m)
		} else {
			v.MembersTruncated = true
			v.MembersNextAfter = v.Members[len(v.Members)-1].AccountID
		}
	}
	return v, rows.Err()
}

func readBilling(ctx context.Context, tx *db.Tx, id string) (Billing, error) {
	var v Billing
	var userID, teamID, product sql.NullString
	var start, end sql.NullTime
	err := tx.QueryRow(ctx, `SELECT id,user_id,team_id,product_code,state,quantity,period_start,period_end,cancel_at_period_end,revision,updated_at FROM billing_workspaces WHERE id=$1`, id).Scan(&v.ID, &userID, &teamID, &product, &v.State, &v.Quantity, &start, &end, &v.CancelAtPeriodEnd, &v.Revision, &v.ObservedAt)
	if err != nil {
		return v, scanErr(err)
	}
	if userID.Valid {
		v.Scope, v.ScopeID = "account", userID.String
	} else {
		v.Scope, v.ScopeID = "team", teamID.String
	}
	v.ProductCode = product.String
	if start.Valid {
		v.PeriodStart = &start.Time
	}
	if end.Valid {
		v.PeriodEnd = &end.Time
	}
	return v, nil
}

func readControlOperation(ctx context.Context, tx *db.Tx, id string) (ControlOperation, error) {
	var v ControlOperation
	var next, uncertain, completed sql.NullTime
	err := tx.QueryRow(ctx, `SELECT id,operation_type,state,attempts,next_attempt_at,uncertain_at,completed_at,created_at,updated_at FROM control_operations WHERE id=$1`, id).Scan(&v.ID, &v.Type, &v.State, &v.Attempts, &next, &uncertain, &completed, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return v, scanErr(err)
	}
	if next.Valid {
		v.NextAttemptAt = &next.Time
	}
	if uncertain.Valid {
		v.UncertainAt = &uncertain.Time
	}
	if completed.Valid {
		v.CompletedAt = &completed.Time
	}
	return v, nil
}

func readBillingOperation(ctx context.Context, tx *db.Tx, id string) (BillingOperation, error) {
	var v BillingOperation
	var attempted sql.NullTime
	err := tx.QueryRow(ctx, `SELECT idempotency_key,workspace_id,actor_user_id,kind,state,provider_attempted_at,created_at,updated_at FROM billing_workspace_operations WHERE idempotency_key=$1`, id).Scan(&v.ID, &v.WorkspaceID, &v.ActorUserID, &v.Kind, &v.State, &attempted, &v.CreatedAt, &v.UpdatedAt)
	if attempted.Valid {
		v.ProviderAttemptedAt = &attempted.Time
	}
	return v, scanErr(err)
}

func (s *Service) readInfrastructure(ctx context.Context, tx *db.Tx, target, after string, limit int) (_ InfrastructurePage, resultErr error) {
	if target != "host" && target != "relay" && target != "tunnel" {
		return InfrastructurePage{}, ErrInvalid
	}
	var query string
	switch target {
	case "host":
		query = `SELECT id,state,(online AND state='online' AND last_seen_at>now()-($3*interval '1 microsecond')),last_seen_at,(last_seen_at IS NULL OR last_seen_at<=now()-($3*interval '1 microsecond')),version FROM user_machines WHERE id>$1 AND deleted_at IS NULL ORDER BY id LIMIT $2`
	case "relay":
		query = `SELECT id,state,(ready AND state='ready' AND last_heartbeat_at>now()-($3*interval '1 microsecond') AND registry_expires_at>now()),last_heartbeat_at,(last_heartbeat_at IS NULL OR last_heartbeat_at<=now()-($3*interval '1 microsecond') OR registry_expires_at IS NULL OR registry_expires_at<=now()),version FROM control_tunnel_nodes WHERE id>$1 AND roles @> ARRAY['relay']::text[] ORDER BY id LIMIT $2`
	case "tunnel":
		query = `SELECT id,desired_state,false,NULL::timestamptz,true,generation FROM tunnels WHERE id>$1 AND deleted_at IS NULL ORDER BY id LIMIT $2`
	}
	staleMicros := s.hostStaleAfter.Microseconds()
	if target == "relay" {
		staleMicros = s.nodeStaleAfter.Microseconds()
	}
	args := []any{after, limit + 1}
	if target != "tunnel" {
		args = append(args, staleMicros)
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return InfrastructurePage{}, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	page := InfrastructurePage{Items: make([]Infrastructure, 0, limit)}
	for rows.Next() {
		var v Infrastructure
		var observed sql.NullTime
		if err := rows.Scan(&v.ID, &v.State, &v.Ready, &observed, &v.Stale, &v.Revision); err != nil {
			return page, err
		}
		if observed.Valid {
			v.ObservedAt = &observed.Time
		}
		v = canonicalInfrastructure(target, v)
		if len(page.Items) == limit {
			page.NextAfter = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, v)
	}
	return page, rows.Err()
}

func canonicalInfrastructure(kind string, v Infrastructure) Infrastructure {
	v.Kind = kind
	if kind == "tunnel" {
		v.DesiredState, v.State, v.ObservedStatus, v.Ready, v.Stale = v.State, "", "unknown", false, true
	} else if v.Stale {
		v.ObservedStatus, v.Ready = "stale", false
	} else if v.Ready {
		v.ObservedStatus = "ready"
	} else {
		v.ObservedStatus = "not_ready"
	}
	return v
}

func readAccounts(ctx context.Context, tx *db.Tx, email, after string, limit int) (_ AccountPage, resultErr error) {
	if email != "" && (len(email) > 254 || strings.Count(email, "@") != 1 || strings.ContainsAny(email, " \t\r\n")) {
		return AccountPage{}, ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT u.id,u.primary_email,u.display_name,u.status,u.version,coalesce(w.id,''),u.created_at FROM users u LEFT JOIN billing_workspaces w ON w.user_id=u.id WHERE u.id>$1 AND u.role<>'system_worker' AND ($2='' OR lower(u.primary_email)=lower($2)) ORDER BY u.id LIMIT $3`, after, email, limit+1)
	if err != nil {
		return AccountPage{}, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	p := AccountPage{Accounts: make([]Account, 0, limit)}
	for rows.Next() {
		var v Account
		if err := rows.Scan(&v.ID, &v.PrimaryEmail, &v.DisplayName, &v.Status, &v.Version, &v.BillingWorkspaceID, &v.CreatedAt); err != nil {
			return p, err
		}
		if len(p.Accounts) == limit {
			p.NextAfter = p.Accounts[len(p.Accounts)-1].ID
			break
		}
		p.Accounts = append(p.Accounts, v)
	}
	return p, rows.Err()
}

func readTeams(ctx context.Context, tx *db.Tx, target, after string, limit int) (_ TeamPage, resultErr error) {
	if target != "" {
		return TeamPage{}, ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT t.team_id,t.owner_account,t.generation,t.suspended,coalesce(w.id,''),coalesce(w.state,'') FROM teams t LEFT JOIN billing_workspaces w ON w.team_id=t.team_id WHERE t.team_id>$1 AND t.deleted_at IS NULL ORDER BY t.team_id LIMIT $2`, after, limit+1)
	if err != nil {
		return TeamPage{}, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	p := TeamPage{Teams: make([]TeamSummary, 0, limit)}
	for rows.Next() {
		var v TeamSummary
		if err := rows.Scan(&v.ID, &v.OwnerAccount, &v.Generation, &v.Suspended, &v.BillingWorkspaceID, &v.BillingState); err != nil {
			return p, err
		}
		if len(p.Teams) == limit {
			p.NextAfter = p.Teams[len(p.Teams)-1].ID
			break
		}
		p.Teams = append(p.Teams, v)
	}
	return p, rows.Err()
}

var billingStates = map[string]bool{"free": true, "incomplete": true, "incomplete_expired": true, "trialing": true, "active": true, "past_due": true, "canceled": true, "unpaid": true, "paused": true}
var operationStates = map[string]bool{"pending": true, "running": true, "succeeded": true, "failed": true, "uncertain": true, "dead_letter": true}

func readWorkspaces(ctx context.Context, tx *db.Tx, state, after string, limit int) (_ BillingPage, resultErr error) {
	if state != "" && !billingStates[state] {
		return BillingPage{}, ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT id,user_id,team_id,product_code,state,quantity,period_start,period_end,cancel_at_period_end,revision,updated_at FROM billing_workspaces WHERE id>$1 AND ($2='' OR state=$2) ORDER BY id LIMIT $3`, after, state, limit+1)
	if err != nil {
		return BillingPage{}, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	p := BillingPage{Workspaces: make([]Billing, 0, limit)}
	for rows.Next() {
		var v Billing
		var uid, tid, product sql.NullString
		var start, end sql.NullTime
		if err := rows.Scan(&v.ID, &uid, &tid, &product, &v.State, &v.Quantity, &start, &end, &v.CancelAtPeriodEnd, &v.Revision, &v.ObservedAt); err != nil {
			return p, err
		}
		if uid.Valid {
			v.Scope, v.ScopeID = "account", uid.String
		} else {
			v.Scope, v.ScopeID = "team", tid.String
		}
		v.ProductCode = product.String
		if start.Valid {
			v.PeriodStart = &start.Time
		}
		if end.Valid {
			v.PeriodEnd = &end.Time
		}
		if len(p.Workspaces) == limit {
			p.NextAfter = p.Workspaces[len(p.Workspaces)-1].ID
			break
		}
		p.Workspaces = append(p.Workspaces, v)
	}
	return p, rows.Err()
}

func readBillingOperations(ctx context.Context, tx *db.Tx, state, after string, limit int) (_ BillingOperationPage, resultErr error) {
	if state != "" && !map[string]bool{"pending": true, "succeeded": true, "failed": true, "uncertain": true}[state] {
		return BillingOperationPage{}, ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT idempotency_key,workspace_id,actor_user_id,kind,state,provider_attempted_at,created_at,updated_at FROM billing_workspace_operations WHERE idempotency_key>$1 AND ($2='' OR state=$2) ORDER BY idempotency_key LIMIT $3`, after, state, limit+1)
	if err != nil {
		return BillingOperationPage{}, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	p := BillingOperationPage{Operations: make([]BillingOperation, 0, limit)}
	for rows.Next() {
		var v BillingOperation
		var attempted sql.NullTime
		if err := rows.Scan(&v.ID, &v.WorkspaceID, &v.ActorUserID, &v.Kind, &v.State, &attempted, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return p, err
		}
		if attempted.Valid {
			v.ProviderAttemptedAt = &attempted.Time
		}
		if len(p.Operations) == limit {
			p.NextAfter = p.Operations[len(p.Operations)-1].ID
			break
		}
		p.Operations = append(p.Operations, v)
	}
	return p, rows.Err()
}

func readControlOperations(ctx context.Context, tx *db.Tx, state, after string, limit int) (_ ControlOperationPage, resultErr error) {
	if state != "" && !operationStates[state] {
		return ControlOperationPage{}, ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT id,operation_type,state,attempts,next_attempt_at,uncertain_at,completed_at,created_at,updated_at FROM control_operations WHERE id>$1 AND ($2='' OR state=$2) ORDER BY id LIMIT $3`, after, state, limit+1)
	if err != nil {
		return ControlOperationPage{}, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	p := ControlOperationPage{Operations: make([]ControlOperation, 0, limit)}
	for rows.Next() {
		var v ControlOperation
		var next, uncertain, completed sql.NullTime
		if err := rows.Scan(&v.ID, &v.Type, &v.State, &v.Attempts, &next, &uncertain, &completed, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return p, err
		}
		if next.Valid {
			v.NextAttemptAt = &next.Time
		}
		if uncertain.Valid {
			v.UncertainAt = &uncertain.Time
		}
		if completed.Valid {
			v.CompletedAt = &completed.Time
		}
		if len(p.Operations) == limit {
			p.NextAfter = p.Operations[len(p.Operations)-1].ID
			break
		}
		p.Operations = append(p.Operations, v)
	}
	return p, rows.Err()
}

func (s *Service) readOverview(ctx context.Context, tx *db.Tx, target string) (_ Overview, resultErr error) {
	if target != "" {
		return Overview{}, ErrInvalid
	}
	var v Overview
	readCounts := func(query string, dst *[]StateCount) (countErr error) {
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return err
		}
		defer func() { countErr = closeRowsError(rows, countErr) }()
		for rows.Next() {
			var c StateCount
			if err := rows.Scan(&c.State, &c.Count); err != nil {
				return err
			}
			*dst = append(*dst, c)
		}
		return rows.Err()
	}
	if err := readCounts(`SELECT status,count(*) FROM users WHERE role<>'system_worker' GROUP BY status ORDER BY status`, &v.Accounts); err != nil {
		return v, err
	}
	if err := readCounts(`SELECT state,count(*) FROM billing_workspaces GROUP BY state ORDER BY state`, &v.BillingWorkspaces); err != nil {
		return v, err
	}
	if err := readCounts(`SELECT state,count(*) FROM control_operations GROUP BY state ORDER BY state`, &v.ControlOperations); err != nil {
		return v, err
	}
	var host, relay sql.NullTime
	if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE online AND state='online' AND last_seen_at>now()-($1*interval '1 microsecond')),count(*) FILTER(WHERE last_seen_at IS NULL OR last_seen_at<=now()-($1*interval '1 microsecond')),max(last_seen_at) FROM user_machines WHERE deleted_at IS NULL`, s.hostStaleAfter.Microseconds()).Scan(&v.Hosts, &v.HostsReady, &v.HostsStale, &host); err != nil {
		return v, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE ready AND state='ready' AND last_heartbeat_at>now()-($1*interval '1 microsecond') AND registry_expires_at>now()),count(*) FILTER(WHERE last_heartbeat_at IS NULL OR last_heartbeat_at<=now()-($1*interval '1 microsecond') OR registry_expires_at IS NULL OR registry_expires_at<=now()),max(last_heartbeat_at) FROM control_tunnel_nodes WHERE roles @> ARRAY['relay']::text[]`, s.nodeStaleAfter.Microseconds()).Scan(&v.Relays, &v.RelaysReady, &v.RelaysStale, &relay); err != nil {
		return v, err
	}
	if host.Valid {
		v.HostsObservedAt = &host.Time
	}
	if relay.Valid {
		v.RelaysObservedAt = &relay.Time
	}
	return v, nil
}

func readTunnelStatus(ctx context.Context, tx *db.Tx, id string) (TunnelStatus, error) {
	var out TunnelStatus
	if err := tx.QueryRow(ctx, `SELECT id,desired_state,generation,summary_code,summary_transitioned_at FROM tunnels WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&out.ID, &out.DesiredState, &out.Generation, &out.SummaryCode, &out.SummaryTransitionedAt); err != nil {
		return out, scanErr(err)
	}
	type layerQuery struct {
		states, observed, unobserved string
		destination                  *ObservationLayer
	}
	queries := []layerQuery{
		{`SELECT s.state,count(*) FROM tunnel_connector_sessions s JOIN tunnel_connectors c ON c.id=s.connector_id WHERE c.tunnel_id=$1 GROUP BY s.state ORDER BY s.state`, `SELECT max(s.last_heartbeat_at) FROM tunnel_connector_sessions s JOIN tunnel_connectors c ON c.id=s.connector_id WHERE c.tunnel_id=$1`, `SELECT count(*) FROM tunnel_connectors c WHERE c.tunnel_id=$1 AND c.revoked_at IS NULL AND NOT EXISTS(SELECT 1 FROM tunnel_connector_sessions s WHERE s.connector_id=c.id)`, &out.ConnectorSessions},
		{`SELECT observed_state,count(*) FROM tunnel_edge_route_assignments WHERE tunnel_id=$1 GROUP BY observed_state ORDER BY observed_state`, `SELECT max(observed_at) FROM tunnel_edge_route_assignments WHERE tunnel_id=$1`, `SELECT count(*) FROM tunnel_routes r WHERE r.tunnel_id=$1 AND r.deleted_at IS NULL AND r.desired_state='active' AND NOT EXISTS(SELECT 1 FROM tunnel_edge_route_assignments a WHERE a.route_id=r.id AND a.state<>'detached')`, &out.EdgeAssignments},
		{`SELECT p.state,count(*) FROM edge_dns_publications p WHERE p.owner_id=$1 GROUP BY p.state ORDER BY p.state`, `SELECT max(p.observed_at) FROM edge_dns_publications p WHERE p.owner_id=$1`, `SELECT count(*) FROM tunnel_domains d WHERE d.tunnel_id=$1 AND d.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM edge_dns_publications p WHERE p.hostname=d.hostname AND p.owner_id=d.tunnel_id)`, &out.DNSPublications},
		{`SELECT c.state,count(*) FROM tunnel_certificate_records c WHERE c.tunnel_id=$1 GROUP BY c.state ORDER BY c.state`, `SELECT max(d.observed_at) FROM tunnel_certificate_edge_distributions d JOIN tunnel_certificate_records c ON c.id=d.certificate_id WHERE c.tunnel_id=$1`, `SELECT count(*) FROM tunnel_domains d WHERE d.tunnel_id=$1 AND d.deleted_at IS NULL AND d.certificate_strategy<>'none' AND NOT EXISTS(SELECT 1 FROM tunnel_certificate_records c WHERE c.domain_id=d.id)`, &out.Certificates},
	}
	for _, query := range queries {
		rows, err := tx.Query(ctx, query.states, id)
		if err != nil {
			return out, err
		}
		var scanErr error
		for rows.Next() {
			var state StateCount
			if scanErr = rows.Scan(&state.State, &state.Count); scanErr != nil {
				break
			}
			query.destination.States = append(query.destination.States, state)
		}
		if err := closeRowsError(rows, scanErr); err != nil {
			return out, err
		}
		var observed sql.NullTime
		if err := tx.QueryRow(ctx, query.observed, id).Scan(&observed); err != nil {
			return out, err
		}
		if observed.Valid {
			query.destination.LatestObservedAt = &observed.Time
		}
		if err := tx.QueryRow(ctx, query.unobserved, id).Scan(&query.destination.Unobserved); err != nil {
			return out, err
		}
		if query.destination.States == nil {
			query.destination.States = []StateCount{}
		}
	}
	return out, nil
}

func readAudit(ctx context.Context, tx *db.Tx, target, after string, limit int) (_ AuditPage, resultErr error) {
	resourceType, resourceID, ok := strings.Cut(target, ":")
	if !ok || !safeID(resourceType) || !safeID(resourceID) {
		return AuditPage{}, ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT id,coalesce(actor_user_id,''),actor_type,event_type,resource_type,resource_id,created_at FROM audit_events WHERE resource_type=$1 AND resource_id=$2 AND id>$3 ORDER BY id LIMIT $4`, resourceType, resourceID, after, limit+1)
	if err != nil {
		return AuditPage{}, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	page := AuditPage{Events: make([]AuditEvent, 0, limit)}
	for rows.Next() {
		var v AuditEvent
		if err := rows.Scan(&v.ID, &v.ActorUserID, &v.ActorType, &v.EventType, &v.ResourceType, &v.ResourceID, &v.CreatedAt); err != nil {
			return page, err
		}
		if len(page.Events) == limit {
			page.NextAfter = page.Events[len(page.Events)-1].ID
			break
		}
		page.Events = append(page.Events, v)
	}
	return page, rows.Err()
}

func scanErr(err error) error {
	if directNoRows(err) {
		return ErrNotFound
	}
	return err
}
