package controlplane

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	sharedmetrics "github.com/pinksaucepasta/paperboat/shared/metrics"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
	"github.com/pinksaucepasta/paperboat/shared/nodemetrics"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"github.com/pinksaucepasta/paperboat/shared/relayshare"
	"log/slog"
	"math"
	"net/http"
	"slices"
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
	ServingSPKISHA256  string `json:"serving_spki_sha256,omitempty"`
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
	// AccountDemand is what each connected account offered this relay over the observation interval and how much of it the relay's speed
	// limit held back (bytes per second): the control plane divides the account's speed among its relays by it (relayshare.Shares).
	AccountDemand map[string]relayshare.Demand `json:"account_demand,omitempty"`
	// Mesh is the relay's relay-to-relay forwarding counters; optional, stored with the node's latest observation.
	Mesh *nodemesh.Stats `json:"mesh,omitempty"`
	// ServingSPKISHA256 is the "sha256:" + hex pin of the relay's serving certificate; optional, kept in the node's stored observation.
	ServingSPKISHA256 string `json:"serving_spki_sha256,omitempty"`
}

// relayAccountSpeed is one account a relay holds to less than the default speed.
type relayAccountSpeed struct {
	Account string  `json:"account"`
	Mbit    float64 `json:"mbit"`
}

type relayNodeLeaseDocument struct {
	NodeID       string                `json:"node_id"`
	Generation   int64                 `json:"node_generation"`
	ProcessEpoch string                `json:"process_epoch"`
	ExpiresAt    int64                 `json:"expires_at"`
	Revocations  []relayRevocation     `json:"revocations"`
	PeerRelay    *relayServiceIdentity `json:"peer_relay,omitempty"`
	// MeshPeers are the other managed relay nodes this one may link to (pin = their serving certificate's SPKI pin); none for
	// a customer-owned relay, which never meshes with Paperboat's.
	MeshPeers []nodemesh.Peer `json:"mesh_peers,omitempty"`
	// PoolHandover lists accounts whose relay pool no longer includes this node and when the relay closes their
	// carriers (Unix seconds): the old relay serves them until clients have moved to the new ones. Nil leaves the
	// relay's list as it is (the read failed); an empty list clears it.
	PoolHandover *[]relayPoolHandover `json:"pool_handover,omitempty"`
	// AccountMbit is the speed of one account on this relay in Mbit/s, and AccountSpeeds the connected accounts held to a different
	// one: those past their plan's included relay bytes this month (the plan's slower speed), paid ones (the paid speed) and those on
	// more than one relay (this relay's share of their speed by demand, relayshare.Shares), until a later observation no longer lists them.
	// An account's speed is one bucket across all relays: the shares of its relays add up to its speed. All empty on a customer-owned
	// relay: Paperboat never limits it. They never carry authority.
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

// relayServingPin is the canonical form of the serving certificate pin a relay reports ("" when it reports none); ok is false
// for a malformed one.
func relayServingPin(pin string) (canonical string, ok bool) {
	if pin == "" {
		return "", true
	}
	raw, err := nodemesh.ParsePin(pin)
	if err != nil {
		return "", false
	}
	return nodemesh.PinFromDigest(raw), true
}

// relayStoredObservation is the fragment a relay report merges into the node's stored observation: the mesh counters (nil
// leaves none) and the serving pin (empty leaves the stored pin as it is).
func relayStoredObservation(mesh *nodemesh.Stats, servingPin string) ([]byte, error) {
	fragment := map[string]any{}
	if mesh != nil {
		fragment["mesh"] = mesh
	}
	if servingPin != "" {
		fragment["serving_spki_sha256"] = servingPin
	}
	return json.Marshal(fragment)
}

// StartRelayNode activates an existing operator-authorized registry entry. This
// API cannot create nodes or change region, account policy, endpoints or keys.
func (s *EdgeService) StartRelayNode(ctx context.Context, in relayNodeStart) (relayNodeLeaseDocument, error) {
	var out relayNodeLeaseDocument
	if !relayIdentifier(in.NodeID) || !relayIdentifier(in.ProcessEpoch) || in.ExpectedGeneration < 1 || in.ExpectedGeneration >= int64(^uint64(0)>>1)-1 {
		return out, invalidUsage("relay_nodes.go:79")
	}
	servingPin, ok := relayServingPin(in.ServingSPKISHA256)
	if !ok {
		return out, invalidUsage("relay_nodes.go:serving_spki_sha256")
	}
	fragment, err := relayStoredObservation(nil, servingPin)
	if err != nil {
		return out, err
	}
	tx, err := s.store.SQL().BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var epoch, state string
	var gen int64
	var leaseExpires sql.NullTime
	// A node with any relay transport may start (overlap, not containment): a relay that serves fewer
	// transports than a later one still starts.
	err = tx.QueryRowContext(ctx, `SELECT n.node_generation,n.process_epoch,n.state,n.registry_expires_at FROM paperboat.control_tunnel_nodes n LEFT JOIN paperboat.managed_node_authority m ON m.node_id=n.id LEFT JOIN paperboat.operator_regions r ON r.id=m.region_id WHERE n.id=$1 AND n.roles @> ARRAY['relay']::text[] AND n.transports && ARRAY['derp_quic','derp_wss']::text[] AND n.region IS NOT NULL AND (m.node_id IS NULL OR (m.retired_at IS NULL AND r.enabled)) FOR UPDATE OF n`, in.NodeID).Scan(&gen, &epoch, &state, &leaseExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return out, invalidUsage("relay_nodes.go:90")
	}
	if err != nil {
		return out, err
	}
	if !relayNodeMayStart(state) {
		return out, invalidUsage("relay_nodes.go:96")
	}
	if epoch == in.ProcessEpoch && gen == in.ExpectedGeneration+1 {
		// A live startup retry preserves the node's existing readiness.
		expires := leaseExpires.Time
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
		out = relayNodeLeaseDocument{NodeID: in.NodeID, Generation: gen, ProcessEpoch: epoch, ExpiresAt: min(expires.Unix(), s.clock().Add(relayNodeLease).Unix()), Revocations: []relayRevocation{}}
	} else {
		now := s.clock().UTC()
		// A process whose remembered generation differs (its state file was lost or restored) adopts the
		// node's generation when no live process holds the lease; while one does, it stays fenced. A drained
		// relay keeps its lease, so adoption waits for it to lapse (relayRegistryLease), never steals it.
		if epoch == in.ProcessEpoch || gen != in.ExpectedGeneration && leaseExpires.Valid && leaseExpires.Time.After(now) {
			return out, invalidUsage("relay_nodes.go:119")
		}
		gen = max(gen, in.ExpectedGeneration)
		expires := now.Add(relayRegistryLease)
		_, err = tx.ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET node_generation=$5,process_epoch=$2,state='registered',ready=false,capacity_used=0,capacity_observed_at=$3,last_heartbeat_at=$3,registry_expires_at=$4,drain_deadline=NULL,updated_at=$3,version=version+1,observation=coalesce(observation,'{}'::jsonb)||$6::jsonb WHERE id=$1`, in.NodeID, in.ProcessEpoch, now, expires, gen+1, fragment)
		if err != nil {
			return out, err
		}
		out = relayNodeLeaseDocument{NodeID: in.NodeID, Generation: gen + 1, ProcessEpoch: in.ProcessEpoch, ExpiresAt: now.Add(relayNodeLease).Unix(), Revocations: []relayRevocation{}}
	}
	out.PeerRelay, err = relayServiceForNode(ctx, tx, in.NodeID)
	if err != nil {
		return relayNodeLeaseDocument{}, err
	}
	if out.MeshPeers, err = relayMeshPeers(ctx, tx, in.NodeID, s.clock().UTC()); err != nil {
		return relayNodeLeaseDocument{}, err
	}
	return out, tx.Commit()
}

func (s *EdgeService) ObserveRelayNode(ctx context.Context, in relayNodeObservation) (relayNodeLeaseDocument, error) {
	var out relayNodeLeaseDocument
	if !relayIdentifier(in.NodeID) || !relayIdentifier(in.ProcessEpoch) || in.Generation < 1 || in.CapacityUsed < 0 || (in.SubjectsDigest != "" && !validRelaySubjectsDigest(in.SubjectsDigest)) {
		return out, invalidUsage("relay_nodes.go:139")
	}
	servingPin, ok := relayServingPin(in.ServingSPKISHA256)
	if !ok {
		return out, invalidUsage("relay_nodes.go:serving_spki_sha256")
	}
	in.ServingSPKISHA256 = servingPin
	in.Metrics = usableMetrics(ctx, in.NodeID, in.Metrics)
	if in.Mesh != nil && (in.Mesh.Links < 0 || in.Mesh.Names < 0) {
		return out, invalidUsage("relay_nodes.go:mesh")
	}
	for account, d := range in.AccountDemand {
		if !relayIdentifier(account) || !validRate(d.Offered) || !validRate(d.Held) {
			return out, invalidUsage("relay_nodes.go:demand")
		}
	}
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
	if err := s.relayFairUse(ctx, in.NodeID, in.AccountDemand, set, now, &out); err != nil {
		return relayNodeLeaseDocument{}, err
	}
	out.PoolHandover = s.relayPoolHandovers(ctx, in.NodeID)
	return out, nil
}

// relayFairUse puts the speeds in a Paperboat-operated relay's lease (read after the commit and outside the transaction: cached,
// bounded, fail-safe). An account's speed is its plan's fast speed (Free 10, paid 100 Mbit/s), or its plan's slower speed once any
// workspace it belongs to used its pool (edge and relay traffic together) this month, and it is ONE bucket across relays: an account
// on more than one live relay has its speed divided among them by the demand they report (relayshare.Shares), so one busy relay gets nearly
// all of it and the shares never add up to more than the speed. A customer-owned relay gets none, so it is never limited.
// Prior art: the bucket per client is Tailscale's DERP receive limiter (derp/derpserver/derpserver.go rateLimit, x/time/rate); the
// split among them is max-min fair as in Doorman's FairShare (github.com/youtube/doorman); a central bucket would put the control plane on every
// packet's path.
func (s *EdgeService) relayFairUse(ctx context.Context, nodeID string, demand map[string]relayshare.Demand, set relaySubjectSet, now time.Time, out *relayNodeLeaseDocument) error {
	if customerOwned, err := s.customerOwnedNode(ctx, nodeID); err != nil || customerOwned {
		return err // a customer-owned relay is never limited, and its accounts' demand is not Paperboat's to divide
	}
	out.AccountMbit = s.fairUse.RelayMbitFree // an account the plan lookup did not list: Free's speed
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
	// One plan lookup for every listed account: paid ones run at the paid speed, or the paid slow speed once their pool is used up.
	paid, err := bandwidthgate.PaidAccounts(ctx, s.store, accounts)
	if err != nil {
		paid, slow = nil, nil // fail-safe: a failed plan lookup leaves the accounts at the Free speed this round
	}
	isSlow := make(map[string]bool, len(slow))
	for _, account := range slow {
		isSlow[account] = true
	}
	plan := make(map[string]float64, len(accounts))
	for _, account := range accounts {
		mbit := s.fairUse.RelayMbitFree
		if paid[account] {
			mbit = s.fairUse.RelayMbitPaid
		}
		if isSlow[account] {
			// the plan's slow speed, the same one the account's edge traffic runs at (one pool, one slow speed)
			mbit = float64(s.fairUse.EdgeFreeBytesPerSecond) * 8 / 1e6
			if paid[account] {
				mbit = float64(s.fairUse.EdgeBytesPerSecond) * 8 / 1e6
			}
		}
		plan[account] = mbit
	}
	// An account more than one relay carries gets this relay's share of its speed (a new connection here starts at the start
	// allowance, not at the full speed); one carried here alone is listed when its plan speed is not the default.
	shares := s.relayDemand.divide(nodeID, demand, plan, now)
	for _, account := range accounts {
		mbit := plan[account]
		if share, shared := shares[account]; shared {
			mbit = share
		} else if mbit == s.fairUse.RelayMbitFree {
			continue
		}
		out.AccountSpeeds = append(out.AccountSpeeds, relayAccountSpeed{Account: account, Mbit: mbit})
	}
	return nil
}

// relayPoolHandovers reads the accounts handed over off a node (selfhost.handOverRelays writes them) and drops the
// ones every grant minted before the handover has expired for: the old relay then needs no refusal. Nil on a
// read failure, so the relay keeps its list.
func (s *EdgeService) relayPoolHandovers(ctx context.Context, node string) *[]relayPoolHandover {
	rows, err := s.store.SQL().QueryContext(ctx, `WITH expired AS (DELETE FROM paperboat.relay_pool_handovers WHERE node_id=$1 AND close_at<now()-make_interval(secs=>$2::float8))
SELECT account_id,floor(extract(epoch from close_at))::bigint FROM paperboat.relay_pool_handovers WHERE node_id=$1 AND close_at>=now()-make_interval(secs=>$2::float8) ORDER BY account_id`, node, (relaylimits.MaxGrantLifetime + peernetwork.MaxClockSkew).Seconds())
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

// relayPoolFreshLookups bounds the uncached usage reads one relay observation may cost.
const relayPoolFreshLookups = 64

// observeRelayNodeTx is one attempt of ObserveRelayNode's transaction; RetrySerializable may replay it.
func (s *EdgeService) observeRelayNodeTx(ctx context.Context, in relayNodeObservation, now time.Time) (out relayNodeLeaseDocument, set relaySubjectSet, required bool, err error) {
	tx, err := s.store.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return out, set, required, err
	}
	defer tx.Rollback()
	var expires time.Time
	var state string
	err = tx.QueryRowContext(ctx, `SELECT registry_expires_at,state FROM paperboat.control_tunnel_nodes WHERE id=$1 AND node_generation=$2 AND process_epoch=$3 AND roles @> ARRAY['relay']::text[] FOR UPDATE`, in.NodeID, in.Generation, in.ProcessEpoch).Scan(&expires, &state)
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
	if state == "retired" || (state == "offline" && !in.Draining && expires.After(now)) {
		return out, set, required, invalidUsage("relay_nodes.go:166")
	}
	if state == "draining" && !in.Draining {
		return out, set, required, invalidUsage("relay_nodes.go:169")
	}
	// The mesh counters ride in the node's stored observation (operatorinfra.Node.Forward reads them); a relay that sends none clears them.
	// The serving pin stays unless the relay reports another: the update below only replaces the mesh counters.
	observation, err := relayStoredObservation(in.Mesh, in.ServingSPKISHA256)
	if err != nil {
		return out, set, required, err
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
	_, err = tx.ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET state=$4,ready=$5,capacity_used=$6,capacity_observed_at=$7,last_heartbeat_at=$7,registry_expires_at=$8,drain_deadline=CASE WHEN $4='draining' THEN coalesce(drain_deadline,$7) ELSE NULL END,observation=(coalesce(observation,'{}'::jsonb)-'mesh')||$9::jsonb,updated_at=$7 WHERE id=$1 AND node_generation=$2 AND process_epoch=$3`, in.NodeID, in.Generation, in.ProcessEpoch, state, in.Ready && !in.Draining, in.CapacityUsed, now, expires, observation)
	if err != nil {
		return out, set, required, err
	}
	out = relayNodeLeaseDocument{NodeID: in.NodeID, Generation: in.Generation, ProcessEpoch: in.ProcessEpoch, ExpiresAt: now.Add(relayNodeLease).Unix(), Revocations: []relayRevocation{}}
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
	if out.MeshPeers, err = relayMeshPeers(ctx, tx, in.NodeID, now); err != nil {
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
	writeEdgeSnapshot(w, r, out)
}

// maxRelayObservationDocument fits a relay's full subject list, whatever its length (identifiers at their 256-byte limit),
// which the general edge document limit does not.
const maxRelayObservationDocument = connectorprotocol.MaximumSnapshotBytes

func (s *EdgeService) handleRelayObserve(w http.ResponseWriter, r *http.Request) {
	var in relayNodeObservation
	if !s.decodeLimit(w, r, &in, maxRelayObservationDocument) {
		return
	}
	if node, scoped := r.Context().Value(scopedRelayObservationNodeKey{}).(string); scoped && node != in.NodeID {
		writeEdgeError(w, r, http.StatusUnauthorized, "unauthenticated", false, 0)
		return
	}
	if s.refuseBelowFloor(w, r, "relay", in.Metrics) {
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
	writeEdgeSnapshot(w, r, out)
}

// relayNodeMayStart admits a relay process start for a node that is not retired or draining.
func relayNodeMayStart(state string) bool {
	return state != "retired" && state != "draining"
}

// customerOwnedNode says the node is a customer's own self-hosted edge or relay (an account or team installation). Paperboat's
// own (global) installations are hosted nodes. Every limit Paperboat sets for hosted traffic (fair-use speeds, terminal caps, the
// Free request rates) is withheld from a customer-owned node: it enforces only what the server tells it.
func (s *EdgeService) customerOwnedNode(ctx context.Context, nodeID string) (bool, error) {
	var owned bool
	err := s.store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.selfhost_installations WHERE node_id=$1 AND scope_kind IN ('account','team'))`, nodeID).Scan(&owned)
	return owned, err
}

// relayMeshPeers are the managed relay nodes other than node that it may link to: heartbeating, ready or draining, with a
// serving certificate pin and a QUIC endpoint. Nothing for a node that is not a managed one (a customer-owned relay), and a
// managed relay never lists a customer-owned one: the populations are never mixed.
func relayMeshPeers(ctx context.Context, tx *sql.Tx, node string, now time.Time) ([]nodemesh.Peer, error) {
	rows, err := tx.QueryContext(ctx, `SELECT n.id, coalesce(n.region,m.region_id), n.endpoint_host, n.endpoint_quic_port, n.observation->>'serving_spki_sha256', n.process_epoch, n.drain_deadline IS NOT NULL
FROM paperboat.control_tunnel_nodes n
JOIN paperboat.managed_node_authority m ON m.node_id=n.id AND m.retired_at IS NULL
JOIN paperboat.operator_regions region ON region.id=m.region_id AND region.enabled
WHERE n.roles @> ARRAY['relay']::text[] AND n.transports @> ARRAY['derp_quic']::text[] AND n.state IN ('ready','draining') AND n.id<>$1
 AND n.last_heartbeat_at>$2::timestamptz-make_interval(secs=>$3) AND n.observation->>'serving_spki_sha256' ~ '^sha256:[0-9a-f]{64}$'
 AND n.endpoint_host IS NOT NULL AND n.endpoint_quic_port IS NOT NULL
 AND EXISTS(SELECT 1 FROM paperboat.managed_node_authority self WHERE self.node_id=$1 AND self.retired_at IS NULL)
ORDER BY n.id LIMIT $4`, node, now, meshFreshness.Seconds(), nodemesh.MaxPeers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []nodemesh.Peer
	for rows.Next() {
		var p nodemesh.Peer
		var pin string
		var port int
		if err := rows.Scan(&p.NodeID, &p.Region, &p.Host, &port, &pin, &p.ProcessEpoch, &p.Draining); err != nil {
			return nil, err
		}
		p.Port, p.SPKISHA256 = uint16(port), pin
		// A row the wire cannot carry is left out, not allowed to fail the lease.
		if p.Validate() == nil {
			peers = append(peers, p)
		}
	}
	// A pin two nodes report is listed for neither: a relay refuses a directory with a repeated pin whole, so one node
	// reporting another's (public) certificate pin would otherwise freeze every relay's mesh at its last good directory.
	reported := map[string]int{}
	for _, p := range peers {
		reported[p.SPKISHA256]++
	}
	peers = slices.DeleteFunc(peers, func(p nodemesh.Peer) bool { return reported[p.SPKISHA256] > 1 })
	return peers, rows.Err()
}

// validRate is a byte rate a relay may report: finite and not negative.
func validRate(v float64) bool { return v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }
