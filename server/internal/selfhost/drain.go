package selfhost

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/nodedrain"
)

// DrainScoped takes the installation's own node out of service for new connections (move=false: existing ones
// finish within nodedrain.DrainGrace) or also moves its machines off now (move=true: nodedrain.MoveGrace). It has the
// same authorization as RemoveScoped, and refuses a managed node (the operator drains those) and a node another
// scope also holds an installation on.
func (s *Service) DrainScoped(ctx context.Context, actor, id string, operator, move bool) (Installation, error) {
	node, err := s.changeNodeState(ctx, actor, id, operator, func(ctx context.Context, tx *db.Tx, node string) error {
		return nodedrain.Begin(ctx, tx, node, move)
	})
	if err != nil {
		return Installation{}, err
	}
	if move {
		// A failed wake is not an error: the drain deadline still ends the carriers, only later.
		if err = nodedrain.Wake(ctx, s.database.Pool(), node); err != nil {
			slog.WarnContext(ctx, "selfhost_drain_move_wake_failed", "installation_id", id, "error", err)
		}
	}
	return s.GetInstallation(ctx, actor, id, operator)
}

// ResumeScoped returns a drained node to service; it becomes ready again on its next heartbeat.
func (s *Service) ResumeScoped(ctx context.Context, actor, id string, operator bool) (Installation, error) {
	if _, err := s.changeNodeState(ctx, actor, id, operator, nodedrain.Resume); err != nil {
		return Installation{}, err
	}
	return s.GetInstallation(ctx, actor, id, operator)
}

func (s *Service) changeNodeState(ctx context.Context, actor, id string, operator bool, change func(context.Context, *db.Tx, string) error) (string, error) {
	var scope Scope
	err := s.database.SQL().QueryRowContext(ctx, `SELECT scope_kind,scope_id FROM paperboat.selfhost_installations WHERE id=$1 AND state='active'`, id).Scan(&scope.Kind, &scope.ID)
	if err != nil {
		return "", notFoundIsUnauthorized(err)
	}
	// The operator path reads (and so answers for) global installations only: through it, an account's own would drain
	// and then not be found.
	if operator && scope.Kind != "global" {
		return "", ErrUnauthorized
	}
	if _, err = s.authorizeScope(ctx, actor, scope, true, operator); err != nil {
		return "", err
	}
	var node string
	err = s.database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		err := tx.QueryRow(ctx, `SELECT i.node_id FROM paperboat.selfhost_installations i WHERE i.id=$1 AND i.state='active' AND i.scope_kind=$2 AND i.scope_id=$3
 AND NOT EXISTS(SELECT 1 FROM paperboat.managed_node_authority m WHERE m.node_id=i.node_id)
 AND NOT EXISTS(SELECT 1 FROM paperboat.selfhost_installations o WHERE o.node_id=i.node_id AND o.state='active' AND (o.scope_kind,o.scope_id)<>(i.scope_kind,i.scope_id))
 FOR SHARE OF i`, id, scope.Kind, scope.ID).Scan(&node)
		if err != nil {
			return notFoundIsUnauthorized(err)
		}
		return change(ctx, tx, node)
	})
	if errors.Is(err, nodedrain.ErrNotFound) || errors.Is(err, nodedrain.ErrRetired) {
		return "", ErrInstallationNotFound
	}
	return node, err
}

func notFoundIsUnauthorized(err error) error {
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthorized
	}
	return err
}
