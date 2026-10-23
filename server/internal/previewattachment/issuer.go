package previewattachment

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// DBPreviewCarrierIssuer is the server-owned v1 issuer. It allocates only
// deterministic, operation-scoped identity metadata. No tunnel, connector,
// credential, or private key row is created. The selected edge node is read
// again under a lock so an endpoint cannot be substituted for the node that
// the allocator selected. Carrier endpoints are deliberately separate from
// the legacy connector endpoint columns.
//
// The host and edge still own the actual data-carrier process. They consume
// the returned identity through the canonical preview carrier transport and
// authenticate with the existing renewable machine identity.
type DBPreviewCarrierIssuer struct {
	db  *db.DB
	now func() time.Time
}

func NewDBPreviewCarrierIssuer(database *db.DB) (*DBPreviewCarrierIssuer, error) {
	if database == nil || database.Pool() == nil {
		return nil, fmt.Errorf("%w: database is not open", ErrInvalid)
	}
	return &DBPreviewCarrierIssuer{db: database, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (i *DBPreviewCarrierIssuer) SetClock(now func() time.Time) error {
	if i == nil || now == nil {
		return fmt.Errorf("%w: nil carrier issuer clock", ErrInvalid)
	}
	i.now = now
	return nil
}

func (i *DBPreviewCarrierIssuer) clock() time.Time {
	if i == nil || i.now == nil {
		return time.Now().UTC()
	}
	return i.now().UTC()
}

func (i *DBPreviewCarrierIssuer) IssuePreviewCarrier(ctx context.Context, in PreviewCarrierAllocationRequest) (PreviewCarrierAllocation, error) {
	if i == nil || i.db == nil || i.db.Pool() == nil || ctx == nil {
		return PreviewCarrierAllocation{}, fmt.Errorf("%w: carrier issuer is not available", ErrInvalid)
	}
	if err := in.Proof.Validate(); err != nil {
		return PreviewCarrierAllocation{}, err
	}
	if err := in.Request.Validate(); err != nil {
		return PreviewCarrierAllocation{}, err
	}
	if err := in.Lease.Target.Validate(); err != nil {
		return PreviewCarrierAllocation{}, err
	}
	if !validID(in.EdgeNodeID) || in.Lease.OperationID != in.Request.OperationID || in.Lease.OwnerMachineID != in.Proof.MachineID || in.Lease.MachineAccountID != in.Proof.UserID {
		return PreviewCarrierAllocation{}, ErrUnauthorized
	}
	if len(in.RequestHash) != 64 {
		return PreviewCarrierAllocation{}, fmt.Errorf("%w: request hash is required", ErrInvalid)
	}
	if _, err := hex.DecodeString(in.RequestHash); err != nil {
		return PreviewCarrierAllocation{}, fmt.Errorf("%w: request hash is invalid", ErrInvalid)
	}

	now := i.clock()
	var node previewCarrierEdgeNode
	err := i.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		row := tx.QueryRow(ctx, getPreviewCarrierEdgeNodeSQL, in.EdgeNodeID, now, in.Lease.AccountID, in.Lease.OwnerMachineID, in.Proof.InstallationGeneration, in.Lease.MachineAccountID, in.Lease.PreviewID)
		if err := row.Scan(&node.id, &node.host, &node.tcpPort, &node.quicPort, &node.processEpoch, &node.carrierServerSPKISHA256, &node.carrierServerCertificateChainPEM, &node.state, &node.ready, &node.lastHeartbeat, &node.drainDeadline, &node.workerGeneration, &node.previewSessionID); err != nil {
			if err == pgx.ErrNoRows {
				return ErrAdmissionUnavailable
			}
			return err
		}
		if node.previewSessionID == "" {
			seed := strings.Join([]string{"paperboat:selfhost-preview-session:v1", in.Lease.AccountID, in.Lease.OwnerMachineID, fmt.Sprint(in.Proof.InstallationGeneration), fmt.Sprint(node.workerGeneration), node.id}, "\x00")
			digest := sha256.Sum256([]byte(seed))
			node.previewSessionID = "session_" + hex.EncodeToString(digest[:24])
		}
		if !node.valid(now) {
			return ErrAdmissionUnavailable
		}
		return nil
	})
	if err != nil {
		return PreviewCarrierAllocation{}, err
	}

	// The route configuration is independent of the edge process fence. An
	// edge replacement changes only EdgeProcessEpoch; keeping this hash stable
	// lets a same-worker attachment rebind without falsely looking like a
	// configuration generation change.
	configHash := previewCarrierConfigHash(in, node.id, node.previewSessionID)
	endpoints := []string{
		"h2://" + net.JoinHostPort(node.host, fmt.Sprint(node.tcpPort)),
		"h3://" + net.JoinHostPort(node.host, fmt.Sprint(node.quicPort)),
	}
	carrier := CarrierSnapshot{
		AccountID: in.Lease.AccountID, HostID: in.Lease.OwnerMachineID, Ephemeral: true,
		TunnelID: in.Lease.OwnerMachineID, ConnectorID: in.Lease.OwnerMachineID, SessionID: node.previewSessionID,
		ProcessGeneration: uint64(node.workerGeneration), ConfigGeneration: 1, ConfigContentHash: configHash,
		LeaseDeadline: in.Lease.LeaseDeadline, EdgeNodeID: node.id,
		EdgeProcessEpoch:                     node.processEpoch,
		EdgeCarrierServerSPKISHA256:          node.carrierServerSPKISHA256,
		EdgeCarrierServerCertificateChainPEM: node.carrierServerCertificateChainPEM,
		MachineIdentityPublicKey:             in.Lease.MachineIdentityPublicKey,
		MachineIdentityThumbprint:            in.Lease.MachineIdentityThumbprint,
		EdgeEndpoints:                        endpoints,
	}
	route := RouteSnapshot{
		AccountID: in.Lease.AccountID, TunnelID: in.Lease.OwnerMachineID, RouteID: in.Lease.PreviewID,
		Generation: 1, Protocol: "https", PublicEndpoint: in.Lease.Endpoint,
	}
	return PreviewCarrierAllocation{Carrier: carrier, Route: route}, nil
}

type previewCarrierEdgeNode struct {
	previewSessionID                 string
	id                               string
	host                             string
	tcpPort                          int32
	quicPort                         int32
	processEpoch                     string
	carrierServerSPKISHA256          string
	carrierServerCertificateChainPEM string
	state                            string
	ready                            bool
	lastHeartbeat                    sql.NullTime
	drainDeadline                    sql.NullTime
	workerGeneration                 int64
}

func (n previewCarrierEdgeNode) valid(now time.Time) bool {
	if !validID(n.id) || !validID(n.previewSessionID) || connectorprotocol.ValidateOpaqueEpoch(n.processEpoch) != nil || !validCarrierServerSPKISHA256(n.carrierServerSPKISHA256) || len(n.carrierServerCertificateChainPEM) == 0 || len(n.carrierServerCertificateChainPEM) > 64<<10 || n.host == "" || strings.TrimSpace(n.host) != n.host || hasControl(n.host) || strings.ContainsAny(n.host, "/:@[]") || n.tcpPort < 1 || n.tcpPort > 65535 || n.quicPort < 1 || n.quicPort > 65535 || n.tcpPort == n.quicPort || n.workerGeneration <= 0 || n.state != "ready" || !n.ready {
		return false
	}
	if n.lastHeartbeat.Valid && !n.lastHeartbeat.Time.After(now.Add(-2*time.Minute)) {
		return false
	}
	if n.drainDeadline.Valid && !n.drainDeadline.Time.After(now) {
		return false
	}
	return true
}

func previewCarrierConfigHash(in PreviewCarrierAllocationRequest, edgeNodeID string, sessionID string, edgeProcessEpochs ...string) string {
	_ = edgeProcessEpochs
	seed := strings.Join([]string{
		"paperboat.preview-carrier-config.v1", in.Lease.AccountID,
		in.Lease.OwnerMachineID, fmt.Sprint(in.Proof.InstallationGeneration), edgeNodeID,
		in.Lease.OwnerMachineID, sessionID,
	}, "\x00")
	digest := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(digest[:])
}

const getPreviewCarrierEdgeNodeSQL = `
SELECT node.id, COALESCE(node.carrier_endpoint_host, ''), COALESCE(node.carrier_endpoint_tcp_port, 0), COALESCE(node.carrier_endpoint_quic_port, 0),
       node.process_epoch, COALESCE(node.carrier_server_spki_sha256, ''), COALESCE(node.carrier_server_certificate_chain_pem, ''), node.state, node.ready, node.last_heartbeat_at, node.drain_deadline,
       machine.worker_generation, COALESCE(active.preview_session_id,'')
FROM control_tunnel_nodes AS node
JOIN user_machines AS machine
  ON machine.id = $4 AND machine.user_id = $6
LEFT JOIN runtime_carrier_attachments AS active
  ON active.machine_id=machine.id AND active.account_id=$3
 AND active.installation_generation=$5 AND active.worker_generation=machine.worker_generation
 AND active.ready AND active.edge_node_id=node.id
 AND active.edge_process_epoch=node.process_epoch
 AND paperboat.runtime_carrier_valid(active.route_id,active.edge_node_id,active.edge_process_epoch,true)
WHERE node.id = $1
  AND paperboat.node_preview_eligible(node.id,$3,$7)
  AND (active.edge_node_id IS NOT NULL OR paperboat.workspace_edge_mode($3,paperboat.publication_workspace_team('preview',$7))='self-hosted-only')
  AND node.state = 'ready'
  AND node.ready
  AND node.carrier_endpoint_host IS NOT NULL
  AND node.carrier_endpoint_tcp_port IS NOT NULL
  AND node.carrier_endpoint_quic_port IS NOT NULL
	AND node.carrier_server_spki_sha256 IS NOT NULL
	AND node.carrier_server_certificate_chain_pem IS NOT NULL
  AND (node.last_heartbeat_at IS NULL OR node.last_heartbeat_at > $2::timestamptz - interval '2 minutes')
  AND (node.drain_deadline IS NULL OR node.drain_deadline > $2::timestamptz)
  AND machine.installation_generation = $5
  AND machine.state = 'online'
  AND machine.online
  AND machine.deleted_at IS NULL
  AND machine.revoked_at IS NULL
  AND machine.public_identity_key IS NOT NULL
  AND machine.worker_generation > 0
FOR UPDATE OF node, machine`
