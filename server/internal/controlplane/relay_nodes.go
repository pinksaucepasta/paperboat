package controlplane

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	sharedmetrics "github.com/pinksaucepasta/paperboat/shared/metrics"
	"github.com/pinksaucepasta/paperboat/shared/nodemetrics"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/bandwidthgate"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
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
	// Generation is the grant generation a relay without subjects_digest sends with each subject. Relays that
	// send subjects_digest send membership only.
	Generation int64 `json:"generation,omitempty"`
}

// relayRevocation is one lease entry. A relay without subjects_digest gets generation (the floor it must reach);
// a digest relay gets min_generation, the first grant generation still valid for that endpoint, and no generation.
type relayRevocation struct {
	AccountID     string `json:"account_id"`
	EndpointID    string `json:"endpoint_id"`
	Generation    int64  `json:"generation,omitempty"`
	MinGeneration int64  `json:"min_generation,omitempty"`
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
	// Metrics is the relay's version, CPU, memory and bytes forwarded; optional, an older relay sends none.
	Metrics *nodemetrics.Report `json:"metrics,omitempty"`
}

// relayAccountSpeed is one account a relay holds to less than the default speed.
type relayAccountSpeed struct {
	Account string  `json:"account"`
	Mbit    float64 `json:"mbit"`
}

type relayNodeLeaseDocument struct {
	NodeID        string                `json:"node_id"`
	Generation    int64                 `json:"node_generation"`
	ProcessEpoch  string                `json:"process_epoch"`
	ExpiresAt     int64                 `json:"expires_at"`
	CapacityLimit int64                 `json:"capacity_limit"`
	Revocations   []relayRevocation     `json:"revocations"`
	PeerRelay     *relayServiceIdentity `json:"peer_relay,omitempty"`
	// PoolHandover lists accounts whose relay pool no longer includes this node and when the relay closes their
	// carriers (Unix seconds): the old relay serves them until clients have moved to the new ones. Nil leaves the
	// relay's list as it is (the read failed); an empty list clears it.
	PoolHandover *[]relayPoolHandover `json:"pool_handover,omitempty"`
	// AccountMbit is the speed of one account on this relay in Mbit/s when it is alone on it, and AccountSpeeds the connected accounts
	// held to less: those past their plan's included relay bytes this month (the plan's slower speed) and those on more than one relay
	// (their speed divided among the relays), until a later observation no longer lists them. An account's speed is one bucket across
	// all relays: the shares of its relays add up to its speed. All empty on a customer-owned relay: Paperboat never limits it. They
	// never carry authority.
	AccountMbit   float64             `json:"account_mbit"`
	AccountSpeeds []relayAccountSpeed `json:"account_speeds,omitempty"`
	// SubjectsRequired asks a relay that sent SubjectsDigest for its full list; no revocations are given with it.
	// Only relays that sent a digest are ever sent the field.
	SubjectsRequired bool `json:"subjects_required,omitempty"`
	// JWKS is the mint trust document (keys plus withdrawn_key_ids). The relay replaces its trusted grant
	// keys with it, so a rotation or withdrawal applies within one observation.
	JWKS json.RawMessage `json:"jwks,omitempty"`
}
type relayPoolHandover struct {
	AccountID string `json:"account_id"`
	CloseAt   int64  `json:"close_at"`
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
		out = relayNodeLeaseDocument{NodeID: in.NodeID, Generation: gen, ProcessEpoch: epoch, ExpiresAt: min(expires.Unix(), s.clock().Add(relayNodeLease).Unix()), CapacityLimit: capacity, Revocations: []relayRevocation{}}
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
		out = relayNodeLeaseDocument{NodeID: in.NodeID, Generation: gen + 1, ProcessEpoch: in.ProcessEpoch, ExpiresAt: now.Add(relayNodeLease).Unix(), CapacityLimit: capacity, Revocations: []relayRevocation{}}
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
	in.Metrics = usableMetrics(ctx, in.NodeID, in.Metrics)
	seen := map[string]bool{}
	for _, subject := range in.Subjects {
		k := subject.AccountID + "\x00" + subject.EndpointID
		// A digest relay sends membership only; a relay without a digest sends each grant generation.
		generation := subject.Generation >= 1 && subject.Generation != int64(^uint64(0)>>1)
		if in.SubjectsDigest != "" {
			generation = subject.Generation == 0
		}
		if !relayIdentifier(subject.AccountID) || !relayIdentifier(subject.EndpointID) || strings.ContainsAny(subject.AccountID+subject.EndpointID, "\x00\n") || !generation || seen[k] {
			return out, invalidUsage("relay_nodes.go:145")
		}
		seen[k] = true
	}
	now := s.clock().UTC()
	var required bool
	var set relaySubjectSet
	// The repeatable-read SELECT ... FOR UPDATE fails with 40001 when the relay's row changed after the
	// snapshot (a relay restart's StartRelayNode, a second server during a rollout); replay it like InTx does.
	err := db.RetrySerializable(ctx, func() (err error) {
		out, set, required, err = s.observeRelayNodeTx(ctx, in, now)
		return err
	})
	if err != nil {
		return relayNodeLeaseDocument{}, err
	}
	s.countRelayObservation(in, required)
	s.recordNodeMetrics(ctx, "relay", in.NodeID, in.ProcessEpoch, in.CapacityUsed, in.Metrics)
	if err := s.relayFairUse(ctx, in.NodeID, set, now, &out); err != nil {
		return relayNodeLeaseDocument{}, err
	}
	out.PoolHandover = s.relayPoolHandovers(ctx, in.NodeID)
	return out, nil
}

// relayFairUse puts the speeds in a Paperboat-operated relay's lease (read after the commit and outside the transaction: cached,
// bounded, fail-safe). An account's speed is the fast speed, or its plan's slower speed once any workspace it belongs to used its
// pool (edge and relay traffic together) this month, and it is ONE bucket across relays: the connected account's speed is divided by the number of relays it is on, so
// the shares add up to the account's speed (the total across relays never exceeds it; an idle relay's share is not handed to the busy
// one until the account is seen on one relay only, within the cache interval below). A customer-owned relay gets none, so it is never
// limited.
// Prior art: the bucket per client is Tailscale's DERP receive limiter (derp/derpserver/derpserver.go rateLimit, x/time/rate); sharing
// one limit among several servers by giving each its share of the limit is the usual static partition (Envoy's local rate limit is
// configured per replica the same way); a central bucket would put the control plane on every packet's path.
func (s *EdgeService) relayFairUse(ctx context.Context, nodeID string, set relaySubjectSet, now time.Time, out *relayNodeLeaseDocument) error {
	var customerOwned bool
	if err := s.store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.selfhost_installations WHERE node_id=$1 AND scope_kind IN ('account','team'))`, nodeID).Scan(&customerOwned); err != nil || customerOwned {
		return err
	}
	out.AccountMbit = s.fairUse.RelayMbit
	accounts := make([]string, 0, len(set.list))
	listed := map[string]bool{}
	for _, subject := range set.list {
		if !listed[subject.AccountID] {
			listed[subject.AccountID] = true
			accounts = append(accounts, subject.AccountID)
		}
	}
	var slow []string
	if s.relayPool != nil {
		slow = s.relayPool.DeniedAmong(ctx, accounts, now, relayPoolFreshLookups)
	}
	paid := map[string]bool{}
	if len(slow) > 0 {
		var err error
		if paid, err = bandwidthgate.PaidAccounts(ctx, s.store, slow); err != nil {
			slow = nil // fail-safe: a failed plan lookup leaves the accounts at the fast speed this round
		}
	}
	isSlow := make(map[string]bool, len(slow))
	for _, account := range slow {
		isSlow[account] = true
	}
	relays := s.relaysPerAccount(ctx, now)
	for _, account := range accounts {
		mbit := s.fairUse.RelayMbit
		if isSlow[account] {
			// the plan's slow speed, the same one the account's edge traffic runs at (one pool, one slow speed)
			mbit = float64(s.fairUse.EdgeFreeBytesPerSecond) * 8 / 1e6
			if paid[account] {
				mbit = float64(s.fairUse.EdgeBytesPerSecond) * 8 / 1e6
			}
		}
		count := max(relays[account], 1)
		if mbit != s.fairUse.RelayMbit || count > 1 {
			out.AccountSpeeds = append(out.AccountSpeeds, relayAccountSpeed{Account: account, Mbit: max(mbit/float64(count), minRelayAccountMbit)})
		}
	}
	return nil
}

// relayPoolHandovers reads the accounts handed over off a node (selfhost.handOverRelays writes them) and drops the
// ones every grant minted before the handover has expired for: the old relay then needs no refusal. Nil on a
// read failure, so the relay keeps its list.
func (s *EdgeService) relayPoolHandovers(ctx context.Context, node string) *[]relayPoolHandover {
	rows, err := s.store.SQL().QueryContext(ctx, `WITH expired AS (DELETE FROM paperboat.relay_pool_handovers WHERE node_id=$1 AND close_at<now()-make_interval(secs=>$2::float8))
SELECT account_id,floor(extract(epoch from close_at))::bigint FROM paperboat.relay_pool_handovers WHERE node_id=$1 AND close_at>=now()-make_interval(secs=>$2::float8) ORDER BY account_id LIMIT `+strconv.Itoa(relaylimits.MaxConnectionsPerNode), node, (relaylimits.MaxGrantLifetime + peernetwork.MaxClockSkew).Seconds())
	if err != nil {
		slog.WarnContext(ctx, "relay_pool_handover_read_failed", "node_id", node, "error", err)
		return nil
	}
	defer rows.Close()
	list := []relayPoolHandover{}
	for rows.Next() {
		var h relayPoolHandover
		if err = rows.Scan(&h.AccountID, &h.CloseAt); err != nil {
			slog.WarnContext(ctx, "relay_pool_handover_read_failed", "node_id", node, "error", err)
			return nil
		}
		list = append(list, h)
	}
	if rows.Err() != nil {
		return nil
	}
	return &list
}

// minRelayAccountMbit keeps one relay's share of an account able to carry its control traffic.
const minRelayAccountMbit = 0.064

// relayCountTTL is how long the per-account relay counts are reused: the accounts' shares follow a change in the relays that carry them
// within this and a relay observation interval.
const relayCountTTL = 10 * time.Second

// relaysPerAccount counts, for each account on more than one live Paperboat-operated relay, how many relays carry it (from the
// subject sets the relays report), cached for relayCountTTL. A failed read answers none: every account keeps its full per-relay speed
// until the next read.
func (s *EdgeService) relaysPerAccount(ctx context.Context, now time.Time) map[string]int {
	s.relayCountsMu.Lock()
	defer s.relayCountsMu.Unlock()
	if s.relayCounts != nil && now.Sub(s.relayCountsAt) < relayCountTTL {
		return s.relayCounts
	}
	counts := map[string]int{}
	rows, err := s.store.SQL().QueryContext(ctx, `SELECT e->>'account_id', count(DISTINCT s.node_id)
FROM paperboat.relay_subject_sets s
JOIN paperboat.control_tunnel_nodes n ON n.id=s.node_id AND n.process_epoch=s.process_epoch AND n.node_generation=s.node_generation AND n.last_heartbeat_at > $1
 AND NOT EXISTS(SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=s.node_id AND i.scope_kind IN ('account','team'))
CROSS JOIN LATERAL jsonb_array_elements(s.subjects) e
GROUP BY 1 HAVING count(DISTINCT s.node_id) > 1`, now.Add(-relayNodeLease))
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var account string
			var n int
			if err = rows.Scan(&account, &n); err != nil {
				break
			}
			counts[account] = n
		}
		if err == nil {
			err = rows.Err()
		}
	}
	if err != nil {
		counts = map[string]int{}
	}
	s.relayCounts, s.relayCountsAt = counts, now
	return counts
}

// relayPoolFreshLookups bounds the uncached usage reads one relay observation may cost.
const relayPoolFreshLookups = 64

// observeRelayNodeTx is one attempt of ObserveRelayNode's transaction; RetrySerializable may replay it.
func (s *EdgeService) observeRelayNodeTx(ctx context.Context, in relayNodeObservation, now time.Time) (out relayNodeLeaseDocument, set relaySubjectSet, required bool, err error) {
	tx, err := s.store.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return out, set, required, err
	}
	defer tx.Rollback()
	var capacity int64
	var expires time.Time
	var state string
	err = tx.QueryRowContext(ctx, `SELECT capacity_limit,registry_expires_at,state FROM paperboat.control_tunnel_nodes WHERE id=$1 AND node_generation=$2 AND process_epoch=$3 AND roles @> ARRAY['relay']::text[] FOR UPDATE`, in.NodeID, in.Generation, in.ProcessEpoch).Scan(&capacity, &expires, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return out, set, required, invalidUsage("relay_nodes.go:160")
	}
	if err != nil {
		return out, set, required, err
	}
	// A heartbeat late past its lease, even after the reconciler marked the node offline as stale, comes from the
	// process that owns this generation and epoch (the update below matches both): it renews the lease. Refusing it
	// fenced the relay, which restarted under a new epoch and invalidated every grant whenever the control plane was
	// slow (F21-01). A process that announced its own shutdown (offline within its lease) stays shut down.
	if state == "retired" || (state == "offline" && !in.Draining && expires.After(now)) || in.CapacityUsed > capacity {
		return out, set, required, invalidUsage("relay_nodes.go:166")
	}
	if state == "draining" && !in.Draining {
		return out, set, required, invalidUsage("relay_nodes.go:169")
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
		return out, set, required, err
	}
	out = relayNodeLeaseDocument{NodeID: in.NodeID, Generation: in.Generation, ProcessEpoch: in.ProcessEpoch, ExpiresAt: now.Add(relayNodeLease).Unix(), CapacityLimit: capacity, Revocations: []relayRevocation{}}
	set, required, err = relayObservedSubjects(ctx, tx, in)
	if err != nil {
		return relayNodeLeaseDocument{}, set, required, err
	}
	out.SubjectsRequired = required
	if !required {
		if in.SubjectsDigest != "" {
			out.Revocations, err = relayMinGenerations(ctx, tx, in.NodeID, set)
		} else {
			out.Revocations, err = relayRevocationsByGeneration(ctx, tx, in.NodeID, set)
		}
		if err != nil {
			return relayNodeLeaseDocument{}, set, required, err
		}
	}
	out.PeerRelay, err = relayServiceForNode(ctx, tx, in.NodeID)
	if err != nil {
		return relayNodeLeaseDocument{}, set, required, err
	}
	return out, set, required, tx.Commit()
}

// countRelayObservation records how an observation carried its subjects. Metrics are best-effort: an observation
// never fails on them.
func (s *EdgeService) countRelayObservation(in relayNodeObservation, required bool) {
	if s.relayMetrics == nil {
		return
	}
	shape := "full"
	if in.SubjectsDigest != "" && in.Subjects == nil {
		shape = "digest_only"
		if required {
			shape = "required"
		}
	}
	_ = s.relayMetrics.IncCounter(telemetry.MetricRelaySubjectObservations, sharedmetrics.Labels{"shape": shape})
}

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
	// A starting relay gets the current trust document, so it never serves on a stale startup file.
	if out.JWKS, err = s.TrustKeys(r.Context()); err != nil {
		writeEdgeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", true, 1000, err)
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
	if out.JWKS, err = s.TrustKeys(r.Context()); err != nil {
		writeEdgeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", true, 1000, err)
		return
	}
	writeEdgeJSON(w, http.StatusOK, out)
}

// relayNodeMayStart admits a relay process start for a node that is not retired or draining and whose
// registered capacity is within the ceiling the relay itself enforces (shared/relaylimits).
func relayNodeMayStart(state string, capacity int64) bool {
	return state != "retired" && state != "draining" && capacity > 0 && capacity <= relaylimits.MaxConnectionsPerNode
}
