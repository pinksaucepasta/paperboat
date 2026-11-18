package posthogcapture

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// auditEvents maps the committed audit-log event types that matter for product analytics to the PostHog event and
// labels they become. The log is the one place every account, machine, team and feature creation already passes.
var auditEvents = map[string]Event{
	"auth.login":                       {Name: "signed_in"},
	"machine.setup":                    {Name: "machine_enrolled", Props: map[string]string{"via": "cli"}},
	"user_machine.enrollment_approved": {Name: "machine_enrolled", Props: map[string]string{"via": "console"}},
	"team.create":                      {Name: "team_created"},
	"preview.created":                  {Name: "feature_used", Props: map[string]string{"feature": "preview"}},
	"tunnel.created":                   {Name: "feature_used", Props: map[string]string{"feature": "tunnel"}},
	"environment.vault.created":        {Name: "feature_used", Props: map[string]string{"feature": "env"}},
	"recipe.created":                   {Name: "feature_used", Props: map[string]string{"feature": "recipe"}},
	"billing.plan_changed":             {Name: "plan_changed"}, // Stripe reconcile (workspace_service.go)
	"operator.plan_grant":              {Name: "plan_changed"},
	"operator.plan_revoke":             {Name: "plan_changed"},
}

// planEvents name a billing workspace ("personal:<account>" or "team:<id>"), not an actor: the account that
// owns the plan is the one the event is about.
var planEvents = []string{"billing.plan_changed", "operator.plan_grant", "operator.plan_revoke"}

const (
	tailPage   = 200
	tailLag    = 30 * time.Second // an audit row commits at most this late; the cursor never passes a younger row
	tailReplay = 2000             // rows re-read at start; PostHog merges the repeats by UUID
	tailEvery  = 30 * time.Second
	uuidSpace  = "paperboat-audit-posthog"
)

var namespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte(uuidSpace))

// Tail reads the audit log forward and captures each mapped event once per row (a restart replays the last
// tailReplay rows; the row's own UUID makes PostHog keep one). It returns when ctx ends.
func Tail(ctx context.Context, c *Client, pool *pgxpool.Pool) error {
	if c == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	types := make([]string, 0, len(auditEvents))
	for t := range auditEvents {
		types = append(types, t)
	}
	cursor := int64(-1) // unset until the log's end is known
	tick := time.NewTicker(tailEvery)
	defer tick.Stop()
	for {
		// A failed read is retried at the next tick: analytics never takes the server down.
		if cursor < 0 {
			if err := pool.QueryRow(ctx, `SELECT greatest(coalesce(max(cursor_sequence),0)-$1,0) FROM audit_events`, tailReplay).Scan(&cursor); err != nil {
				cursor = -1
			}
		}
		if cursor >= 0 {
			if next, err := tailOnce(ctx, c, pool, types, cursor); err == nil {
				cursor = next
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// tailOnce captures one page and returns the cursor to continue from.
func tailOnce(ctx context.Context, c *Client, pool *pgxpool.Pool, types []string, cursor int64) (int64, error) {
	var horizon int64
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(cursor_sequence),0) FROM audit_events WHERE created_at <= now() - $1::interval`, fmt.Sprintf("%d seconds", int(tailLag.Seconds()))).Scan(&horizon); err != nil {
		return cursor, err
	}
	if horizon <= cursor {
		return cursor, nil
	}
	rows, err := pool.Query(ctx, `SELECT cursor_sequence, id, event_type, who, created_at, plan, state FROM (
 SELECT cursor_sequence, id, event_type, created_at,
  CASE WHEN event_type = ANY($5) THEN CASE
    WHEN resource_id LIKE 'personal:%' THEN substr(resource_id, 10)
    WHEN resource_id LIKE 'team:%' THEN (SELECT owner_account FROM teams WHERE team_id = substr(resource_id, 6)) END
   ELSE actor_user_id END AS who,
  coalesce(metadata->>'product_code', '') AS plan, coalesce(metadata->>'state', '') AS state
 FROM audit_events
 WHERE cursor_sequence > $1 AND cursor_sequence <= $2 AND event_type = ANY($3) AND result = 'success') t
 WHERE who IS NOT NULL ORDER BY cursor_sequence LIMIT $4`, cursor, horizon, types, tailPage, planEvents)
	if err != nil {
		return cursor, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var seq int64
		var id, kind, actor, plan, state string
		var at time.Time
		if err := rows.Scan(&seq, &id, &kind, &actor, &at, &plan, &state); err != nil {
			return cursor, err
		}
		e := auditEvents[kind]
		e.Distinct, e.At, e.UUID = actor, at, uuid.NewSHA1(namespace, []byte(id)).String()
		if e.Name == "plan_changed" {
			e.Props = map[string]string{"plan": plan, "state": state}
		}
		c.Capture(e)
		cursor, n = seq, n+1
	}
	if err := rows.Err(); err != nil {
		return cursor, err
	}
	if n < tailPage {
		cursor = horizon
	}
	return cursor, nil
}
