// Package planlimits enforces the per-plan count limits (user decision 2026-10-23): machines,
// previews and tunnels together, and console browser terminals. Free and Pro limit a personal
// workspace; Teams are advertised as unlimited and only meet a hidden per-team safety ceiling.
// Only live things count, existing ones keep working when a workspace is over, and only a new
// one is refused, inside the transaction that creates it.
package planlimits

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Kind names one counted resource.
type Kind string

const (
	Machines         Kind = "machines"
	Publications     Kind = "publications"
	BrowserTerminals Kind = "browser_terminals"
)

// Plan is the plan a workspace's limits come from.
type Plan string

const (
	Free Plan = "free"
	Pro  Plan = "pro"
	Team Plan = "team"
)

// ErrLimitReached matches every LimitError.
var ErrLimitReached = errors.New("plan limit reached")

// LimitError refuses a new resource past its plan's limit.
type LimitError struct {
	Kind  Kind
	Plan  Plan
	Limit int
	Used  int
	// BillingURL is the console page to upgrade from; empty without a console origin.
	BillingURL string
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s plan allows %d %s", e.Plan, e.Limit, e.Kind)
}
func (e *LimitError) Unwrap() error { return ErrLimitReached }

// Code is the typed API error code for the refused kind.
func (e *LimitError) Code() string {
	switch e.Kind {
	case Machines:
		return "machine_limit_reached"
	case Publications:
		return "preview_limit_reached"
	default:
		return "browser_terminal_limit_reached"
	}
}

type settings struct {
	limits     config.PlanLimits
	billingURL string
}

var current atomic.Pointer[settings]

func init() { current.Store(&settings{limits: config.Default().Billing.Limits}) }

// Configure sets the deployment's limits and the console Billing page named in refusals.
func Configure(limits config.PlanLimits, billingURL string) {
	current.Store(&settings{limits: limits, billingURL: billingURL})
}

// ViewersPerSharedTerminal is the viewer cap of one shared terminal session.
func ViewersPerSharedTerminal() int { return current.Load().limits.ViewersPerSharedTerminal }

// Scope is the workspace a resource belongs to: a team, or the account's personal workspace.
type Scope struct{ Account, Team string }

func (s Scope) workspaceID() string {
	if s.Team != "" {
		return "team:" + s.Team
	}
	return "personal:" + s.Account
}

// Usage is one kind's count against its limit; Limit is nil for Teams (advertised unlimited).
type Usage struct {
	Used  int  `json:"used"`
	Limit *int `json:"limit"`
}

// CheckTx refuses one more resource of kind when the scope's plan limit is already met. It
// takes a transaction-scoped advisory lock per workspace and kind, so concurrent creations in one
// workspace cannot each claim the last place, whatever locks the caller holds.
func CheckTx(ctx context.Context, tx *db.Tx, scope Scope, kind Kind) error {
	set := current.Load()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,7311))`, scope.workspaceID()+"/"+string(kind)); err != nil {
		return err
	}
	plan, err := planTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	limit := limitFor(set.limits, kind, plan)
	used, err := countTx(ctx, tx, scope, kind)
	if err != nil {
		return err
	}
	if used >= limit {
		return &LimitError{Kind: kind, Plan: plan, Limit: limit, Used: used, BillingURL: set.billingURL}
	}
	return nil
}

// CheckBrowserTerminalTx admits one console browser connection to a terminal session: one
// already open in a browser (a reconnect, or another viewer of a shared terminal) is always
// admitted; a newly opened one counts against the plan's open browser terminals.
func CheckBrowserTerminalTx(ctx context.Context, tx *db.Tx, scope Scope, terminalSessionID string) error {
	var open bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM native_usage_authorities WHERE workspace_id=$1 AND resource_kind='terminal'
 AND resource_id=$2 AND edge_node_id IS NOT NULL AND expires_at>now())`, scope.workspaceID(), terminalSessionID).Scan(&open); err != nil {
		return err
	}
	if open {
		return nil
	}
	return CheckTx(ctx, tx, scope, BrowserTerminals)
}

// UsageTx reports every kind for the scope, for the billing overview.
func UsageTx(ctx context.Context, tx *db.Tx, scope Scope) (map[Kind]Usage, error) {
	set := current.Load()
	kinds := []Kind{Machines, Publications, BrowserTerminals}
	// One statement for the billing overview: the plan and every count.
	query := `SELECT CASE WHEN $2::text<>'' THEN '' ELSE coalesce((SELECT w.product_code FROM billing_workspaces w
 WHERE w.user_id=$1 AND w.team_id IS NULL AND billing_workspace_paid_until(w)>now()),'') END`
	for _, kind := range kinds {
		sub, err := countSQL(kind)
		if err != nil {
			return nil, err
		}
		query += ",\n" + sub
	}
	var product string
	used := make([]int, len(kinds))
	if err := tx.QueryRow(ctx, query, scope.Account, scope.Team, scope.workspaceID()).Scan(&product, &used[0], &used[1], &used[2]); err != nil {
		return nil, err
	}
	plan := planOf(scope, product)
	out := make(map[Kind]Usage, len(kinds))
	for i, kind := range kinds {
		u := Usage{Used: used[i]}
		if plan != Team {
			limit := limitFor(set.limits, kind, plan)
			u.Limit = &limit
		}
		out[kind] = u
	}
	return out, nil
}

func limitFor(l config.PlanLimits, kind Kind, plan Plan) int {
	p := l.Machines
	switch kind {
	case Publications:
		p = l.Publications
	case BrowserTerminals:
		p = l.BrowserTerminals
	}
	switch plan {
	case Pro:
		return p.Pro
	case Team:
		return p.TeamCeiling
	}
	return p.Free
}

// planTx is Team for a team scope, Pro while the personal workspace has a paid Pro period
// (including a renewal grace), Free otherwise.
func planTx(ctx context.Context, tx *db.Tx, scope Scope) (Plan, error) {
	if scope.Team != "" {
		return Team, nil
	}
	var product string
	err := tx.QueryRow(ctx, `SELECT coalesce((SELECT w.product_code FROM billing_workspaces w
 WHERE w.user_id=$1 AND w.team_id IS NULL AND billing_workspace_paid_until(w)>now()),'')`, scope.Account).Scan(&product)
	if err != nil {
		return "", err
	}
	return planOf(scope, product), nil
}

func planOf(scope Scope, product string) Plan {
	switch {
	case scope.Team != "":
		return Team
	case strings.HasPrefix(product, "pro-"):
		return Pro
	default:
		return Free
	}
}

// countSQL is one kind's live count as a scalar subquery over $1 account, $2 team, $3 workspace id.
func countSQL(kind Kind) (string, error) {
	switch kind {
	case Machines:
		return `(SELECT count(*) FROM user_machines WHERE deleted_at IS NULL AND revoked_at IS NULL AND
 CASE WHEN $2<>'' THEN owner_team_id=$2 ELSE user_id=$1 AND owner_team_id IS NULL AND seat_state='occupied' END)`, nil
	case Publications:
		return `((SELECT count(*) FROM preview_leases WHERE terminal_state='active' AND stopped_at IS NULL AND lease_deadline>now() AND
  CASE WHEN $2<>'' THEN owner_team_id=$2 ELSE account_id=$1 AND owner_team_id IS NULL END) +
 (SELECT count(*) FROM tunnels WHERE desired_state='active' AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at>now()) AND
  CASE WHEN $2<>'' THEN owner_team_id=$2 ELSE account_id=$1 AND owner_team_id IS NULL END))`, nil
	case BrowserTerminals:
		// Open console terminals: distinct terminal sessions with a live edge admission, so a
		// reconnect or a shared terminal's viewers never count twice.
		return `(SELECT count(DISTINCT resource_id) FROM native_usage_authorities WHERE workspace_id=$3
 AND resource_kind='terminal' AND edge_node_id IS NOT NULL AND expires_at>now())`, nil
	default:
		return "", fmt.Errorf("unknown plan limit kind %q", kind)
	}
}

func countTx(ctx context.Context, tx *db.Tx, scope Scope, kind Kind) (int, error) {
	query, err := countSQL(kind)
	if err != nil {
		return 0, err
	}
	var n int
	err = tx.QueryRow(ctx, `SELECT `+query+` FROM (SELECT $1::text,$2::text,$3::text) AS params`, scope.Account, scope.Team, scope.workspaceID()).Scan(&n)
	return n, err
}
