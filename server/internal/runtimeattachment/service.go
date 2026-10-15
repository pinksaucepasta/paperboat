// Package runtimeattachment owns the automatic installed-machine runtime route.
// Admissions bind existing runtime routes to authenticated, expiring carriers;
// they never create a product tunnel or grant arbitrary loopback access.
package runtimeattachment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/admission"
	"io"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/edgeplacement"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
)

const Schema = "paperboat.runtime-carrier/v1"
const MaximumAdmissions = 4096

var ErrInvalid = errors.New("invalid runtime carrier request")
var ErrUnavailable = errors.New("runtime carrier unavailable")
var ErrStale = errors.New("stale runtime carrier binding")
var errInvalidMachineIdentityKeySize = errors.New("runtime machine identity key has invalid size")

type staticFailure struct {
	message string
	cause   error
}

func (e staticFailure) Error() string { return e.message }

func (e staticFailure) Unwrap() error { return e.cause }

type Request struct {
	OperationID            string                    `json:"operation_id"`
	ProcessID              string                    `json:"process_id"`
	WorkerGeneration       uint64                    `json:"worker_generation"`
	InstallationGeneration uint64                    `json:"installation_generation"`
	RuntimeListenPort      int32                     `json:"runtime_listen_port"`
	EdgePreference         *edgeplacement.Preference `json:"edge_preference,omitempty"`
	ActiveEdgeNodeID       string                    `json:"active_edge_node_id,omitempty"`
	ActiveEdgeProcessEpoch string                    `json:"active_edge_process_epoch,omitempty"`
}
type Binding struct {
	AccountID                            string `json:"account_id"`
	HostID                               string `json:"host_id"`
	TunnelID                             string `json:"tunnel_id"`
	ConnectorID                          string `json:"connector_id"`
	SessionID                            string `json:"session_id"`
	ProcessGeneration                    uint64 `json:"process_generation"`
	ConfigGeneration                     uint64 `json:"config_generation"`
	RouteID                              string `json:"route_id"`
	RouteGeneration                      uint64 `json:"route_generation"`
	InstallationGeneration               uint64 `json:"installation_generation"`
	EdgeNodeID                           string `json:"edge_node_id"`
	EdgeProcessEpoch                     string `json:"edge_process_epoch"`
	EdgeCarrierServerSPKISHA256          string `json:"edge_carrier_server_spki_sha256"`
	EdgeCarrierServerCertificateChainPEM string `json:"edge_carrier_server_certificate_chain_pem"`
	MachineIdentityPublicKey             string `json:"machine_identity_public_key"`
	MachineIdentityThumbprint            string `json:"machine_identity_thumbprint"`
}
type Admission struct {
	Schema               string    `json:"schema"`
	Binding              Binding   `json:"binding"`
	AttachmentGeneration uint64    `json:"attachment_generation"`
	ConfigContentHash    string    `json:"config_content_hash"`
	EdgeEndpoints        []string  `json:"edge_endpoints"`
	Hostname             string    `json:"hostname"`
	RouteKind            string    `json:"route_kind"`
	RouteRevision        uint64    `json:"route_revision"`
	ExpiresAt            time.Time `json:"expires_at"`
}
type HostAdmission struct {
	Admission
	EdgeCandidates         []edgeplacement.Candidate `json:"edge_candidates"`
	Backup                 *Admission                `json:"backup,omitempty"`
	ActiveEdgeNodeID       string                    `json:"active_edge_node_id,omitempty"`
	ActiveEdgeProcessEpoch string                    `json:"active_edge_process_epoch,omitempty"`
}
type Observation struct {
	RouteID              string `json:"route_id"`
	AttachmentGeneration uint64 `json:"attachment_generation"`
	SessionID            string `json:"session_id"`
	Ready                bool   `json:"ready"`
}
type Service struct {
	DB                *db.DB
	AdmissionLifetime time.Duration
}

func validRequest(p previewattachment.MachineProof, r Request) bool {
	return p.Validate() == nil && r.OperationID == p.OperationID && connectorprotocol.ValidateOpaqueEpoch(r.ProcessID) == nil && r.WorkerGeneration > 0 && r.WorkerGeneration <= 1<<62 && r.InstallationGeneration == p.InstallationGeneration && r.RuntimeListenPort >= 1024 && r.RuntimeListenPort <= 65535 && ((r.ActiveEdgeNodeID == "" && r.ActiveEdgeProcessEpoch == "") || (connectorprotocol.ValidateIdentifier(r.ActiveEdgeNodeID) == nil && connectorprotocol.ValidateOpaqueEpoch(r.ActiveEdgeProcessEpoch) == nil))
}

func (s *Service) Attach(ctx context.Context, proof previewattachment.MachineProof, request Request) (HostAdmission, error) {
	var out HostAdmission
	if !validRequest(proof, request) {
		return out, ErrInvalid
	}
	err := s.DB.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var route, environment, host, managedHost, publicKey, mode string
		var revision int64
		var teamID sql.NullString
		// Serialize attachment renewal/replacement with machine generation changes.
		err := tx.QueryRow(ctx, `SELECT r.id,r.environment_id,r.public_host,r.desired_revision,m.public_identity_key,m.owner_team_id,r.managed_public_host,paperboat.workspace_edge_mode(m.user_id,m.owner_team_id)
FROM user_machines m JOIN users u ON u.id=m.user_id JOIN control_environments e ON e.id=m.environment_id
JOIN control_routes r ON r.environment_id=e.id AND r.connector_id='runtime' AND r.kind='runtime_https_wss'
WHERE m.id=$1 AND m.user_id=$2 AND m.installation_generation=$3 AND m.worker_generation=$4
AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.seat_state='occupied' AND u.status='active'
AND e.desired_state='active' AND r.desired_state IN ('attached','replacing')
ORDER BY r.id LIMIT 1 FOR UPDATE OF m,r`, proof.MachineID, proof.UserID, proof.InstallationGeneration, request.WorkerGeneration).Scan(&route, &environment, &host, &revision, &publicKey, &teamID, &managedHost, &mode)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		// The machine owns its loopback listener. Keep the route record accurate
		// for diagnostics; the edge never dials this target directly.
		if _, err := tx.Exec(ctx, `UPDATE control_routes SET target_port=$2 WHERE id=$1 AND target_port<>$2`, route, request.RuntimeListenPort); err != nil {
			return err
		}
		key, err := base64.RawURLEncoding.Strict().DecodeString(publicKey)
		if err != nil {
			return staticFailure{message: ErrUnavailable.Error(), cause: errors.Join(ErrUnavailable, err)}
		}
		if len(key) != 32 {
			return staticFailure{message: ErrUnavailable.Error(), cause: errors.Join(ErrUnavailable, errInvalidMachineIdentityKeySize)}
		}
		previous := map[string]Admission{}
		previewSessions := map[string]string{}
		var generation uint64
		var activeNode, activeEpoch string
		previousRows, err := tx.Query(ctx, `SELECT edge_node_id,admission,process_id,generation,active,preview_session_id FROM runtime_carrier_attachments WHERE route_id=$1 FOR UPDATE`, route)
		if err != nil {
			return err
		}
		for previousRows.Next() {
			var nodeID, processID, previewSessionID string
			var raw []byte
			var priorGeneration uint64
			var active bool
			if err = previousRows.Scan(&nodeID, &raw, &processID, &priorGeneration, &active, &previewSessionID); err != nil {
				break
			}
			var a Admission
			if err = json.Unmarshal(raw, &a); err != nil {
				break
			}
			if a.Binding.ProcessGeneration == request.WorkerGeneration && a.Binding.InstallationGeneration == request.InstallationGeneration && processID != request.ProcessID {
				err = ErrStale
				break
			}
			previous[nodeID] = a
			previewSessions[nodeID] = previewSessionID
			if priorGeneration > generation {
				generation = priorGeneration
			}
			if active {
				activeNode, activeEpoch = nodeID, a.Binding.EdgeProcessEpoch
			}
		}
		previousRows.Close()
		if err == nil {
			err = previousRows.Err()
		}
		if err != nil {
			return err
		}
		candidates, err := edgeplacement.Candidates(ctx, tx, proof.UserID, teamID, time.Now().UTC())
		if err != nil {
			return err
		}
		if request.EdgePreference != nil {
			if _, err := edgeplacement.Save(ctx, tx, proof.UserID, proof.MachineID, request.InstallationGeneration, request.WorkerGeneration, *request.EdgePreference, candidates, time.Now().UTC()); err != nil {
				return err
			}
		}
		type edgeRow struct {
			node, epoch, domain, endpoint, browserHost, pin, chain string
			tcp, quic                                              int64
		}
		selected := make([]edgeRow, 0, 2)
		rows, err := tx.Query(ctx, `SELECT n.id,n.process_epoch,coalesce(n.failure_domain,''),n.carrier_endpoint_host,coalesce(n.endpoint_host,''),n.carrier_endpoint_tcp_port,n.carrier_endpoint_quic_port,n.carrier_server_spki_sha256,n.carrier_server_certificate_chain_pem
FROM control_tunnel_nodes n LEFT JOIN machine_edge_placements p ON p.machine_id=$3 AND p.account_id=$1
AND p.installation_generation=$4 AND p.worker_generation=$5 AND p.observed_at>now()-interval '2 minutes'
WHERE n.ready AND n.state='ready' AND n.roles @> ARRAY['edge']::text[]
AND n.transports @> ARRAY['http2','http3']::text[] AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
AND n.capacity_observed_at>now()-interval '30 seconds' AND n.capacity_used<n.capacity_limit-n.capacity_limit/10
AND (n.drain_deadline IS NULL OR n.drain_deadline>now()) AND paperboat.node_workspace_edge_eligible(n.id,$1,$6)
AND n.carrier_endpoint_host IS NOT NULL AND n.carrier_endpoint_tcp_port IS NOT NULL AND n.carrier_endpoint_quic_port IS NOT NULL
AND n.carrier_server_spki_sha256 IS NOT NULL AND n.carrier_server_certificate_chain_pem IS NOT NULL
ORDER BY CASE WHEN n.id=p.primary_node_id AND n.process_epoch=p.primary_process_epoch THEN 0
WHEN n.id=p.secondary_node_id AND n.process_epoch=p.secondary_process_epoch THEN 1
WHEN n.id=$2 THEN 2 ELSE 3 END,n.id LIMIT 32 FOR SHARE OF n`, proof.UserID, activeNode, proof.MachineID, request.InstallationGeneration, request.WorkerGeneration, teamID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var edge edgeRow
			if err = rows.Scan(&edge.node, &edge.epoch, &edge.domain, &edge.endpoint, &edge.browserHost, &edge.tcp, &edge.quic, &edge.pin, &edge.chain); err != nil {
				break
			}
			if edge.tcp < 1 || edge.tcp > 65535 || edge.quic < 1 || edge.quic > 65535 || connectorprotocol.ValidateOpaqueEpoch(edge.epoch) != nil {
				continue
			}
			if len(selected) == 0 || (selected[0].domain != "" && edge.domain != "" && edge.domain != selected[0].domain) {
				selected = append(selected, edge)
			}
			if len(selected) == 2 {
				break
			}
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		if err != nil {
			return err
		}
		if len(selected) == 0 {
			return ErrUnavailable
		}

		nextHost := managedHost
		switch mode {
		case "managed-only":
			if managedHost == "" {
				return ErrUnavailable
			}
		case "self-hosted-only":
			// A customer alias is a signed routing key. All public TLS uses the
			// selected edge's enrolled infrastructure hostname, not this alias.
			nextHost = host
			if host == managedHost {
				label := sha256.Sum256([]byte(proof.MachineID))
				nextHost = "machine-" + hex.EncodeToString(label[:16]) + "." + selected[0].browserHost
			}
			if nextHost == "" || len(nextHost) > 253 || nextHost == managedHost || selected[0].browserHost == "" {
				return ErrUnavailable
			}
		default:
			return ErrUnavailable
		}
		if nextHost != host {
			if err := tx.QueryRow(ctx, `UPDATE control_routes SET public_host=$2,desired_revision=desired_revision+1 WHERE id=$1 RETURNING desired_revision`, route, nextHost).Scan(&revision); err != nil {
				return err
			}
			host = nextHost
		}
		digest := sha256.Sum256(key)
		thumb := "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:])
		out.EdgeCandidates = candidates
		for i, edge := range selected {
			prior := previous[edge.node]
			same := prior.Binding.InstallationGeneration == request.InstallationGeneration && prior.Binding.ProcessGeneration == request.WorkerGeneration && prior.Binding.EdgeProcessEpoch == edge.epoch && prior.Binding.MachineIdentityPublicKey == publicKey && prior.RouteRevision == uint64(revision) && prior.Binding.EdgeCarrierServerSPKISHA256 == edge.pin
			rowGeneration := prior.AttachmentGeneration
			if !same {
				generation++
				rowGeneration = generation
			}
			sessionID := prior.Binding.SessionID
			if !same {
				sessionID, err = newCarrierSessionID(rand.Reader)
				if err != nil {
					return err
				}
			}
			previewSessionID := previewSessions[edge.node]
			if prior.Binding.InstallationGeneration != request.InstallationGeneration || prior.Binding.ProcessGeneration != request.WorkerGeneration || prior.Binding.MachineIdentityPublicKey != publicKey {
				previewSessionID, err = newCarrierSessionID(rand.Reader)
				if err != nil {
					return err
				}
			}
			binding := Binding{AccountID: proof.UserID, HostID: proof.MachineID, TunnelID: route, ConnectorID: proof.MachineID, SessionID: sessionID, ProcessGeneration: request.WorkerGeneration, ConfigGeneration: uint64(revision), RouteID: route, RouteGeneration: uint64(revision), InstallationGeneration: request.InstallationGeneration, EdgeNodeID: edge.node, EdgeProcessEpoch: edge.epoch, EdgeCarrierServerSPKISHA256: edge.pin, EdgeCarrierServerCertificateChainPEM: edge.chain, MachineIdentityPublicKey: publicKey, MachineIdentityThumbprint: thumb}
			encoded, _ := json.Marshal(binding)
			hash := sha256.Sum256(encoded)
			admission := Admission{Schema: Schema, Binding: binding, AttachmentGeneration: rowGeneration, ConfigContentHash: "sha256:" + hex.EncodeToString(hash[:]), EdgeEndpoints: []string{"h2://" + net.JoinHostPort(edge.endpoint, fmt.Sprint(edge.tcp)), "h3://" + net.JoinHostPort(edge.endpoint, fmt.Sprint(edge.quic))}, Hostname: host, RouteKind: "runtime_https_wss", RouteRevision: uint64(revision), ExpiresAt: admission.Deadline(time.Now(), s.AdmissionLifetime)}
			encoded, err = json.Marshal(admission)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT(route_id,edge_node_id) DO UPDATE SET machine_id=EXCLUDED.machine_id,account_id=EXCLUDED.account_id,installation_generation=EXCLUDED.installation_generation,worker_generation=EXCLUDED.worker_generation,process_id=EXCLUDED.process_id,generation=EXCLUDED.generation,edge_process_epoch=EXCLUDED.edge_process_epoch,public_identity_key=EXCLUDED.public_identity_key,route_revision=EXCLUDED.route_revision,session_id=EXCLUDED.session_id,preview_session_id=EXCLUDED.preview_session_id,admission=EXCLUDED.admission,expires_at=EXCLUDED.expires_at,ready=CASE WHEN $16 THEN runtime_carrier_attachments.ready ELSE false END,observed_at=CASE WHEN $16 THEN runtime_carrier_attachments.observed_at ELSE NULL END`, route, proof.MachineID, proof.UserID, request.InstallationGeneration, request.WorkerGeneration, request.ProcessID, rowGeneration, edge.node, edge.epoch, publicKey, revision, binding.SessionID, previewSessionID, encoded, admission.ExpiresAt, same)
			if err != nil {
				return err
			}
			if i == 0 {
				out.Admission = admission
			} else {
				out.Backup = &admission
			}
		}
		// Keep a working active carrier during replacement, but immediately
		// retire one whose authority is no longer valid.
		if activeNode != "" {
			var valid bool
			if err = tx.QueryRow(ctx, `SELECT paperboat.runtime_carrier_valid($1,$2,$3,false)`, route, activeNode, activeEpoch).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				if _, err = tx.Exec(ctx, `UPDATE runtime_carrier_attachments SET active=false WHERE route_id=$1 AND active`, route); err != nil {
					return err
				}
				activeNode, activeEpoch = "", ""
			}
		}
		if activeNode == "" {
			activeNode, activeEpoch = selected[0].node, selected[0].epoch
			if _, err = tx.Exec(ctx, `UPDATE runtime_carrier_attachments SET active=true WHERE route_id=$1 AND edge_node_id=$2`, route, activeNode); err != nil {
				return err
			}
		}
		if request.ActiveEdgeNodeID != "" && (request.ActiveEdgeNodeID != activeNode || request.ActiveEdgeProcessEpoch != activeEpoch) {
			var ready bool
			for _, edge := range selected {
				if edge.node == request.ActiveEdgeNodeID && edge.epoch == request.ActiveEdgeProcessEpoch {
					err = tx.QueryRow(ctx, `SELECT paperboat.runtime_carrier_valid($1,$2,$3,true)`, route, edge.node, edge.epoch).Scan(&ready)
					if err != nil {
						return err
					}
					break
				}
			}
			if ready {
				if _, err = tx.Exec(ctx, `UPDATE runtime_carrier_attachments SET active=false WHERE route_id=$1 AND active`, route); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, `UPDATE runtime_carrier_attachments SET active=true WHERE route_id=$1 AND edge_node_id=$2 AND edge_process_epoch=$3`, route, request.ActiveEdgeNodeID, request.ActiveEdgeProcessEpoch); err != nil {
					return err
				}
				activeNode, activeEpoch = request.ActiveEdgeNodeID, request.ActiveEdgeProcessEpoch
				if _, err = tx.Exec(ctx, `UPDATE control_connector_generations SET state='admitted',edge_node_id=$2,version=version+1,updated_at=now() WHERE environment_id=$1 AND connector_id='runtime'`, environment, activeNode); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, `UPDATE control_routes SET applied_node_id=$2,applied_revision=desired_revision,applied_generation=(SELECT generation FROM control_connector_generations WHERE environment_id=$3 AND connector_id='runtime'),version=version+1,updated_at=now() WHERE id=$1`, route, activeNode, environment); err != nil {
					return err
				}
			}
		}
		if _, err = tx.Exec(ctx, `DELETE FROM runtime_carrier_attachments WHERE route_id=$1 AND NOT active AND edge_node_id<>ALL($2::text[])`, route, []string{selected[0].node, func() string {
			if len(selected) > 1 {
				return selected[1].node
			}
			return selected[0].node
		}()}); err != nil {
			return err
		}
		out.ActiveEdgeNodeID, out.ActiveEdgeProcessEpoch = activeNode, activeEpoch
		return nil
	})
	return out, err
}

func (s *Service) Pull(ctx context.Context, edge previewattachment.PreviewEdgeIdentity) ([]Admission, error) {
	rows, err := s.DB.Pool().Query(ctx, `SELECT admission FROM runtime_carrier_attachments WHERE edge_node_id=$1 AND edge_process_epoch=$2 AND paperboat.runtime_carrier_valid(route_id,edge_node_id,edge_process_epoch,false) ORDER BY route_id LIMIT $3`, edge.NodeID, edge.ProcessEpoch, MaximumAdmissions+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Admission{}
	totalBytes := 0
	for rows.Next() {
		var raw []byte
		var a Admission
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		totalBytes += len(raw)
		if totalBytes > 16<<20 {
			return nil, ErrUnavailable
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		a.ExpiresAt = admission.Deadline(time.Now(), s.AdmissionLifetime)
		out = append(out, a)
		if len(out) > MaximumAdmissions {
			return nil, ErrUnavailable
		}
	}
	return out, rows.Err()
}

func (s *Service) Observe(ctx context.Context, edge previewattachment.PreviewEdgeIdentity, observations []Observation) error {
	if len(observations) > MaximumAdmissions {
		return ErrInvalid
	}
	return s.DB.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		seen := map[string]bool{}
		for _, o := range observations {
			if connectorprotocol.ValidateIdentifier(o.RouteID) != nil || connectorprotocol.ValidateIdentifier(o.SessionID) != nil || o.AttachmentGeneration == 0 || seen[o.RouteID] {
				return ErrInvalid
			}
			seen[o.RouteID] = true
			var active bool
			err := tx.QueryRow(ctx, `UPDATE runtime_carrier_attachments SET ready=$6,observed_at=now() WHERE route_id=$1 AND generation=$2 AND session_id=$3 AND edge_node_id=$4 AND edge_process_epoch=$5 AND (NOT $6 OR paperboat.runtime_carrier_valid(route_id,edge_node_id,edge_process_epoch,false)) RETURNING active`, o.RouteID, o.AttachmentGeneration, o.SessionID, edge.NodeID, edge.ProcessEpoch, o.Ready).Scan(&active)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStale
			}
			if err != nil {
				return err
			}
			if !active {
				continue
			}
			// Existing browser consumers retain their generation checks. Their queries
			// additionally require a current observed attachment, so legacy observation
			// writers cannot create readiness without a real carrier.
			_, err = tx.Exec(ctx, `UPDATE control_connector_generations c SET state=CASE WHEN $2 THEN 'admitted' ELSE 'pending' END,edge_node_id=CASE WHEN $2 THEN a.edge_node_id ELSE NULL END,version=c.version+1,updated_at=now() FROM runtime_carrier_attachments a,control_routes r WHERE a.route_id=$1 AND a.active AND r.id=a.route_id AND c.environment_id=r.environment_id AND c.connector_id='runtime' AND c.machine_id=a.machine_id`, o.RouteID, o.Ready)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE control_routes r SET applied_node_id=CASE WHEN $2 THEN a.edge_node_id ELSE NULL END,applied_revision=CASE WHEN $2 THEN a.route_revision ELSE 0 END,applied_generation=CASE WHEN $2 THEN c.generation ELSE 0 END,version=r.version+1,updated_at=now() FROM runtime_carrier_attachments a,control_connector_generations c WHERE r.id=$1 AND a.route_id=r.id AND a.active AND c.environment_id=r.environment_id AND c.connector_id='runtime' AND c.machine_id=a.machine_id`, o.RouteID, o.Ready)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func newCarrierSessionID(random io.Reader) (string, error) {
	id, err := uuid.NewRandomFromReader(random)
	if err != nil {
		return "", err
	}
	return "session_" + id.String(), nil
}
