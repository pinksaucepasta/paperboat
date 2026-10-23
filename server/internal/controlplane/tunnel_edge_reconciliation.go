package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const (
	defaultTunnelEdgeAssignmentBatch int32 = 100
	maxTunnelEdgeAssignmentBatch     int32 = 100
)

// ReconcileTunnelEdgeRouteAssignments publishes connector-ready HTTP and
// TCP routes
// to at most two ready edge processes in distinct failure domains.  Candidate selection and the immutable assignment
// write happen in one serializable transaction.  This makes a concurrent
// connector replacement, config activation, route mutation, or edge process
// replacement either win as a complete assignment or get retried by the next
// bounded pass.
func (s *EdgeService) ReconcileTunnelEdgeRouteAssignments(ctx context.Context, batchSize int32) (int, error) {
	if s == nil || s.store == nil {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > maxTunnelEdgeAssignmentBatch {
		batchSize = defaultTunnelEdgeAssignmentBatch
	}
	now := s.clock().UTC()
	staged := 0
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Queries().RetireUnavailableTunnelEdgeAssignmentsV1(ctx, now); err != nil {
			return err
		}
		if err := finalizeUnavailableTunnelEdgeDrains(ctx, tx, now, batchSize); err != nil {
			return err
		}
		candidates, err := tx.Queries().ListReadyTunnelEdgeRouteCandidatesV1(ctx, dbsqlc.ListReadyTunnelEdgeRouteCandidatesV1Params{
			Now: now, RowLimit: batchSize,
		})
		if err != nil {
			return err
		}
		attemptStaged := 0
		for _, candidate := range candidates {
			// The query currently filters this value through the session join, but
			// retain the validity check at the write boundary so a future query
			// change cannot turn a NULL session into an unbound assignment.
			if !candidate.ConnectorSessionID.Valid || !candidate.MachineIdentityPublicKey.Valid || len(candidate.ConfigContentHash) != sha256.Size {
				continue
			}
			machineThumbprint, err := canonicalMachineIdentityThumbprint(candidate.MachineIdentityPublicKey.String)
			if err != nil {
				continue
			}
			// Lock the route and all of its assignment history before checking
			// the exact binding. Without this post-lock check, two concurrent
			// reconcilers can both miss a staged row and the later one can
			// retire the row inserted by the first.
			nextGeneration, err := tx.Queries().NextTunnelEdgeRouteAssignmentGenerationV1(ctx, candidate.RouteID)
			if err != nil {
				return err
			}
			if nextGeneration < 1 {
				return fmt.Errorf("invalid next tunnel edge assignment generation %d", nextGeneration)
			}
			binding, err := tx.Queries().GetTunnelEdgeRouteAssignmentBindingV1(ctx, dbsqlc.GetTunnelEdgeRouteAssignmentBindingV1Params{
				RouteID: candidate.RouteID, AccountID: candidate.AccountID, TunnelID: candidate.TunnelID,
				ConnectorID: candidate.ConnectorID, HostID: candidate.HostID, ConnectorGeneration: candidate.ConnectorGeneration,
				ConnectorSessionID: candidate.ConnectorSessionID.String, ConnectorProcessGeneration: candidate.ConnectorProcessGeneration,
				ConfigGeneration: candidate.ConfigGeneration, ConfigContentHash: append([]byte(nil), candidate.ConfigContentHash...),
				AccessMode: candidate.AccessMode, ViewerPolicyGeneration: candidate.ViewerPolicyGeneration,
				EdgeNodeID: candidate.EdgeNodeID, EdgeProcessEpoch: candidate.EdgeProcessEpoch,
			})
			if err == nil {
				// A staged or active row with the exact immutable tuple is the
				// idempotent result of an earlier pass.
				_ = binding
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			_, err = tx.Queries().SupersedeStaleStagedTunnelEdgeRouteAssignmentV1(ctx, dbsqlc.SupersedeStaleStagedTunnelEdgeRouteAssignmentV1Params{
				RouteID: candidate.RouteID, ConnectorID: candidate.ConnectorID,
				AccountID: candidate.AccountID, TunnelID: candidate.TunnelID, HostID: candidate.HostID,
				MachineIdentityPublicKey: candidate.MachineIdentityPublicKey.String, MachineIdentityThumbprint: machineThumbprint,
				ConnectorGeneration: candidate.ConnectorGeneration, ConnectorSessionID: candidate.ConnectorSessionID.String,
				ConnectorProcessGeneration: candidate.ConnectorProcessGeneration, ConfigGeneration: candidate.ConfigGeneration,
				ConfigContentHash: append([]byte(nil), candidate.ConfigContentHash...), AccessMode: candidate.AccessMode,
				ViewerPolicyGeneration: candidate.ViewerPolicyGeneration,
				RouteGeneration:        candidate.RouteGeneration, RouteRevision: candidate.RouteRevision,
				EdgeNodeID: candidate.EdgeNodeID, EdgeProcessEpoch: candidate.EdgeProcessEpoch,
				EdgeFailureDomain: candidate.EdgeFailureDomain, Now: now,
			})
			if err != nil {
				return err
			}
			assignmentID := tunnelEdgeRouteAssignmentID(candidate, nextGeneration)
			_, err = tx.Queries().StageTunnelEdgeRouteAssignmentV1(ctx, dbsqlc.StageTunnelEdgeRouteAssignmentV1Params{
				AssignmentID: assignmentID, AssignmentGeneration: nextGeneration, Now: now,
				AccountID: candidate.AccountID, ConnectorID: candidate.ConnectorID,
				ConnectorSessionID: candidate.ConnectorSessionID.String, ConfigGeneration: candidate.ConfigGeneration,
				MachineIdentityThumbprint: machineThumbprint,
				EdgeNodeID:                candidate.EdgeNodeID, EdgeProcessEpoch: candidate.EdgeProcessEpoch,
				RouteID: candidate.RouteID, TunnelID: candidate.TunnelID,
				ConnectorGeneration: candidate.ConnectorGeneration, ConnectorProcessGeneration: candidate.ConnectorProcessGeneration,
				ConfigContentHash: append([]byte(nil), candidate.ConfigContentHash...),
				AccessMode:        candidate.AccessMode, ViewerPolicyGeneration: candidate.ViewerPolicyGeneration,
			})
			if errors.Is(err, sql.ErrNoRows) {
				// The candidate became stale or another assignment is staged
				// during this bounded pass.  Leave it for the next pass.
				continue
			}
			if err != nil {
				return err
			}
			if s.audit != nil {
				if err := s.audit.WriteTx(ctx, tx, audit.Event{
					ActorType: audit.ActorSystem, EventType: "edge.route_assignment_staged",
					ResourceType: "tunnel_edge_route_assignment", ResourceID: assignmentID,
					IdempotencyKey: "edge.route_assignment.staged:" + assignmentID,
					Metadata: map[string]any{
						"route_id": candidate.RouteID, "tunnel_id": candidate.TunnelID,
						"connector_id": candidate.ConnectorID, "connector_generation": candidate.ConnectorGeneration,
						"connector_session_id": candidate.ConnectorSessionID.String,
						"config_generation":    candidate.ConfigGeneration, "edge_node_id": candidate.EdgeNodeID,
						"edge_process_epoch": candidate.EdgeProcessEpoch, "assignment_generation": nextGeneration,
					},
				}); err != nil {
					return err
				}
			}
			attemptStaged++
		}
		staged = attemptStaged
		return nil
	})
	if err != nil {
		return 0, err
	}
	return staged, nil
}

// TunnelEdgeAssignmentWorker runs an immediate bounded reconciliation and
// then repeats it.  A transient database/provider failure is logged and left
// for the next pass so one failed observation cannot take down all server
// workers or erase the edge's last-known-good state.
func (s *EdgeService) TunnelEdgeAssignmentWorker(interval time.Duration, batchSize int32) func(context.Context) error {
	if interval <= 0 {
		interval = time.Minute
	}
	if batchSize <= 0 || batchSize > maxTunnelEdgeAssignmentBatch {
		batchSize = defaultTunnelEdgeAssignmentBatch
	}
	return func(ctx context.Context) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			finishProgress := workers.BeginIteration(ctx, interval)
			cycleErr := func() error {
				// Tunnels whose agents serve them on the endpoint engine leave the connector path
				// first (PLAN 3a step 4), so the reconcile below neither stages nor keeps them.
				if err := endpoints.SyncTunnelAgents(ctx, s.store, 500); err != nil && ctx.Err() == nil {
					workers.IterationFailure(ctx, "edge_route_assignment", "edge_route_reconcile_failed", err)
				}
				if _, err := s.ReconcileTunnelEdgeRouteAssignments(ctx, batchSize); err != nil && ctx.Err() == nil {
					workers.IterationFailure(ctx, "edge_route_assignment", "edge_route_reconcile_failed", err)
				}

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

func tunnelEdgeRouteAssignmentID(candidate dbsqlc.ListReadyTunnelEdgeRouteCandidatesV1Row, generation int64) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("paperboat:tunnel-edge-assignment:v1"))
	writeAssignmentString(hash, candidate.AccountID)
	writeAssignmentString(hash, candidate.TunnelID)
	writeAssignmentString(hash, candidate.RouteID)
	writeAssignmentString(hash, candidate.ConnectorID)
	writeAssignmentString(hash, candidate.HostID)
	writeAssignmentString(hash, candidate.ConnectorSessionID.String)
	writeAssignmentString(hash, candidate.EdgeNodeID)
	writeAssignmentString(hash, candidate.EdgeProcessEpoch)
	writeAssignmentInt64(hash, candidate.ConnectorGeneration)
	writeAssignmentInt64(hash, candidate.ConnectorProcessGeneration)
	writeAssignmentInt64(hash, candidate.ConfigGeneration)
	writeAssignmentString(hash, candidate.AccessMode)
	writeAssignmentInt64(hash, candidate.ViewerPolicyGeneration)
	writeAssignmentInt64(hash, generation)
	_, _ = hash.Write(candidate.ConfigContentHash)
	return "asn_" + hex.EncodeToString(hash.Sum(nil)[:24])
}

func writeAssignmentString(hash interface{ Write([]byte) (int, error) }, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write([]byte(value))
}

func writeAssignmentInt64(hash interface{ Write([]byte) (int, error) }, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = hash.Write(encoded[:])
}

// An obsolete process cannot acknowledge its drain. Retain its history, but
// finish cleanup once its registry authority and every previously issued
// ingress decision have expired, including the existing clock-skew bound.
func finalizeUnavailableTunnelEdgeDrains(ctx context.Context, tx *db.Tx, now time.Time, limit int32) error {
	cutoff := now.Add(-connectorprotocol.IngressAuthorityLifetime - connectorprotocol.MaxClockSkew)
	rows, err := tx.Query(ctx, `WITH expired AS (
 SELECT a.assignment_id FROM tunnel_edge_route_assignments a
 LEFT JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 WHERE a.state='draining' AND a.updated_at<=$2
 AND (n.id IS NULL OR n.process_epoch<>a.edge_process_epoch OR n.registry_expires_at<=$1 OR n.drain_deadline<=$1)
 ORDER BY a.updated_at,a.assignment_id LIMIT $3 FOR UPDATE OF a SKIP LOCKED
 ) UPDATE tunnel_edge_route_assignments a SET state='detached',observed_state='detached',released_at=$1,updated_at=$1
 FROM expired e WHERE a.assignment_id=e.assignment_id RETURNING a.account_id,a.tunnel_id`, now, cutoff, limit)
	if err != nil {
		return err
	}
	affected := map[string][]string{}
	for rows.Next() {
		var account, tunnel string
		if err = rows.Scan(&account, &tunnel); err != nil {
			break
		}
		affected[account] = append(affected[account], tunnel)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for account, tunnels := range affected {
		if err = tunnelv1.RefreshHealth(ctx, tx, account, tunnels, now); err != nil {
			return err
		}
	}
	return nil
}
