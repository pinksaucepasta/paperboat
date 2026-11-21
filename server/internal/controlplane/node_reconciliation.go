package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
)

func ControlTunnelNodeStaleAfter() time.Duration {
	return controlTunnelNodeStaleAfter
}

// ReconcileStaleNodes fences nodes that missed their heartbeat deadline. All
// connector and route mutations happen in the same transaction as the stale
// observation, so a newer heartbeat/reassignment cannot be deleted by this run.
func (s *EdgeService) ReconcileStaleNodes(ctx context.Context, cutoff time.Time, batchSize int) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 100 {
		batchSize = 25
	}
	count := 0
	// SKIP LOCKED partitions stale rows between concurrent workers. Read
	// committed is intentional: serializable predicate locks make independent
	// skip-locked claimers conflict, while row locks and expected versions fence
	// every mutation performed by this transaction.
	err := s.store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		attemptCount := 0
		nodes, err := tx.Queries().ListStaleControlTunnelNodesForUpdate(ctx, dbsqlc.ListStaleControlTunnelNodesForUpdateParams{Cutoff: sql.NullTime{Time: cutoff, Valid: true}, BatchSize: int32(batchSize)})
		if err != nil {
			return err
		}
		now := s.clock().UTC()
		for _, node := range nodes {
			if rows, err := tx.Queries().MarkControlTunnelNodeOffline(ctx, dbsqlc.MarkControlTunnelNodeOfflineParams{ID: node.ID, ExpectedVersion: node.Version, Now: now}); err != nil {
				return err
			} else if rows != 1 {
				continue
			}
			nodeID := sql.NullString{String: node.ID, Valid: true}
			if _, err := tx.Queries().FenceControlConnectorsForNode(ctx, dbsqlc.FenceControlConnectorsForNodeParams{EdgeNodeID: nodeID, Now: now}); err != nil {
				return err
			}
			if _, err := tx.Queries().AdvanceControlRoutesForNodeLoss(ctx, dbsqlc.AdvanceControlRoutesForNodeLossParams{EdgeNodeID: nodeID, Now: now}); err != nil {
				return err
			}
			if err := s.audit.WriteTx(ctx, tx, audit.Event{ActorType: audit.ActorSystem, EventType: "edge.node_stale", ResourceType: "edge_node", ResourceID: node.ID, IdempotencyKey: fmt.Sprintf("edge.node_stale:%s:%d", node.ID, node.Version), Metadata: map[string]any{"cutoff": cutoff}}); err != nil {
				return err
			}
			attemptCount++
		}
		// InTx may replay this closure after a serialization failure. Publish
		// only the current attempt's count so aborted work is never reported.
		count = attemptCount
		return nil
	})
	if err == nil {
		serverdiagnostics.ControlNodeStale(int64(count))
	}
	return count, err
}

// ReconcileExpiredDrains finishes drains past their deadline the way a lost node ends: offline (a returning node
// registers again, a retired one cannot), its connectors fenced and its routes advanced, in one transaction with
// the audit event. Without it a draining row stayed forever and every eligibility scan paid for it.
func (s *EdgeService) ReconcileExpiredDrains(ctx context.Context, now time.Time, batchSize int) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 500 {
		batchSize = 500
	}
	count := 0
	err := s.store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		type drained struct {
			id       string
			version  int64
			deadline time.Time
		}
		rows, err := tx.Query(ctx, `SELECT id, version, drain_deadline FROM control_tunnel_nodes
WHERE state='draining' AND drain_deadline <= $1 ORDER BY drain_deadline, id FOR UPDATE SKIP LOCKED LIMIT $2`, now, batchSize)
		if err != nil {
			return err
		}
		var nodes []drained
		for rows.Next() {
			var n drained
			if err := rows.Scan(&n.id, &n.version, &n.deadline); err != nil {
				rows.Close()
				return err
			}
			nodes = append(nodes, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		attemptCount := 0
		for _, node := range nodes {
			tag, err := tx.Exec(ctx, `UPDATE control_tunnel_nodes SET state='offline', ready=false, version=version+1, updated_at=$2
WHERE id=$1 AND version=$3 AND state='draining'`, node.id, now, node.version)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				continue
			}
			nodeID := sql.NullString{String: node.id, Valid: true}
			if _, err := tx.Queries().FenceControlConnectorsForNode(ctx, dbsqlc.FenceControlConnectorsForNodeParams{EdgeNodeID: nodeID, Now: now}); err != nil {
				return err
			}
			if _, err := tx.Queries().AdvanceControlRoutesForNodeLoss(ctx, dbsqlc.AdvanceControlRoutesForNodeLossParams{EdgeNodeID: nodeID, Now: now}); err != nil {
				return err
			}
			if err := s.audit.WriteTx(ctx, tx, audit.Event{ActorType: audit.ActorSystem, EventType: "edge.node_drain_expired", ResourceType: "edge_node", ResourceID: node.id, IdempotencyKey: fmt.Sprintf("edge.node_drain_expired:%s:%d", node.id, node.version), Metadata: map[string]any{"drain_deadline": node.deadline}}); err != nil {
				return err
			}
			attemptCount++
		}
		count = attemptCount
		return nil
	})
	return count, err
}

func (s *EdgeService) StaleNodeWorker(interval, staleAfter time.Duration) func(context.Context) error {
	if interval <= 0 {
		interval = time.Second
	}
	if staleAfter <= 0 {
		staleAfter = 2 * interval
	}
	return func(ctx context.Context) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			finishProgress := workers.BeginIteration(ctx, interval)
			cycleErr := func() error {
				_, progressErr := s.ReconcileStaleNodes(ctx, s.clock().UTC().Add(-staleAfter), 25)
				workers.IterationFailure(ctx, "stale_edge_node", "stale_node_reconcile_failed", progressErr)
				_, drainErr := s.ReconcileExpiredDrains(ctx, s.clock().UTC(), 500)
				workers.IterationFailure(ctx, "stale_edge_node", "expired_drain_reconcile_failed", drainErr)
				return nil
			}()
			finishProgress(cycleErr)
			if cycleErr != nil {
				return cycleErr
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
}
