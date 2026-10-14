package previewattachment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// DBPreviewEdgeNodeSelector chooses only a currently ready control-tunnel
// node. It returns the node identity, never an endpoint chosen by a caller.
// The carrier issuer resolves transport addresses from this identity through
// the canonical edge registry.
type DBPreviewEdgeNodeSelector struct {
	db  *db.DB
	now func() time.Time
}

func NewDBPreviewEdgeNodeSelector(database *db.DB) (*DBPreviewEdgeNodeSelector, error) {
	if database == nil || database.Pool() == nil {
		return nil, fmt.Errorf("%w: database is not open", ErrInvalid)
	}
	return &DBPreviewEdgeNodeSelector{db: database, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *DBPreviewEdgeNodeSelector) SetClock(now func() time.Time) error {
	if s == nil || now == nil {
		return fmt.Errorf("%w: nil edge-node selector clock", ErrInvalid)
	}
	s.now = now
	return nil
}

func (s *DBPreviewEdgeNodeSelector) SelectPreviewEdgeNode(ctx context.Context, in PreviewEdgeNodeSelectionRequest) (string, error) {
	if s == nil || s.db == nil || s.db.Pool() == nil || ctx == nil {
		return "", fmt.Errorf("%w: edge-node selector is not available", ErrInvalid)
	}
	if !validID(in.AccountID) || !validID(in.PreviewID) || !validID(in.OperationID) || !validID(in.OwnerMachineID) || in.InstallationGeneration == 0 {
		return "", fmt.Errorf("%w: incomplete edge-node selection", ErrInvalid)
	}
	var nodeID string
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		err := tx.QueryRow(ctx, selectPreviewEdgeNodeSQL, s.clock(), in.OwnerMachineID, in.InstallationGeneration, in.AccountID, in.PreviewID).Scan(&nodeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAdmissionUnavailable
		}
		return err
	})
	if err != nil {
		return "", err
	}
	if !validID(nodeID) {
		return "", fmt.Errorf("%w: selected edge-node identity is invalid", ErrConflict)
	}
	return nodeID, nil
}

func (s *DBPreviewEdgeNodeSelector) clock() time.Time {
	if s == nil || s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

const selectPreviewEdgeNodeSQL = `
SELECT node.id
FROM control_tunnel_nodes AS node
LEFT JOIN runtime_carrier_attachments AS active
  ON active.machine_id=$2 AND active.account_id=$4
 AND active.installation_generation=$3 AND active.active AND active.ready
 AND active.edge_node_id=node.id AND active.edge_process_epoch=node.process_epoch
 AND paperboat.runtime_carrier_valid(active.route_id,active.edge_node_id,active.edge_process_epoch,true)
WHERE node.state = 'ready'
 AND EXISTS(SELECT 1 FROM preview_leases p JOIN user_machines m ON m.id=p.owner_machine_id WHERE p.id=$5 AND p.account_id=$4 AND p.owner_machine_id=$2 AND m.installation_generation=$3 AND m.deleted_at IS NULL AND m.revoked_at IS NULL)
  AND paperboat.node_preview_eligible(node.id,$4,$5)
  AND (active.edge_node_id IS NOT NULL OR paperboat.workspace_edge_mode($4,paperboat.publication_workspace_team('preview',$5))='self-hosted-only')
  AND node.ready
  AND node.carrier_endpoint_host IS NOT NULL
  AND node.carrier_endpoint_tcp_port IS NOT NULL
  AND node.carrier_endpoint_quic_port IS NOT NULL
  AND (node.last_heartbeat_at IS NULL OR node.last_heartbeat_at > $1::timestamptz - interval '2 minutes')
  AND (node.drain_deadline IS NULL OR node.drain_deadline > $1::timestamptz)
ORDER BY node.id
LIMIT 1
FOR UPDATE OF node SKIP LOCKED`
