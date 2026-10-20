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
		_, slot := SplitOperationSlot(in.OperationID)
		err := tx.QueryRow(ctx, selectPreviewEdgeNodeSQL, s.clock(), in.OwnerMachineID, in.InstallationGeneration, in.AccountID, in.PreviewID, slot).Scan(&nodeID)
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

// The preview rides a fresh carrier the machine holds to the edge (any edge of its set, not only
// the one its runtime route calls primary). Slot 0 prefers the primary edge; slot 1 takes another
// edge in a different failure domain from the live slot-0 attachment and exists only beside one.
// Neither slot takes the edge the other holds. A placement is sticky: the edge already holding the slot's
// attachment, then the edge the preview's DNS record already names, come before the primary one, so a
// primary that flips while the host attaches never moves a published URL to an edge resolvers have not seen.
const selectPreviewEdgeNodeSQL = `
SELECT node.id
FROM control_tunnel_nodes AS node
LEFT JOIN runtime_carrier_attachments AS carrier
  ON carrier.machine_id=$2 AND carrier.account_id=$4
 AND carrier.installation_generation=$3 AND carrier.ready
 AND carrier.edge_node_id=node.id AND carrier.edge_process_epoch=node.process_epoch
 AND paperboat.runtime_carrier_valid(carrier.route_id,carrier.edge_node_id,carrier.edge_process_epoch,true)
WHERE node.state = 'ready'
 AND EXISTS(SELECT 1 FROM preview_leases p JOIN user_machines m ON m.id=p.owner_machine_id WHERE p.id=$5 AND p.account_id=$4 AND p.owner_machine_id=$2 AND m.installation_generation=$3 AND m.deleted_at IS NULL AND m.revoked_at IS NULL)
  AND paperboat.node_preview_eligible(node.id,$4,$5)
  AND (carrier.edge_node_id IS NOT NULL OR paperboat.workspace_edge_mode($4,paperboat.publication_workspace_team('preview',$5))='self-hosted-only')
  AND node.ready
  AND node.carrier_endpoint_host IS NOT NULL
  AND node.carrier_endpoint_tcp_port IS NOT NULL
  AND node.carrier_endpoint_quic_port IS NOT NULL
  AND (node.last_heartbeat_at IS NULL OR node.last_heartbeat_at > $1::timestamptz - interval '2 minutes')
  AND (node.drain_deadline IS NULL OR node.drain_deadline > $1::timestamptz)
  AND NOT EXISTS (
    SELECT 1 FROM preview_lease_carrier_attachments held JOIN control_tunnel_nodes held_node ON held_node.id=held.edge_node_id
    WHERE held.account_id=$4 AND held.preview_id=$5 AND held.edge_slot<>$6::smallint AND held.state NOT IN ('failed','released')
      AND (held.edge_node_id=node.id OR (held_node.failure_domain IS NOT NULL AND held_node.failure_domain=node.failure_domain)))
  AND ($6::smallint=0 OR EXISTS (
    SELECT 1 FROM preview_lease_carrier_attachments primary_row
    WHERE primary_row.account_id=$4 AND primary_row.preview_id=$5 AND primary_row.edge_slot=0 AND primary_row.state NOT IN ('failed','released')))
ORDER BY
  EXISTS (SELECT 1 FROM preview_lease_carrier_attachments mine WHERE mine.account_id=$4 AND mine.preview_id=$5
    AND mine.edge_slot=$6::smallint AND mine.state NOT IN ('failed','released') AND mine.edge_node_id=node.id) DESC,
  EXISTS (SELECT 1 FROM preview_leases p JOIN edge_dns_publications pub ON pub.hostname=substring(p.endpoint from 9)
    WHERE p.id=$5 AND p.account_id=$4 AND $6::smallint=0 AND node.public_ingress_ipv4 IS NOT NULL
    AND pub.desired_addresses ? host(node.public_ingress_ipv4)) DESC,
  (carrier.active IS TRUE) DESC, node.id
LIMIT 1
FOR UPDATE OF node SKIP LOCKED`
