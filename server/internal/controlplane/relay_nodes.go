package controlplane

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"net/http"
	"strings"
	"time"
)

const relayNodeLease = 15 * time.Second
const relayRegistryLease = time.Minute

type relayNodeStart struct {
	NodeID             string `json:"node_id"`
	ExpectedGeneration int64  `json:"expected_generation"`
	ProcessEpoch       string `json:"process_epoch"`
}
type relaySubject struct {
	AccountID  string `json:"account_id"`
	EndpointID string `json:"endpoint_id"`
	Generation int64  `json:"generation"`
}
type relayNodeObservation struct {
	NodeID       string `json:"node_id"`
	Generation   int64  `json:"node_generation"`
	ProcessEpoch string `json:"process_epoch"`
	Ready        bool   `json:"ready"`
	Draining     bool   `json:"draining"`
	CapacityUsed int64  `json:"capacity_used"`
	// Subjects is the full admitted list. A relay sends it only when the list changed or SubjectsRequired asked for it.
	Subjects []relaySubject `json:"subjects"`
	// SubjectsDigest is relaySubjectsDigest of the relay's list. A relay that sends it may omit Subjects when the
	// server still holds that list for the node's generation and epoch.
	SubjectsDigest string `json:"subjects_digest,omitempty"`
	// QuotaDeniedSupported is sent by relays that understand LeaseDocument.QuotaDenied. Relays
	// decode leases strictly, so an older relay must never be sent the field.
	QuotaDeniedSupported bool `json:"quota_denied_supported,omitempty"`
}
type relayNodeLeaseDocument struct {
	NodeID        string                `json:"node_id"`
	Generation    int64                 `json:"node_generation"`
	ProcessEpoch  string                `json:"process_epoch"`
	ExpiresAt     int64                 `json:"expires_at"`
	CapacityLimit int64                 `json:"capacity_limit"`
	Revocations   []relaySubject        `json:"revocations"`
	PeerRelay     *relayServiceIdentity `json:"peer_relay,omitempty"`
	// QuotaDenied lists connected accounts whose workspaces are all out of bandwidth allowance:
	// the relay refuses their new admissions and closes their carriers (typed), until a later
	// observation no longer lists them. It never carries authority.
	QuotaDenied []string `json:"quota_denied,omitempty"`
	// SubjectsRequired asks a relay that sent SubjectsDigest for its full list; no revocations are given with it.
	// Only relays that sent a digest are ever sent the field.
	SubjectsRequired bool `json:"subjects_required,omitempty"`
}
type relayServiceIdentity struct {
	WireGuardPublicKey string `json:"wireguard_public_key"`
	DiscoPublicKey     string `json:"disco_public_key"`
	VirtualAddress     string `json:"virtual_address"`
}

func relayServiceForNode(ctx context.Context, tx *sql.Tx, node string) (*relayServiceIdentity, error) {
	var enabled bool
	var wg, disco []byte
	var address sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT roles @> ARRAY['peer_relay']::text[],peer_relay_wireguard_public_key,peer_relay_disco_public_key,host(peer_relay_virtual_address) FROM paperboat.control_tunnel_nodes WHERE id=$1`, node).Scan(&enabled, &wg, &disco, &address); err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	if len(wg) != 32 || len(disco) != 32 || !address.Valid {
		return nil, invalidUsage("relay_nodes.go:67")
	}
	return &relayServiceIdentity{base64.RawURLEncoding.EncodeToString(wg), base64.RawURLEncoding.EncodeToString(disco), address.String}, nil
}

func relayIdentifier(s string) bool { return len(s) > 0 && len(s) <= 256 }

// StartRelayNode activates an existing operator-authorized registry entry. This
// API cannot create nodes or change region, account policy, endpoints or keys.
func (s *EdgeService) StartRelayNode(ctx context.Context, in relayNodeStart) (relayNodeLeaseDocument, error) {
	var out relayNodeLeaseDocument
	if !relayIdentifier(in.NodeID) || !relayIdentifier(in.ProcessEpoch) || in.ExpectedGeneration < 1 || in.ExpectedGeneration == int64(^uint64(0)>>1) {
		return out, invalidUsage("relay_nodes.go:79")
	}
	tx, err := s.store.SQL().BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var epoch, state string
	var gen, capacity int64
	err = tx.QueryRowContext(ctx, `SELECT n.node_generation,n.process_epoch,n.capacity_limit,n.state FROM paperboat.control_tunnel_nodes n LEFT JOIN paperboat.managed_node_authority m ON m.node_id=n.id LEFT JOIN paperboat.operator_regions r ON r.id=m.region_id WHERE n.id=$1 AND n.roles @> ARRAY['relay']::text[] AND n.transports @> ARRAY['derp_quic','derp_wss']::text[] AND n.region IS NOT NULL AND (m.node_id IS NULL OR (m.retired_at IS NULL AND r.enabled)) FOR UPDATE OF n`, in.NodeID).Scan(&gen, &epoch, &capacity, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return out, invalidUsage("relay_nodes.go:90")
	}
	if err != nil {
		return out, err
	}
	if !relayNodeMayStart(state, capacity) {
		return out, invalidUsage("relay_nodes.go:96")
	}
	if epoch == in.ProcessEpoch && gen == in.ExpectedGeneration+1 {
		// A live startup retry preserves the node's existing readiness.
		var expires time.Time
		if err = tx.QueryRowContext(ctx, `SELECT registry_expires_at FROM paperboat.control_tunnel_nodes WHERE id=$1`, in.NodeID).Scan(&expires); err != nil {
			return out, err
		}
		if !expires.After(s.clock()) {
			// An interrupted startup never published readiness. Its persisted
			// pending epoch can safely resume without granting a second process.
			if state != "registered" && state != "offline" {
				return out, invalidUsage("relay_nodes.go:108")
			}
			now := s.clock().UTC()
			expires = now.Add(relayRegistryLease)
			if _, err = tx.ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET registry_expires_at=$2,last_heartbeat_at=$3,capacity_observed_at=$3 WHERE id=$1`, in.NodeID, expires, now); err != nil {
				return out, err
			}
		}
		out = relayNodeLeaseDocument{NodeID: in.NodeID, Generation: gen, ProcessEpoch: epoch, ExpiresAt: min(expires.Unix(), s.clock().Add(relayNodeLease).Unix()), CapacityLimit: capacity, Revocations: []relaySubject{}}
	} else {
		if gen != in.ExpectedGeneration || epoch == in.ProcessEpoch {
			return out, invalidUsage("relay_nodes.go:119")
		}
		now := s.clock().UTC()
		expires := now.Add(relayRegistryLease)
		_, err = tx.ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET node_generation=node_generation+1,process_epoch=$2,state='registered',ready=false,capacity_used=0,capacity_observed_at=$3,last_heartbeat_at=$3,registry_expires_at=$4,drain_deadline=NULL,updated_at=$3,version=version+1 WHERE id=$1`, in.NodeID, in.ProcessEpoch, now, expires)
		if err != nil {
			return out, err
		}
		out = relayNodeLeaseDocument{NodeID: in.NodeID, Generation: gen + 1, ProcessEpoch: in.ProcessEpoch, ExpiresAt: now.Add(relayNodeLease).Unix(), CapacityLimit: capacity, Revocations: []relaySubject{}}
	}
	out.PeerRelay, err = relayServiceForNode(ctx, tx, in.NodeID)
	if err != nil {
		return relayNodeLeaseDocument{}, err
	}
	return out, tx.Commit()
}

func (s *EdgeService) ObserveRelayNode(ctx context.Context, in relayNodeObservation) (relayNodeLeaseDocument, error) {
	var out relayNodeLeaseDocument
	if !relayIdentifier(in.NodeID) || !relayIdentifier(in.ProcessEpoch) || in.Generation < 1 || in.CapacityUsed < 0 || len(in.Subjects) > relaylimits.MaxConnectionsPerNode || (in.SubjectsDigest != "" && !validRelaySubjectsDigest(in.SubjectsDigest)) {
		return out, invalidUsage("relay_nodes.go:139")
	}
	seen := map[string]bool{}
	for _, subject := range in.Subjects {
		k := subject.AccountID + "\x00" + subject.EndpointID
		if !relayIdentifier(subject.AccountID) || !relayIdentifier(subject.EndpointID) || strings.ContainsAny(subject.AccountID+subject.EndpointID, "\x00\n") || subject.Generation < 1 || subject.Generation == int64(^uint64(0)>>1) || seen[k] {
			return out, invalidUsage("relay_nodes.go:145")
		}
		seen[k] = true
	}
	tx, err := s.store.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	now := s.clock().UTC()
	var capacity int64
	var expires time.Time
	var state string
	err = tx.QueryRowContext(ctx, `SELECT capacity_limit,registry_expires_at,state FROM paperboat.control_tunnel_nodes WHERE id=$1 AND node_generation=$2 AND process_epoch=$3 AND roles @> ARRAY['relay']::text[] FOR UPDATE`, in.NodeID, in.Generation, in.ProcessEpoch).Scan(&capacity, &expires, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return out, invalidUsage("relay_nodes.go:160")
	}
	if err != nil {
		return out, err
	}
	if state == "retired" || (state == "offline" && !in.Draining) || !expires.After(now) || in.CapacityUsed > capacity {
		return out, invalidUsage("relay_nodes.go:166")
	}
	if state == "draining" && !in.Draining {
		return out, invalidUsage("relay_nodes.go:169")
	}
	expires = now.Add(relayRegistryLease)
	operatorDraining := state == "draining"
	state = "registered"
	if in.Ready {
		state = "ready"
	}
	// Process shutdown withdraws readiness but does not create operator intent.
	// Repeated draining observations retain the acknowledged shutdown lease.
	if in.Draining {
		state = "offline"
	}
	if operatorDraining {
		state = "draining"
	}
	_, err = tx.ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET state=$4,ready=$5,capacity_used=$6,capacity_observed_at=$7,last_heartbeat_at=$7,registry_expires_at=$8,drain_deadline=CASE WHEN $4='draining' THEN coalesce(drain_deadline,$7) ELSE NULL END,updated_at=$7 WHERE id=$1 AND node_generation=$2 AND process_epoch=$3`, in.NodeID, in.Generation, in.ProcessEpoch, state, in.Ready && !in.Draining, in.CapacityUsed, now, expires)
	if err != nil {
		return out, err
	}
	out = relayNodeLeaseDocument{NodeID: in.NodeID, Generation: in.Generation, ProcessEpoch: in.ProcessEpoch, ExpiresAt: now.Add(relayNodeLease).Unix(), CapacityLimit: capacity, Revocations: []relaySubject{}}
	set, required, err := relayObservedSubjects(ctx, tx, in)
	if err != nil {
		return relayNodeLeaseDocument{}, err
	}
	out.SubjectsRequired = required
	if !required {
		if out.Revocations, err = relayRevocations(ctx, tx, in.NodeID, set); err != nil {
			return relayNodeLeaseDocument{}, err
		}
	}
	out.PeerRelay, err = relayServiceForNode(ctx, tx, in.NodeID)
	if err != nil {
		return relayNodeLeaseDocument{}, err
	}
	if err := tx.Commit(); err != nil {
		return relayNodeLeaseDocument{}, err
	}
	// After the commit and outside the transaction: read-only, cached, bounded, fail-open.
	if s.relayQuota != nil && in.QuotaDeniedSupported {
		accounts := make([]string, 0, len(set.list))
		listed := map[string]bool{}
		for _, subject := range set.list {
			if !listed[subject.AccountID] {
				listed[subject.AccountID] = true
				accounts = append(accounts, subject.AccountID)
			}
		}
		out.QuotaDenied = s.relayQuota.DeniedAmong(ctx, accounts, now, relayQuotaFreshLookups)
	}
	return out, nil
}

// relayQuotaFreshLookups bounds the uncached allowance reads one relay observation may cost.
const relayQuotaFreshLookups = 64

func (s *EdgeService) handleRelayStart(w http.ResponseWriter, r *http.Request) {
	var in relayNodeStart
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.StartRelayNode(r.Context(), in)
	if err != nil {
		if !errors.Is(err, ErrInvalidUsageReport) {
			writeEdgeError(w, r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, err)
			return
		}
		writeEdgeError(w, r, http.StatusConflict, "relay_node_fenced", false, 0, err)
		return
	}
	writeEdgeJSON(w, http.StatusOK, out)
}

// maxRelayObservationDocument fits a full subject list of relaylimits.MaxConnectionsPerNode subjects whose
// identifiers are at their 256-byte limit, which the general edge document limit does not.
const maxRelayObservationDocument = 16 << 20

func (s *EdgeService) handleRelayObserve(w http.ResponseWriter, r *http.Request) {
	var in relayNodeObservation
	if !s.decodeLimit(w, r, &in, maxRelayObservationDocument) {
		return
	}
	out, err := s.ObserveRelayNode(r.Context(), in)
	if err != nil {
		if !errors.Is(err, ErrInvalidUsageReport) {
			writeEdgeError(w, r, http.StatusServiceUnavailable, "control_unavailable", true, 1000, err)
			return
		}
		writeEdgeError(w, r, http.StatusConflict, "relay_node_fenced", false, 0, err)
		return
	}
	writeEdgeJSON(w, http.StatusOK, out)
}

// relayNodeMayStart admits a relay process start for a node that is not retired or draining and whose
// registered capacity is within the ceiling the relay itself enforces (shared/relaylimits).
func relayNodeMayStart(state string, capacity int64) bool {
	return state != "retired" && state != "draining" && capacity > 0 && capacity <= relaylimits.MaxConnectionsPerNode
}
