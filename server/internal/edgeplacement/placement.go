// Package edgeplacement holds a machine's measured edge preference. Preferences
// affect ordering only; every consumer must independently check live admission.
package edgeplacement

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

var ErrInvalid = errors.New("invalid edge placement")

// Candidates returns only currently usable edges in the owning workspace's
// browser-edge pool. Addresses are for bounded latency probes, never bearer authority.
func Candidates(ctx context.Context, tx *db.Tx, accountID string, teamID sql.NullString, now time.Time) ([]runtimewire.EdgeCandidate, error) {
	if ctx == nil || tx == nil || connectorprotocol.ValidateIdentifier(accountID) != nil || now.IsZero() {
		return nil, ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT n.id,n.process_epoch,coalesce(n.region,''),coalesce(n.failure_domain,''),n.carrier_endpoint_host,n.carrier_endpoint_tcp_port
FROM control_tunnel_nodes n
WHERE n.ready AND n.state='ready' AND n.roles @> ARRAY['edge']::text[]
AND n.transports @> ARRAY['http2','http3']::text[]
AND n.last_heartbeat_at>$2::timestamptz-interval '30 seconds'
AND n.registry_expires_at>$2 AND n.capacity_observed_at>$2::timestamptz-interval '30 seconds'
AND n.capacity_used<n.capacity_limit-n.capacity_limit/10
AND n.drain_deadline IS NULL AND paperboat.node_workspace_edge_eligible(n.id,$1,$4)
AND paperboat.workspace_edge_set_allows(n.id,$1,$4)
AND n.carrier_endpoint_host IS NOT NULL AND n.carrier_endpoint_tcp_port IS NOT NULL
AND n.carrier_server_spki_sha256 IS NOT NULL AND n.carrier_server_certificate_chain_pem IS NOT NULL
ORDER BY n.id LIMIT $3`, accountID, now, runtimewire.MaximumEdgeCandidates, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]runtimewire.EdgeCandidate, 0, runtimewire.MaximumEdgeCandidates)
	for rows.Next() {
		var item runtimewire.EdgeCandidate
		var host string
		var port int32
		if err := rows.Scan(&item.NodeID, &item.ProcessEpoch, &item.Region, &item.FailureDomain, &host, &port); err != nil {
			rows.Close()
			if rowErr := rows.Err(); rowErr != nil {
				return nil, errors.Join(err, rowErr)
			}
			return nil, err
		}
		if connectorprotocol.ValidateIdentifier(item.NodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(item.ProcessEpoch) != nil || port < 1 || port > 65535 {
			continue
		}
		item.ProbeAddress = net.JoinHostPort(host, fmt.Sprint(port))
		result = append(result, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// Save accepts only a pair from the current server-issued candidate set. A
// stale report is ignored so a network change cannot strand the machine.
func Save(ctx context.Context, tx *db.Tx, accountID, machineID string, installationGeneration, workerGeneration uint64, preference runtimewire.EdgePreference, candidates []runtimewire.EdgeCandidate, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || connectorprotocol.ValidateIdentifier(accountID) != nil || connectorprotocol.ValidateIdentifier(machineID) != nil || installationGeneration == 0 || workerGeneration == 0 || now.IsZero() {
		return false, ErrInvalid
	}
	lookup := make(map[string]runtimewire.EdgeCandidate, len(candidates))
	for _, candidate := range candidates {
		lookup[candidate.NodeID+"\x00"+candidate.ProcessEpoch] = candidate
	}
	primary, ok := lookup[preference.PrimaryNodeID+"\x00"+preference.PrimaryProcessEpoch]
	if !ok {
		return false, nil
	}
	if preference.SecondaryNodeID != "" || preference.SecondaryProcessEpoch != "" {
		secondary, exists := lookup[preference.SecondaryNodeID+"\x00"+preference.SecondaryProcessEpoch]
		if !exists || secondary.NodeID == primary.NodeID || primary.FailureDomain == "" || secondary.FailureDomain == "" || primary.FailureDomain == secondary.FailureDomain {
			return false, nil
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO machine_edge_placements(machine_id,account_id,installation_generation,worker_generation,primary_node_id,primary_process_epoch,secondary_node_id,secondary_process_epoch,observed_at)
VALUES($1,$2,$3,$4,$5,$6,nullif($7,''),nullif($8,''),$9)
ON CONFLICT(machine_id) DO UPDATE SET account_id=EXCLUDED.account_id,installation_generation=EXCLUDED.installation_generation,worker_generation=EXCLUDED.worker_generation,primary_node_id=EXCLUDED.primary_node_id,primary_process_epoch=EXCLUDED.primary_process_epoch,secondary_node_id=EXCLUDED.secondary_node_id,secondary_process_epoch=EXCLUDED.secondary_process_epoch,observed_at=EXCLUDED.observed_at`, machineID, accountID, installationGeneration, workerGeneration, preference.PrimaryNodeID, preference.PrimaryProcessEpoch, preference.SecondaryNodeID, preference.SecondaryProcessEpoch, now)
	return err == nil, err
}
