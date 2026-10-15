// Package accountingauthority preserves only the payer binding needed to settle
// delayed usage. These records never authorize access to a resource.
package accountingauthority

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// DeliveryWindow permits an edge offline for a weekend to deliver its durable
// queue, with additional recovery time. The v1 wire contract uses the same seven
// day bound in paperboat-tunnel/internal/usage/policy.go.
const DeliveryWindow = 7 * 24 * time.Hour

// MaximumPerNode matches the edge's 4096-report durable queue capacity. Admission
// fails closed at capacity; old accounting records are never evicted early.
const MaximumPerNode = 4096

var ErrQuotaExhausted = errors.New("workspace bandwidth allowance is exhausted")

var ErrUnavailable = errors.New("usage accounting authority is unavailable; retry after retained usage expires")

type Binding struct {
	ID, Node, Environment, Route, User, Team string
	Revision                                 int64
	IssuedAt, ExpiresAt                      time.Time
}

type QuotaChecker interface {
	CheckWorkspaceBandwidthTx(context.Context, *db.Tx, string, time.Time) (bool, error)
}

func Record(ctx context.Context, store *db.DB, b Binding, policy ...QuotaChecker) error {
	if b.ID == "" || b.Node == "" || b.Environment == "" || b.Route == "" || b.User == "" || len(b.User) > 256 || b.Revision < 1 || b.ExpiresAt.Before(b.IssuedAt) {
		return ErrUnavailable
	}
	return store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		// Serialize the node's capacity check and upsert without blocking unrelated nodes.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,369197))`, b.Node); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM usage_accounting_authorities WHERE edge_node_id=$1 AND retain_until<=$2`, b.Node, b.IssuedAt); err != nil {
			return err
		}
		workspace := "personal:" + b.User
		var err error
		if b.Team != "" {
			workspace = "team:" + b.Team
			_, err = tx.Exec(ctx, `INSERT INTO billing_workspaces(id,team_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, workspace, b.Team)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO billing_workspaces(id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, workspace, b.User)
		}
		if err != nil {
			return err
		}
		// Payer identity is the unique owning account/team, not the suggested
		// ID used when creating a workspace for the first time.
		if err = tx.QueryRow(ctx, `SELECT id FROM billing_workspaces WHERE (team_id=$1 AND $1<>'' AND user_id IS NULL) OR (user_id=$2 AND $1='' AND team_id IS NULL) FOR UPDATE`, b.Team, b.User).Scan(&workspace); err != nil {
			return err
		}
		// Customer-owned transport is observed but never consumes Paperboat's
		// bandwidth allowance. Classification comes from the registry, not the
		// caller; globally operated installations remain billable hosted nodes.
		var customerOwned bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM selfhost_installations WHERE node_id=$1 AND scope_kind IN ('account','team'))`, b.Node).Scan(&customerOwned); err != nil {
			return err
		}
		if !customerOwned {
			var exhausted bool
			if len(policy) > 0 && policy[0] != nil {
				exhausted, err = policy[0].CheckWorkspaceBandwidthTx(ctx, tx, workspace, b.IssuedAt)
				if err != nil {
					return err
				}
				if exhausted {
					return ErrQuotaExhausted
				}
			}
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start<=$2 AND period_end>$2 AND included_bytes IS NOT NULL AND consumed_bytes>=included_bytes)`, workspace, b.IssuedAt).Scan(&exhausted); err != nil {
				return err
			}
			if exhausted {
				return ErrQuotaExhausted
			}
		}
		return retainBindingTx(ctx, tx, b, workspace)
	})
}

func retainBindingTx(ctx context.Context, tx *db.Tx, b Binding, workspace string) error {
	var capacity bool
	if err := tx.QueryRow(ctx, `SELECT count(*)<$6 OR coalesce(bool_or(authority_id=$2 AND environment_id=$3 AND route_id=$4 AND route_revision=$5),false) FROM usage_accounting_authorities WHERE edge_node_id=$1`, b.Node, b.ID, b.Environment, b.Route, b.Revision, MaximumPerNode).Scan(&capacity); err != nil {
		return err
	}
	if !capacity {
		return ErrUnavailable
	}
	result, err := tx.Exec(ctx, `INSERT INTO usage_accounting_authorities(
 authority_id,edge_node_id,environment_id,route_id,route_revision,workspace_id,
 admitted_at,admission_expires_at,retain_until,actor_account_id
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT(authority_id,edge_node_id,environment_id,route_id,route_revision)
DO UPDATE SET
 actor_account_id=EXCLUDED.actor_account_id,
 admission_expires_at=greatest(usage_accounting_authorities.admission_expires_at,EXCLUDED.admission_expires_at),
 retain_until=greatest(usage_accounting_authorities.retain_until,EXCLUDED.retain_until)
WHERE usage_accounting_authorities.workspace_id=EXCLUDED.workspace_id
 AND (usage_accounting_authorities.actor_account_id IS NULL
      OR usage_accounting_authorities.actor_account_id=EXCLUDED.actor_account_id)`, b.ID, b.Node, b.Environment, b.Route, b.Revision, workspace, b.IssuedAt, b.ExpiresAt, b.ExpiresAt.Add(2*DeliveryWindow), b.User)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrUnavailable
	}
	return nil
}
