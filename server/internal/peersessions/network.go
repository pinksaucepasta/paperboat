package peersessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/keyedlock"
	"golang.org/x/crypto/curve25519"
)

// The function keeps the expanded authorization view's plan across configuration requests.
const inspectorAdmissionsQuery = `SELECT * FROM paperboat.peer_network_inspector_admissions($1,$2,$3,$4)`

// networkConfigLimit bounds one signed configuration or regional candidate token (a relay grant has its own,
// smaller bound, peernetwork.MaxGrantBytes);
// it is the client's bound (shared/peernetwork), so a configuration the server signs is one the
// CLI accepts.
const networkConfigLimit = peernetwork.MaxConfigurationBytes

// maximumAccountEndpoints is the stated product maximum of endpoints in one account, shared
// with the CLI's trusted-key and configuration bounds.
const maximumAccountEndpoints = peernetwork.MaxEndpoints

type NetworkRegistration struct {
	OperationID                                                    string
	UserID, EndpointID, Role, MachineID                            string
	EndpointGeneration, MachineGeneration, ExpectedKeyGeneration   int64
	WireGuardPublicKey, DiscoPublicKey, QUICCertificateFingerprint []byte
}
type NetworkRegistrationResult struct {
	KeyGeneration  int64  `json:"key_generation"`
	VirtualAddress string `json:"virtual_address"`
}
type NetworkConfigRequest struct {
	OperationID, UserID, EndpointID string
	AdmittedScopes                  []AdmittedScope
	// Endpoints are the engine's own candidate endpoints (ip:port); nil when the request reports none.
	Endpoints []string
	// HomeRelay is the node id of the regional relay a machine is homed on; empty when it is homed on none, which clears
	// the stored one. Only an eligible relay of the account is stored (endpointsUpdate).
	HomeRelay string
	// Authority is AuthorityUntilRevoked when the daemon keeps verified peer state until it is revoked; empty for
	// daemons that let every scope end with the document.
	Authority string
	// BaseGeneration is the generation of the configuration the endpoint holds (0: none); when this replica sent it,
	// the answer is a delta from it.
	BaseGeneration uint64
	// CapVersion is the capability version (shared/capver) the requesting client sent; 0 when it sent none. The signed
	// document omits every field newer than it (peernetwork.Configuration.ForCapVersion).
	CapVersion int
}

// AuthorityUntilRevoked is the peer-state contract of PLAN 3b "offline-capable authority". A daemon that asks for it
// receives machine_network scopes without an expiry (expires_at 0: until a newer state removes the peer, the host
// revocation document or the peer certificate's own expiry ends it, F21-01) and, on a host, one machine_access
// "accept" scope per capability each peer's account may start offline, with expires_at 0, resource_id the host's
// machine id and resource_generation the deciding policy version (F21-11). Other daemons get the earlier shape.
const AuthorityUntilRevoked = "until_revoked_v1"

type NetworkConfigResult struct {
	// Exactly one of Configuration (the signed token), ConfigurationDelta (its changes from BaseGeneration) and
	// Unchanged (the endpoint's BaseGeneration configuration, candidates and grants stand; nothing signed) is set.
	Unchanged          bool                            `json:"unchanged,omitempty"`
	Configuration      string                          `json:"configuration,omitempty"`
	ConfigurationDelta *peernetwork.ConfigurationDelta `json:"configuration_delta,omitempty"`
	CandidateSet       string                          `json:"candidate_set"`
	RelayGrants        []string                        `json:"relay_grants"`
}

type NetworkService struct {
	endpointBuilds keyedlock.Locks
	store          *db.DB
	signer         interface {
		SignNetworkConfiguration(peernetwork.Configuration) (string, error)
		SignRegionalCandidates(peernetwork.RegionalCandidates) (string, error)
		SignRelayGrant(peernetwork.RelayGrant) (string, error)
	}
	issuer string
	now    func() time.Time
	// The in-memory policy documents (policy_cache.go) and their LISTEN session; relay nodes per account (relay_nodes.go).
	cache        *policyCache
	policyListen sync.Mutex
	policyConn   *pgx.Conn
	nodes        relayNodeCache
	// policySync, when set (tests), runs before a cached document is read: it waits until the listener has seen
	// every notification committed so far, so a test reads its own writes.
	policySync func(context.Context) error
	// What each endpoint was last sent, for delta answers (sent_configurations.go).
	sent sentConfigurations
	// Each account's mesh as of its latest mesh_seq read here (mesh_cache.go).
	mesh meshCache
}

func NewNetworkService(store *db.DB, signer interface {
	SignNetworkConfiguration(peernetwork.Configuration) (string, error)
	SignRegionalCandidates(peernetwork.RegionalCandidates) (string, error)
	SignRelayGrant(peernetwork.RelayGrant) (string, error)
}, issuer string) (*NetworkService, error) {
	if store == nil || signer == nil || issuer == "" {
		return nil, ErrInvalid
	}
	return &NetworkService{store: store, signer: signer, issuer: issuer, now: time.Now, cache: newPolicyCache()}, nil
}

// Register records a peer endpoint registration. Its serializable transaction is replayed after a
// serialization failure, so a concurrent write (or two servers during a rollout) is not a 503.
func (s *NetworkService) Register(ctx context.Context, in NetworkRegistration) (NetworkRegistrationResult, error) {
	var out NetworkRegistrationResult
	err := db.RetrySerializable(ctx, func() (e error) {
		out, e = s.registerAttempt(ctx, in)
		return e
	})
	return out, err
}

func (s *NetworkService) registerAttempt(ctx context.Context, in NetworkRegistration) (NetworkRegistrationResult, error) {
	if ctx == nil || !bounded(in.OperationID, 8, 128) || !bounded(in.UserID, 1, 256) || !bounded(in.EndpointID, 1, 128) || !validWireGuardPublicKey(in.WireGuardPublicKey) || !validWireGuardPublicKey(in.DiscoPublicKey) || len(in.QUICCertificateFingerprint) != 32 || in.ExpectedKeyGeneration < 0 || (in.Role != peernetwork.RoleCLI && in.Role != peernetwork.RoleMachine) {
		return NetworkRegistrationResult{}, ErrInvalid
	}
	if in.Role == peernetwork.RoleCLI && (in.MachineID != "" || in.MachineGeneration != 0) || in.Role == peernetwork.RoleMachine && (in.MachineID != in.EndpointID || in.MachineGeneration < 1) {
		return NetworkRegistrationResult{}, ErrInvalid
	}
	hash := sha256.Sum256([]byte(in.UserID + "\x00" + in.EndpointID + "\x00" + in.Role + "\x00" + fmt.Sprint(in.EndpointGeneration) + "\x00" + fmt.Sprint(in.MachineGeneration) + "\x00" + fmt.Sprint(in.ExpectedKeyGeneration) + "\x00" + base64.RawURLEncoding.EncodeToString(in.WireGuardPublicKey) + "\x00" + base64.RawURLEncoding.EncodeToString(in.DiscoPublicKey) + "\x00" + hex.EncodeToString(in.QUICCertificateFingerprint)))
	tx, err := db.BeginSerializable(ctx, s.store.SQL())
	if err != nil {
		return NetworkRegistrationResult{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	var certRole, certEndpoint string
	var certGeneration int64
	err = tx.QueryRowContext(ctx, `SELECT c.role,c.endpoint_id,c.generation FROM paperboat.peer_endpoint_certificates c WHERE c.user_id=$1 AND c.fingerprint=$2 AND c.revoked_at IS NULL AND c.issued_at<=$3 AND c.expires_at>$3`, in.UserID, in.QUICCertificateFingerprint, now).Scan(&certRole, &certEndpoint, &certGeneration)
	if err == sql.ErrNoRows || err == nil && (certRole != in.Role || certEndpoint != in.EndpointID || certGeneration != in.EndpointGeneration) {
		return NetworkRegistrationResult{}, ErrDenied
	}
	if err != nil {
		return NetworkRegistrationResult{}, err
	}
	if in.Role == peernetwork.RoleCLI {
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.cli_client_sessions c JOIN paperboat.users u ON u.id=c.user_id AND u.status='active' WHERE c.id=$1 AND c.user_id=$2 AND c.state='active' AND c.revoked_at IS NULL)`, in.EndpointID, in.UserID).Scan(&active)
		if err != nil {
			return NetworkRegistrationResult{}, err
		}
		if !active {
			return NetworkRegistrationResult{}, ErrDenied
		}
	} else {
		var generation int64
		err = tx.QueryRowContext(ctx, `SELECT m.installation_generation FROM paperboat.user_machines m JOIN paperboat.users u ON u.id=m.user_id AND u.status='active' JOIN paperboat.control_environments e ON e.id=m.environment_id AND e.owner_user_id=m.user_id AND e.desired_state='active' AND e.revoked_at IS NULL WHERE m.id=$1 AND m.user_id=$2 AND m.state IN ('online','offline') AND m.seat_state='occupied' AND m.revoked_at IS NULL AND m.deleted_at IS NULL`, in.EndpointID, in.UserID).Scan(&generation)
		if err == sql.ErrNoRows || err == nil && generation != in.MachineGeneration {
			return NetworkRegistrationResult{}, ErrDenied
		}
		if err != nil {
			return NetworkRegistrationResult{}, err
		}
	}
	var oldHash []byte
	var replayGen int64
	err = tx.QueryRowContext(ctx, `SELECT request_hash,key_generation FROM paperboat.peer_network_registration_operations WHERE operation_id=$1`, in.OperationID).Scan(&oldHash, &replayGen)
	if err == nil {
		if !equalBytes(oldHash, hash[:]) {
			return NetworkRegistrationResult{}, ErrConflict
		}
		var address string
		if err = tx.QueryRowContext(ctx, `SELECT host(virtual_address) FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2 AND revoked_at IS NULL`, in.UserID, in.EndpointID).Scan(&address); err != nil {
			return NetworkRegistrationResult{}, unavailableWithCause(err)
		}
		return NetworkRegistrationResult{replayGen, address}, nil
	}
	if err != sql.ErrNoRows {
		return NetworkRegistrationResult{}, err
	}
	var currentGen int64
	var address string
	var currentWireGuard, currentDisco, currentCertificate []byte
	created := false
	err = tx.QueryRowContext(ctx, `SELECT key_generation,host(virtual_address),wireguard_public_key,disco_public_key,quic_certificate_fingerprint FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2 AND role=$3 AND revoked_at IS NULL FOR UPDATE`, in.UserID, in.EndpointID, in.Role).Scan(&currentGen, &address, &currentWireGuard, &currentDisco, &currentCertificate)
	if err == sql.ErrNoRows {
		if in.ExpectedKeyGeneration != 0 {
			return NetworkRegistrationResult{}, ErrConflict
		}
		created = true
		err = tx.QueryRowContext(ctx, `INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,disco_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at) VALUES($1,$2,$3,nullif($4,''),$5,$6,1,$7,$8,$9,('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')), $10,$10) RETURNING key_generation,host(virtual_address)`, in.UserID, in.EndpointID, in.Role, in.MachineID, in.EndpointGeneration, in.MachineGeneration, in.WireGuardPublicKey, in.DiscoPublicKey, in.QUICCertificateFingerprint, now).Scan(&currentGen, &address)
	} else if err == nil {
		if currentGen != in.ExpectedKeyGeneration {
			return NetworkRegistrationResult{}, ErrConflict
		}
		if !equalBytes(currentWireGuard, in.WireGuardPublicKey) || !equalBytes(currentDisco, in.DiscoPublicKey) {
			currentGen++
		}
		_, err = tx.ExecContext(ctx, `UPDATE paperboat.peer_network_identities SET endpoint_generation=$1,machine_generation=$2,key_generation=$3,wireguard_public_key=$4,disco_public_key=$5,quic_certificate_fingerprint=$6,revoked_at=NULL,updated_at=$7 WHERE user_id=$8 AND endpoint_id=$9`, in.EndpointGeneration, in.MachineGeneration, currentGen, in.WireGuardPublicKey, in.DiscoPublicKey, in.QUICCertificateFingerprint, now, in.UserID, in.EndpointID)
	}
	if err != nil {
		return NetworkRegistrationResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.peer_network_registration_operations(operation_id,user_id,endpoint_id,request_hash,key_generation,created_at) VALUES($1,$2,$3,$4,$5,$6)`, in.OperationID, in.UserID, in.EndpointID, hash[:], currentGen, now); err != nil {
		return NetworkRegistrationResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM paperboat.peer_network_registration_operations o WHERE o.user_id=$1 AND o.endpoint_id=$2 AND o.operation_id IN (SELECT operation_id FROM paperboat.peer_network_registration_operations WHERE user_id=$1 AND endpoint_id=$2 ORDER BY key_generation DESC,created_at DESC,operation_id DESC OFFSET 64)`, in.UserID, in.EndpointID); err != nil {
		return NetworkRegistrationResult{}, err
	}
	eventType := "peer_network.key_rotated"
	if created {
		eventType = "peer_network.key_registered"
	} else if equalBytes(currentWireGuard, in.WireGuardPublicKey) && equalBytes(currentDisco, in.DiscoPublicKey) && !equalBytes(currentCertificate, in.QUICCertificateFingerprint) {
		eventType = "peer_network.certificate_rebound"
	}
	auditID := "aud_peer_network_" + hex.EncodeToString(hash[:16])
	metadata, _ := json.Marshal(map[string]any{"endpoint_id": in.EndpointID, "role": in.Role, "key_generation": currentGen, "virtual_address": address})
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,idempotency_key,metadata,created_at) VALUES($1,$2,'user',$3,'peer_network_identity',$4,$5,$6::jsonb,$7) ON CONFLICT (event_type,resource_type,resource_id,idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`, auditID, in.UserID, eventType, in.EndpointID, in.OperationID, metadata, now); err != nil {
		return NetworkRegistrationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return NetworkRegistrationResult{}, err
	}
	return NetworkRegistrationResult{currentGen, address}, nil
}

// networkBinding is an endpoint's signed identity (shared/peernetwork) with its delivered endpoints while it is
// loaded and in the mesh log (N5); on the way out they move to the peer entry (configurationClaims) and are never
// part of a signed identity.
type networkBinding struct {
	peernetwork.Binding
	Endpoints []string `json:"endpoints,omitempty"`
	// HomeRelay is the identity's reported home relay, moved to the peer entry like Endpoints.
	HomeRelay string `json:"home_relay,omitempty"`
}

// networkScope is the shared signed access scope (shared/peernetwork) the client and the relay verify.
type networkScope = peernetwork.Scope
type networkPeer struct {
	Identity networkBinding
	Scopes   []networkScope
}

func (s *NetworkService) Configuration(ctx context.Context, in NetworkConfigRequest) (NetworkConfigResult, error) {
	// Match the database owner's bounded serializable retry policy. A failed
	// attempt is fully rolled back, including its configuration generation.
	// Wake bursts send several configuration requests for one endpoint at once;
	// built concurrently they all bump the same generation row and abort each
	// other past the retry budget. One build per endpoint at a time instead.
	started := time.Now()
	unlock, err := s.endpointBuilds.Lock(ctx, in.EndpointID)
	if err != nil {
		return NetworkConfigResult{}, err
	}
	defer unlock()
	trace := configTrace{lockWait: time.Since(started)}
	defer func() {
		// A slow request names its slow step (prod .37: >100 KB configurations averaged 700 ms; which step is unknown).
		if total := time.Since(started); total >= slowConfigLog {
			slog.Info("peer network configuration slow", "total_ms", total.Milliseconds(), "lock_wait_ms", trace.lockWait.Milliseconds(),
				"generation_ms", trace.generation.Milliseconds(), "cache_hit", trace.hit, "build_ms", trace.build.Milliseconds(),
				"sign_ms", trace.sign.Milliseconds(), "attempts", trace.attempts, "peers", trace.peers)
		}
	}()
	for attempt := 1; ; attempt++ {
		trace.attempts = attempt
		result, err := s.configuration(ctx, in, &trace)
		var pg *pgconn.PgError
		if err == nil || attempt >= 10 || !errors.As(err, &pg) || pg.Code != "40001" && pg.Code != "40P01" {
			return result, err
		}
		backoff := min(5*time.Millisecond<<min(attempt-1, 5), 200*time.Millisecond)
		half := backoff / 2
		timer := time.NewTimer(half + time.Duration(time.Now().UnixNano()%int64(half+1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return NetworkConfigResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// slowConfigLog is the duration past which a configuration request logs its steps.
const slowConfigLog = 250 * time.Millisecond

// configTrace is where one configuration request spent its time.
type configTrace struct {
	lockWait, generation, build, sign time.Duration
	hit                               bool
	attempts, peers                   int
}

func (s *NetworkService) configuration(ctx context.Context, in NetworkConfigRequest, trace *configTrace) (NetworkConfigResult, error) {
	if ctx == nil || !bounded(in.OperationID, 8, 128) || !bounded(in.UserID, 1, 256) || !bounded(in.EndpointID, 1, 128) || in.Authority != "" && in.Authority != AuthorityUntilRevoked {
		return NetworkConfigResult{}, ErrInvalid
	}
	live, err := admittedScopeIDs(in.AdmittedScopes)
	if err != nil {
		return NetworkConfigResult{}, err
	}

	now := s.now().UTC()
	// The configuration is built from one read-only snapshot. Serializable builds of every
	// endpoint of an account read each other's identity rows while each bumped its own, which
	// formed dependency cycles and aborted ~30% of builds past the retry budget on a 109-endpoint
	// account. The generation is allocated by its own statement below; the per-endpoint lock in
	// Configuration keeps one endpoint's generations in build order.
	// The generation is allocated first, on its own connection, so a build never holds a
	// snapshot while waiting for a second pool connection (a burst would exhaust the pool).
	var generation int64
	// The reported endpoints ride on the same row write (endpointsUpdate).
	endpoints, err := normalizeEndpoints(in.Endpoints)
	if err != nil {
		return NetworkConfigResult{}, err
	}
	if in.HomeRelay != "" && !bounded(in.HomeRelay, 1, 128) {
		return NetworkConfigResult{}, ErrInvalid
	}
	var homeRelay any
	if in.HomeRelay != "" {
		homeRelay = in.HomeRelay
	}
	if err := s.markAdmittedSessions(ctx, in, live["machine_access"], now); err != nil {
		return NetworkConfigResult{}, err
	}
	// An endpoint that holds a base and reports no endpoints may be answered unchanged: its generation is bumped
	// only once its answer differs (one account change
	// woke N renewals that each bumped and re-signed; NetBird sends updates to affected peers only:
	// ref/netbird/management/server/peer.go:1594-1622).
	maybeUnchanged := in.BaseGeneration != 0 && in.Endpoints == nil
	start, cacheable := s.cache.begin()
	step := time.Now()
	err = s.store.SQL().QueryRowContext(ctx, endpointsUpdate, now, in.UserID, in.EndpointID, endpoints, endpointPublishInterval.Seconds(), homeRelay, !maybeUnchanged).
		Scan(&generation)
	trace.generation = time.Since(step)
	if err == sql.ErrNoRows {
		return NetworkConfigResult{}, ErrDenied
	}
	if err != nil {
		return NetworkConfigResult{}, err
	}
	key := policyDocKey(in, live)
	if s.policySync != nil {
		if err = s.policySync(ctx); err != nil {
			return NetworkConfigResult{}, err
		}
	}
	doc := s.cache.get(in.EndpointID, key, now)
	trace.hit = doc != nil
	if doc == nil {
		step = time.Now()
		if doc, err = s.buildPolicy(ctx, in, live, now); err != nil {
			return NetworkConfigResult{}, err
		}
		trace.build = time.Since(step)
		doc.liveKey = key
		if cacheable {
			s.cache.put(in.EndpointID, doc, start)
		}
	}
	trace.peers = len(doc.peers)
	if maybeUnchanged {
		nodes, err := s.relayNodes(ctx, in.UserID, now)
		if err != nil {
			return NetworkConfigResult{}, err
		}
		digest := answerDigest(configurationClaims(s.issuer, now, doc.expiry, 0, doc.self, doc.peers, in.CapVersion), nodes, in.HomeRelay)
		if s.sent.unchanged(in.EndpointID, in.BaseGeneration, uint64(generation), digest, now) {
			return NetworkConfigResult{Unchanged: true}, nil
		}
		if err = s.store.SQL().QueryRowContext(ctx, generationBump, in.UserID, in.EndpointID).Scan(&generation); err != nil {
			return NetworkConfigResult{}, err
		}
	}
	step = time.Now()
	defer func() { trace.sign = time.Since(step) }()
	return s.signConfiguration(ctx, in, doc, generation, now)
}

// policyDocKey is everything of the request a cached document depends on: the account, the authority shape, the
// admitted scopes.
func policyDocKey(in NetworkConfigRequest, live map[string][]string) string {
	return fmt.Sprintf("%s\x02%s\x02%t\x02%s", in.UserID, in.Authority, len(in.AdmittedScopes) > 0, liveKey(live))
}

// buildPolicy is the policy part of an endpoint's configuration, read from one snapshot: its binding, every peer
// with its scopes, the refresh hint, and until when the result holds without a notification (policy_cache.go).
func (s *NetworkService) buildPolicy(ctx context.Context, in NetworkConfigRequest, live map[string][]string, now time.Time) (*policyDoc, error) {
	untilRevoked := in.Authority == AuthorityUntilRevoked
	// The shared mesh is brought up to date (once per change for the whole account) before the snapshot opens, so
	// the snapshot sees it; this writes, like the generation above.
	meshSeq, err := s.refreshMesh(ctx, in.UserID)
	if err != nil {
		return nil, err
	}
	tx, err := s.store.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	self, certExpiry, err := loadNetworkBinding(ctx, tx, in.UserID, in.EndpointID, now, len(in.AdmittedScopes) > 0)
	if err == sql.ErrNoRows || err == ErrUnavailable {
		return nil, ErrDenied
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.cli_client_session_id,a.user_machine_id,a.user_id,m.user_id,a.expires_at,array_to_json(a.capabilities),coalesce(a.terminal_session_id,''),coalesce(a.terminal_role,'')
		FROM paperboat.user_machine_access_sessions a
		JOIN paperboat.cli_client_sessions c ON c.id=a.cli_client_session_id AND c.user_id=a.user_id AND c.state='active' AND c.revoked_at IS NULL
		JOIN paperboat.user_machines m ON m.id=a.user_machine_id AND m.environment_id=a.environment_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL
		JOIN paperboat.control_environments e ON e.id=a.environment_id AND e.owner_user_id=m.user_id AND e.desired_state='active' AND e.revoked_at IS NULL
		JOIN paperboat.users actor ON actor.id=a.user_id AND actor.status='active'
		JOIN paperboat.users owner ON owner.id=m.user_id AND owner.status='active'
		WHERE a.state='active' AND a.revoked_at IS NULL
 AND (a.expires_at>$3 OR a.id=ANY($4::text[]))
		AND ((a.cli_client_session_id=$2 AND a.user_id=$1) OR (a.user_machine_id=$2 AND m.user_id=$1))
		AND ((a.machine_service_port IS NOT NULL AND paperboat.machine_service_session_allowed(a.id))
		 OR (a.terminal_role IN ('viewer','interactive') AND a.team_id IS NOT NULL AND m.state='online' AND m.online AND m.configured_capabilities @> ARRAY['terminal_host'] AND m.observed_capabilities @> ARRAY['terminal_host']
		 AND paperboat.terminal_session_role(a.user_id,a.terminal_session_id)=a.terminal_role
		 AND EXISTS(SELECT 1 FROM paperboat.teams t JOIN paperboat.team_members tm ON tm.team_id=t.team_id AND tm.account_id=a.user_id JOIN paperboat.team_resource_bindings b ON b.team_id=t.team_id AND b.resource_kind='terminal_session' AND b.resource_id=a.terminal_session_id AND b.active LEFT JOIN paperboat.team_terminal_session_grants sg ON sg.team_id=t.team_id AND sg.terminal_session_id=a.terminal_session_id AND sg.audience='selected_member' AND sg.account_id=a.user_id LEFT JOIN paperboat.team_terminal_session_grants ag ON ag.team_id=t.team_id AND ag.terminal_session_id=a.terminal_session_id AND ag.audience='all_members' WHERE t.team_id=a.team_id AND t.deleted_at IS NULL AND paperboat.team_paid_entitled(t.team_id) AND tm.active AND tm.membership_generation=a.terminal_membership_generation AND b.generation=a.terminal_binding_generation AND CASE WHEN sg.account_id IS NOT NULL THEN sg.generation ELSE ag.generation END=a.terminal_grant_generation))
		 OR (a.machine_service_port IS NULL AND coalesce(a.terminal_role,'owner')='owner' AND a.team_id IS NULL AND a.user_id=m.user_id AND m.owner_team_id IS NULL)
		 OR (coalesce(a.terminal_role,'owner')='owner' AND a.team_id IS NOT NULL AND NOT ('private_access'=ANY(a.capabilities)) AND NOT EXISTS(
		  SELECT 1 FROM unnest(a.capabilities) capability
		  WHERE NOT paperboat.team_machine_capability_allowed(a.user_id,a.team_id,a.user_machine_id,capability))))
		ORDER BY (a.id=ANY($4::text[])) DESC, (a.expires_at>$3) DESC, a.expires_at DESC, a.id LIMIT 257`, in.UserID, in.EndpointID, now, live["machine_access"])
	if err != nil {
		return nil, err
	}
	type grantRow struct {
		id, cli, machine, cliAccount, machineAccount, kind, terminalSession, terminalRole string
		expiry                                                                            time.Time
		capabilities                                                                      []string
	}
	grants := make([]grantRow, 0, 128)
	for rows.Next() {
		var g grantRow
		var rawCapabilities []byte
		if err = rows.Scan(&g.id, &g.cli, &g.machine, &g.cliAccount, &g.machineAccount, &g.expiry, &rawCapabilities, &g.terminalSession, &g.terminalRole); err != nil {
			rows.Close()
			return nil, unavailableWithCause(err)
		}
		var storedCapabilities []string
		if err = json.Unmarshal(rawCapabilities, &storedCapabilities); err != nil {
			rows.Close()
			return nil, unavailableWithCause(err)
		}
		g.kind = "machine_access"
		if g.terminalRole == "viewer" || g.terminalRole == "interactive" {
			g.capabilities = []string{"terminal"}
			grants = append(grants, g)
			continue
		}
		seenCapabilities := make(map[string]bool, len(storedCapabilities))
		for _, capability := range storedCapabilities {
			projected := capability
			switch capability {
			case "terminal", "exec", "managed_ssh", "private_access":
			case "files":
				projected = "file_transfer"
			case "preview_manage", "tunnel_manage":
				continue
			default:
				rows.Close()
				slog.Warn("peer network authority unavailable", "site", "network.go:326")
				return nil, ErrUnavailable
			}
			if !seenCapabilities[projected] {
				seenCapabilities[projected] = true
				g.capabilities = append(g.capabilities, projected)
			}
		}
		sort.Strings(g.capabilities)
		grants = append(grants, g)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return nil, rowErr
	}
	inspectorRows, err := tx.QueryContext(ctx, inspectorAdmissionsQuery, in.EndpointID, in.UserID, now, live["inspector"])
	if err != nil {
		return nil, err
	}
	for inspectorRows.Next() {
		var g grantRow
		if err = inspectorRows.Scan(&g.id, &g.cli, &g.machine, &g.cliAccount, &g.machineAccount, &g.expiry); err != nil {
			inspectorRows.Close()
			return nil, err
		}
		g.kind = "inspector"
		g.capabilities = []string{"inspector"}
		grants = append(grants, g)
	}
	err = inspectorRows.Err()
	inspectorRows.Close()
	if err != nil {
		return nil, err
	}

	compareRows, err := tx.QueryContext(ctx, `SELECT DISTINCT ON (a.assignment_id,a.native_cli_session_id,a.machine_id) a.assignment_id,a.native_cli_session_id,a.machine_id,a.account_id,a.account_id,a.expires_at
 FROM config_compare_admissions a
 JOIN cli_client_sessions c ON c.id=a.native_cli_session_id AND c.user_id=a.account_id AND c.state='active' AND c.revoked_at IS NULL
 JOIN users u ON u.id=a.account_id AND u.status='active'
 JOIN user_machines m ON m.id=a.machine_id AND m.user_id=a.account_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.installation_generation=a.installation_generation
 JOIN control_config_assignments assignment ON assignment.id=a.assignment_id AND assignment.version=a.assignment_version AND assignment.machine_id=a.machine_id AND assignment.consent_state='accepted'
 JOIN control_config_sync_statuses status ON status.environment_id=a.environment_id AND status.assignment_id=a.assignment_id AND status.machine_id=a.machine_id AND status.installation_generation=a.installation_generation AND status.remote_revision=a.expected_remote_revision
 WHERE ((a.native_cli_session_id=$2 AND a.account_id=$1) OR (a.machine_id=$2 AND a.account_id=$1))
 AND (a.expires_at>$3 OR a.assignment_id=ANY($4::text[]))
 AND ((a.workspace_id='personal' AND assignment.adopted_team_id IS NULL) OR (assignment.adopted_team_id=a.workspace_id AND EXISTS(SELECT 1 FROM teams t JOIN team_members member USING(team_id) WHERE t.team_id=a.workspace_id AND t.deleted_at IS NULL AND NOT t.suspended AND member.account_id=a.account_id AND member.active)))
 AND EXISTS(SELECT 1 FROM jsonb_array_elements(status.conflicts) conflict WHERE conflict->>'path'=a.config_path AND conflict->>'revision'=a.conflict_revision) ORDER BY a.assignment_id,a.native_cli_session_id,a.machine_id,a.expires_at DESC LIMIT 257`, in.UserID, in.EndpointID, now, live["config_compare"])
	if err != nil {
		return nil, err
	}
	for compareRows.Next() {
		var g grantRow
		if err = compareRows.Scan(&g.id, &g.cli, &g.machine, &g.cliAccount, &g.machineAccount, &g.expiry); err != nil {
			compareRows.Close()
			return nil, err
		}
		g.kind = "config_compare"
		g.capabilities = []string{"config_compare"}
		grants = append(grants, g)
	}
	err = compareRows.Err()
	compareRows.Close()
	if err != nil {
		return nil, err
	}
	// A session recording replay from the CLI (recordings.admitNativeTx): the viewer's endpoint reaches the machine
	// for capability recording until its latest ticket for the recording expires, while the account may still replay
	// there (a personal machine's owner always may). The scope's resource is the recording (the ticket's resource_id), as config_compare's is its assignment.
	recordingRows, err := tx.QueryContext(ctx, `SELECT DISTINCT ON (a.recording_id, a.native_cli_session_id, a.machine_id) a.recording_id, a.native_cli_session_id, a.machine_id, a.account_id, m.user_id, a.expires_at
 FROM recording_native_admissions a
 JOIN cli_client_sessions c ON c.id = a.native_cli_session_id AND c.user_id = a.account_id AND c.state = 'active' AND c.revoked_at IS NULL
 JOIN users u ON u.id = a.account_id AND u.status = 'active'
 JOIN user_machines m ON m.id = a.machine_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL
 LEFT JOIN teams t ON t.team_id = paperboat.recording_team(m.id) AND t.deleted_at IS NULL AND NOT t.suspended
 WHERE ((a.native_cli_session_id = $1 AND a.account_id = $2) OR (a.machine_id = $1 AND m.user_id = $2))
 AND (a.expires_at > $3 OR a.recording_id = ANY($4::text[]))
 AND ((paperboat.recording_team(m.id) IS NULL AND m.user_id = a.account_id) OR NOT isempty(paperboat.workspace_policy_rule_ports(a.account_id, t.team_id, a.machine_id, 'recording_view')))
 ORDER BY a.recording_id, a.native_cli_session_id, a.machine_id, a.expires_at DESC LIMIT 257`, in.EndpointID, in.UserID, now, live["recording"])
	if err != nil {
		return nil, err
	}
	for recordingRows.Next() {
		var g grantRow
		if err = recordingRows.Scan(&g.id, &g.cli, &g.machine, &g.cliAccount, &g.machineAccount, &g.expiry); err != nil {
			recordingRows.Close()
			return nil, err
		}
		g.kind = "recording"
		g.capabilities = []string{"recording"}
		grants = append(grants, g)
	}
	err = recordingRows.Err()
	recordingRows.Close()
	if err != nil {
		return nil, err
	}
	// Work the endpoint already runs leads, so the grant and scope bounds drop the newest grants, never a running
	// stream (Linux test VM: ~3 exec grants/s pushed a held exec or terminal out after ~45 s and its scope lease closed it).
	slices.SortStableFunc(grants, func(a, b grantRow) int {
		return boolRank(slices.Contains(live[a.kind], a.id)) - boolRank(slices.Contains(live[b.kind], b.id))
	})
	grants = boundGrants(grants)
	byPeer := map[string]*networkPeer{}
	admittedPeers := map[string]bool{}
	scopes := 0
	// exp is a refresh hint, not a deadline: the endpoint keeps its last configuration in force until a newer one
	// or a revocation replaces it (Tailscale's netmap has no expiry), and machine events wake a renewal on every
	// change. It is bounded by the relay grants it travels with and by the certificates it names.
	expiry := now.Add(relaylimits.MaxGrantLifetime)
	if certExpiry.After(now) && certExpiry.Before(expiry) {
		expiry = certExpiry
	}
	for _, g := range grants {
		// Preserve expired admission scopes until explicit withdrawal. Machines
		// use their original expiry to reject new operations, not to kill work
		// already admitted under this exact access-session identity.
		if len(g.capabilities) == 0 {
			continue
		}
		peerID, peerAccount, direction := g.machine, g.machineAccount, "dial"
		if in.EndpointID == g.machine {
			peerID, peerAccount, direction = g.cli, g.cliAccount, "accept"
		}
		admitted := slices.Contains(live[g.kind], g.id)
		if admitted {
			admittedPeers[peerID] = true
		}
		p := byPeer[peerID]
		if p == nil {
			binding, peerCertExpiry, e := loadNetworkBinding(ctx, tx, peerAccount, peerID, now, admitted)
			if e == sql.ErrNoRows || e == ErrUnavailable {
				continue
			}
			if e != nil {
				return nil, e
			}
			if len(byPeer) >= maximumNetworkAccessPeers {
				continue
			}
			if peerCertExpiry.After(now) && peerCertExpiry.Before(expiry) {
				expiry = peerCertExpiry
			}
			p = &networkPeer{Identity: binding}
			byPeer[peerID] = p
		}
		for _, capability := range g.capabilities {
			if scopes >= maximumNetworkAccessScopes {
				break
			}
			p.Scopes = append(p.Scopes, networkScope{ResourceKind: g.kind, ResourceID: g.id, ResourceGeneration: 1, Capability: capability, Direction: direction, Port: peernetwork.NetworkPort, ExpiresAt: g.expiry.Unix()})
			scopes++
		}
	}
	// Durable personal-machine connectivity is distinct from application access.
	// Exact access-session scopes above still authorize every application stream.
	meshEntries, err := s.mesh.at(ctx, tx, self.AccountID, meshSeq)
	if err != nil {
		return nil, err
	}
	mesh, err := personalNetworkPeers(ctx, tx, self, now, &expiry, admittedPeers, live["machine_network"], meshEntries)
	if err != nil {
		return nil, err
	}
	for _, binding := range mesh {
		p := byPeer[binding.EndpointID]
		if p == nil {
			if len(byPeer) >= maximumAccountEndpoints {
				return nil, ErrResourceLimit
			}
			p = &networkPeer{Identity: binding}
			byPeer[binding.EndpointID] = p
		}
		if scopes >= peernetwork.MaxScopes {
			return nil, ErrResourceLimit
		}
		meshExpiry := expiry.Unix()
		if untilRevoked {
			meshExpiry = 0
		}
		p.Scopes = append(p.Scopes, personalNetworkScope(self, binding, meshExpiry))
		scopes++
	}
	if untilRevoked && self.Role == peernetwork.RoleCLI {
		hosts, err := teamHostPeers(ctx, tx, self, now)
		if err != nil {
			return nil, err
		}
	teamHosts:
		for _, h := range hosts {
			p := byPeer[h.binding.EndpointID]
			if p == nil {
				if len(byPeer) >= maximumAccountEndpoints {
					break
				}
				p = &networkPeer{Identity: h.binding}
				byPeer[h.binding.EndpointID] = p
				if h.certExpiry.After(now) && h.certExpiry.Before(expiry) {
					expiry = h.certExpiry
				}
			}
			for _, capability := range h.capabilities {
				if scopes >= peernetwork.MaxScopes {
					break teamHosts
				}
				p.Scopes = append(p.Scopes, networkScope{ResourceKind: "machine_access", ResourceID: h.binding.EndpointID, ResourceGeneration: uint64(h.generation), Capability: capability, Direction: "dial", Port: peernetwork.NetworkPort, ExpiresAt: 0})
				scopes++
			}
		}
	}
	if untilRevoked && self.Role == peernetwork.RoleMachine {
		offline, err := offlineAccessPeers(ctx, tx, self, now, meshEntries)
		if err != nil {
			return nil, err
		}
	offlinePeers:
		for _, o := range offline {
			p := byPeer[o.binding.EndpointID]
			if p == nil {
				if len(byPeer) >= maximumAccountEndpoints {
					break
				}
				p = &networkPeer{Identity: o.binding}
				byPeer[o.binding.EndpointID] = p
				if o.certExpiry.After(now) && o.certExpiry.Before(expiry) {
					expiry = o.certExpiry
				}
			}
			for _, capability := range o.capabilities {
				if scopes >= peernetwork.MaxScopes {
					break offlinePeers
				}
				p.Scopes = append(p.Scopes, networkScope{ResourceKind: "machine_access", ResourceID: self.EndpointID, ResourceGeneration: uint64(o.generation), Capability: capability, Direction: "accept", Port: peernetwork.NetworkPort, ExpiresAt: 0})
				scopes++
			}
		}
	}
	peerIDs := make([]string, 0, len(byPeer))
	for id := range byPeer {
		peerIDs = append(peerIDs, id)
	}
	sort.Strings(peerIDs)
	peers := make([]networkPeer, 0, len(peerIDs))
	for _, id := range peerIDs {
		p := byPeer[id]
		sort.Slice(p.Scopes, func(i, j int) bool {
			if p.Scopes[i].ResourceID != p.Scopes[j].ResourceID {
				return p.Scopes[i].ResourceID < p.Scopes[j].ResourceID
			}
			return p.Scopes[i].Capability < p.Scopes[j].Capability
		})
		peers = append(peers, *p)
	}
	// The document holds until its earliest time-based input: the refresh hint (certificates) and every grant expiry.
	validUntil := now.Add(maxPolicyAge)
	if expiry.Before(validUntil) {
		validUntil = expiry
	}
	for _, g := range grants {
		if g.expiry.After(now) && g.expiry.Before(validUntil) {
			validUntil = g.expiry
		}
	}
	// A team's paid entitlement (paperboat.team_paid_entitled) starts and ends by time, with no write: the document
	// holds only until the next such instant of a team the endpoint's account belongs to or its machine is shared with.
	var entitlementChange sql.NullTime
	if err = tx.QueryRowContext(ctx, `WITH teams AS (
 SELECT team_id FROM paperboat.team_members WHERE account_id=$1 AND active
 UNION SELECT team_id FROM paperboat.team_resource_bindings WHERE resource_kind='machine' AND resource_id=$2 AND active
 UNION SELECT owner_team_id FROM paperboat.user_machines WHERE id=$2 AND owner_team_id IS NOT NULL)
SELECT min(t) FROM (
 SELECT w.period_start AS t FROM paperboat.billing_workspaces w JOIN teams USING (team_id) WHERE w.user_id IS NULL
 UNION ALL SELECT paperboat.billing_workspace_paid_until(w) FROM paperboat.billing_workspaces w JOIN teams USING (team_id) WHERE w.user_id IS NULL) x
WHERE t>$3`, self.AccountID, self.MachineID, now).Scan(&entitlementChange); err != nil {
		return nil, err
	}
	if entitlementChange.Valid && entitlementChange.Time.Before(validUntil) {
		validUntil = entitlementChange.Time
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &policyDoc{self: self, peers: peers, expiry: expiry, validUntil: validUntil, keys: policyKeys(self, peers)}, nil
}

// signConfiguration signs one request's answer from a policy document: the configuration under this request's
// generation, the regional candidates and relay grants from the current relay nodes.
func (s *NetworkService) signConfiguration(ctx context.Context, in NetworkConfigRequest, doc *policyDoc, generation int64, now time.Time) (NetworkConfigResult, error) {
	claims := configurationClaims(s.issuer, now, doc.expiry, generation, doc.self, doc.peers, in.CapVersion)
	token, err := s.signer.SignNetworkConfiguration(claims)
	if err != nil {
		return NetworkConfigResult{}, err
	}
	if len(token) > networkConfigLimit {
		return NetworkConfigResult{}, ErrResourceLimit
	}
	nodes, err := s.relayNodes(ctx, in.UserID, now)
	if err != nil {
		return NetworkConfigResult{}, err
	}
	candidates, err := s.regionalCandidates(nodes, in.UserID, in.EndpointID, generation, now)
	if err != nil {
		return NetworkConfigResult{}, err
	}
	if len(candidates) > networkConfigLimit {
		slog.Warn("peer network authority unavailable", "site", "network.go:500")
		return NetworkConfigResult{}, ErrUnavailable
	}
	relayGrants, err := s.relayGrants(nodes, doc.self, doc.peers, generation, now)
	if err != nil {
		return NetworkConfigResult{}, err
	}
	result := NetworkConfigResult{Configuration: token, CandidateSet: candidates, RelayGrants: relayGrants}
	digest := answerDigest(claims, nodes, in.HomeRelay)
	if result.ConfigurationDelta = s.sent.answer(in.EndpointID, in.BaseGeneration, token, claims, digest, now); result.ConfigurationDelta != nil {
		result.Configuration = ""
	}
	return result, nil
}

func personalNetworkScopeID(self, peer networkBinding) string {
	first, second := self.EndpointID, peer.EndpointID
	if second < first {
		first, second = second, first
	}
	hash := sha256.Sum256([]byte(self.AccountID + "\x00" + first + "\x00" + second))
	return "network_" + hex.EncodeToString(hash[:])
}

// personalNetworkScope is a mesh scope ending at expiresAt (a Unix time; 0 means until revoked).
func personalNetworkScope(self, peer networkBinding, expiresAt int64) networkScope {
	direction := "dial"
	if self.Role == peernetwork.RoleMachine && (peer.Role == peernetwork.RoleCLI || self.EndpointID > peer.EndpointID) {
		direction = "accept"
	}
	return networkScope{ResourceKind: "machine_network", ResourceID: personalNetworkScopeID(self, peer), ResourceGeneration: 1, Capability: "connect", Direction: direction, Port: peernetwork.NetworkPort, ExpiresAt: expiresAt}
}

func personalNetworkPeers(ctx context.Context, tx *sql.Tx, self networkBinding, now time.Time, expiry *time.Time, admittedPeers map[string]bool, admittedNetworks []string, mesh map[string]networkBinding) ([]networkBinding, error) {
	if self.Role == peernetwork.RoleMachine {
		var personal bool
		if err := tx.QueryRowContext(ctx, `SELECT owner_team_id IS NULL FROM paperboat.user_machines WHERE id=$1 AND user_id=$2`, self.EndpointID, self.AccountID).Scan(&personal); err != nil {
			return nil, err
		}
		if !personal {
			return nil, nil
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT n.endpoint_id FROM paperboat.peer_network_identities n
 WHERE n.user_id=$1 AND n.endpoint_id<>$2 AND n.revoked_at IS NULL AND
 ((n.role='machine' AND EXISTS(SELECT 1 FROM paperboat.user_machines m WHERE m.id=n.machine_id AND m.user_id=n.user_id AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL))
 OR ($3='machine' AND n.role='cli' AND EXISTS(SELECT 1 FROM paperboat.cli_client_sessions c WHERE c.id=n.endpoint_id AND c.user_id=n.user_id AND c.state='active' AND c.revoked_at IS NULL)))
 ORDER BY n.endpoint_id LIMIT $4`, self.AccountID, self.EndpointID, self.Role, maximumAccountEndpoints+1)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Every endpoint of the account is a peer; the bound is the stated product
	// maximum, and the signed configuration size is checked where it is built.
	if len(ids) > maximumAccountEndpoints {
		return nil, ErrResourceLimit
	}
	// The shared mesh (mesh.go) holds every peer with a current certificate, built once per change: a peer is read
	// from it, not loaded again per endpoint (3 queries per peer). A peer this endpoint reports as admitted may keep
	// a superseded or expired certificate, so it alone is loaded as before.
	var validUntil sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT valid_until FROM paperboat.peer_mesh_accounts WHERE account_id=$1`, self.AccountID).Scan(&validUntil); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if validUntil.Valid && validUntil.Time.After(now) && validUntil.Time.Before(*expiry) {
		*expiry = validUntil.Time // the earliest certificate of the mesh: never later than this endpoint's peers' own
	}
	peers := make([]networkBinding, 0, len(ids))
	for _, id := range ids {
		if admittedPeers[id] || slices.Contains(admittedNetworks, personalNetworkScopeID(self, networkBinding{Binding: peernetwork.Binding{EndpointID: id}})) {
			binding, certExpiry, err := loadNetworkBinding(ctx, tx, self.AccountID, id, now, true)
			if err == sql.ErrNoRows || err == ErrUnavailable {
				continue
			}
			if err != nil {
				return nil, err
			}
			if certExpiry.After(now) && certExpiry.Before(*expiry) {
				*expiry = certExpiry
			}
			peers = append(peers, binding)
			continue
		}
		if binding, ok := mesh[id]; ok {
			peers = append(peers, binding)
		}
	}
	return peers, nil
}

// relayGrants signs one grant per eligible relay node, each for every peer: relay traffic is never refused
// for bandwidth (a relay caps each account's speed instead, relay/derpquic).
func (s *NetworkService) relayGrants(nodes []relayNodeRow, self networkBinding, peers []networkPeer, generation int64, now time.Time) ([]string, error) {
	grants := make([]string, 0, 4)
	relays := 0
	for _, n := range nodes {
		if !n.relay {
			continue
		}
		if relays == 32 {
			return nil, ErrResourceLimit
		}
		relays++
		nodeID, processEpoch, nodeGeneration, peerRelayEnabled := n.id, n.processEpoch, n.nodeGeneration, n.peerRelay
		peerRelayWG, peerRelayDisco, peerRelayAddress := n.peerRelayWG, n.peerRelayDisco, n.peerRelayAddress
		peerRelayReady := peerRelayEnabled && self.DiscoPublicKey != "" && peersHaveDiscoKeys(peers)
		// Not capped by the configuration's refresh hint or the relay's registry lease: a grant is bound to this
		// configuration's generation and to the relay's process epoch, and is withdrawn by a relay fence, not by
		// running out (relaylimits.MaxGrantLifetime).
		expires := now.Add(relaylimits.MaxGrantLifetime)
		if peerRelayEnabled && (len(peerRelayWG) != 32 || len(peerRelayDisco) != 32 || !peerRelayAddress.Valid) {
			slog.Warn("peer network authority unavailable", "site", "network.go:620")
			return nil, ErrUnavailable
		}
		claims := peernetwork.RelayGrant{Version: peernetwork.RelayGrantVersion, Issuer: s.issuer, Audience: peernetwork.RelayGrantAudience, IssuedAt: now.Unix(), ExpiresAt: expires.Unix(),
			Generation: uint64(generation), AccountID: self.AccountID, EndpointID: self.EndpointID, WireGuardPublicKey: self.WireGuardPublicKey, DiscoPublicKey: self.DiscoPublicKey,
			CertificateFingerprint: self.QUICCertificateFingerprint, QUICPublicKey: self.QUICPublicKey, NodeID: nodeID, NodeGeneration: uint64(nodeGeneration), ProcessEpoch: processEpoch}
		parts := relayGrantMembershipParts(relayGrantMemberships(self, peers, expires.Unix()))
		if len(parts) > peernetwork.MaxGrantParts { // the relay refuses a longer credential
			slog.Warn("peer network authority unavailable", "site", "network.go:relayGrantParts")
			return nil, ErrUnavailable
		}
		for i, part := range parts {
			g := claims
			g.Memberships = part
			if i == 0 && peerRelayReady { // the relay and the CLI merge the parts; the descriptor rides in the first
				g.PeerRelay = &peernetwork.RelayDescriptor{WireGuardPublicKey: base64.RawURLEncoding.EncodeToString(peerRelayWG),
					DiscoPublicKey: base64.RawURLEncoding.EncodeToString(peerRelayDisco), VirtualAddress: peerRelayAddress.String}
			}
			grant, err := s.signer.SignRelayGrant(g)
			if err != nil {
				return nil, err
			}
			if len(grant) > peernetwork.MaxGrantBytes { // the relay refuses a longer token
				slog.Warn("peer network authority unavailable", "site", "network.go:653")
				return nil, ErrUnavailable
			}
			grants = append(grants, grant)
		}
	}
	return grants, nil
}

// relayGrantMembershipParts splits memberships into grant parts within the relay's token bound (the token is the base64
// of the claims; identity claims and header take well under 4 KiB). There is always one part, empty for an endpoint
// without relayable peers.
func relayGrantMembershipParts(memberships []networkScope) [][]networkScope {
	budget := peernetwork.MaxGrantBytes*3/4 - 4<<10
	parts := [][]networkScope{{}}
	used := 0
	for _, m := range memberships {
		raw, _ := json.Marshal(m)
		last := len(parts) - 1
		if used+len(raw)+1 > budget {
			parts, last, used = append(parts, nil), last+1, 0
		}
		parts[last] = append(parts[last], m)
		used += len(raw) + 1
	}
	return parts
}

// relayGrantMemberships is what one relay grant carries for peers: memberships, not a list of peers (PLAN 3b "Relay
// pair authorization by membership"; Tailscale's DERP server authorizes a node once and keeps no per-peer lists,
// derp/derpserver/derpserver.go verifyClient). The account mesh, a per-pair machine_network scope per peer in the
// configuration, becomes the account's one network (peernetwork.AccountNetworkID) in each direction this endpoint
// holds; the relay pairs two endpoints of one account holding it in opposite directions (derpquic membership.go). A
// peer without a mesh scope (a team or another account's endpoint) adds its relay scopes (relayGrantScopes), each once.
// A grant is then O(memberships), not O(peers): a mesh endpoint of any account size carries one or two.
// None is left out: relayGrantMembershipParts splits them into as many parts as the relay's token bound needs.
func relayGrantMemberships(self networkBinding, peers []networkPeer, grantExpiry int64) []networkScope {
	relayable := make([][]networkScope, 0, len(peers))
	for _, peer := range peers {
		if peer.Identity.WireGuardPublicKey != "" { // one without a key cannot be relayed
			relayable = append(relayable, peer.Scopes)
		}
	}
	return peernetwork.RelayGrantMemberships(self.AccountID, relayable, grantExpiry)
}

// relayGrantScopes is the part of a peer's document scopes a relay grant carries; peernetwork.RelayGrantKeepsStanding
// decides which until-revoked scopes (expires_at 0) it keeps and stamps with the grant expiry. An account's own CLI
// peers pair by machine_network and leave their session rights out (P0 2026-10-26: 64 CLI peers x 4 session rights
// went past the relay's 128-scope bound); a team peer of another account has no machine_network scope, so its standing
// machine_access scopes are the pair. The CLI builds the same set to compare a grant with its configuration.
func relayGrantScopes(scopes []networkScope, grantExpiry int64) []networkScope {
	return peernetwork.RelayGrantScopes(scopes, grantExpiry)
}

func peersHaveDiscoKeys(peers []networkPeer) bool {
	for _, peer := range peers {
		if peer.Identity.DiscoPublicKey == "" {
			return false
		}
	}
	return true
}

func (s *NetworkService) regionalCandidates(rows []relayNodeRow, accountID, endpointID string, generation int64, now time.Time) (string, error) {
	nodes := make([]peernetwork.RegionalNode, 0, peernetwork.MaxRegionalCandidates)
	for i, r := range rows {
		if i == peernetwork.MaxRegionalCandidates {
			break
		}
		n := peernetwork.RegionalNode{NodeID: r.id, NodeGeneration: uint64(r.nodeGeneration), ProcessEpoch: r.processEpoch, Region: r.region, FailureDomain: r.failureDomain, EndpointHost: r.host, State: r.state, CapacityLimit: r.capacityLimit, CapacityUsed: r.capacityUsed}
		roles, transports, pin, tcpPort, quicPort := r.roles, r.transports, r.pin, r.tcpPort, r.quicPort
		observed, expires, capacityAt, drain, selfHosted := r.observed, r.expires, r.capacityAt, r.drain, r.selfHosted
		// A node whose row clients would refuse is left out; it never withholds the other relays.
		if json.Unmarshal(roles, &n.Roles) != nil || json.Unmarshal(transports, &n.Transports) != nil || tcpPort < 0 || tcpPort > 65535 || quicPort < 0 || quicPort > 65535 || len(pin) != 0 && len(pin) != sha256.Size {
			//paperboat:allow-source-policy sensitive-log owner=peersessions reason=node-id-only-no-secret
			slog.Warn("regional candidates omit a relay with an invalid registry row", "site", "network.go:regionalCandidates", "node", n.NodeID)
			continue
		}
		n.EndpointTCPPort, n.EndpointQUICPort = uint16(tcpPort), uint16(quicPort)
		n.ObservedAt, n.ExpiresAt, n.CapacityObservedAt = observed.Unix(), expires.Unix(), capacityAt.Unix()
		if drain.Valid {
			deadline := drain.Time.Unix()
			n.DrainDeadline = &deadline
		}
		if len(pin) != 0 {
			n.TLSSPKISHA256 = base64.RawURLEncoding.EncodeToString(pin)
		}
		if selfHosted {
			n.Hosting = peernetwork.HostingSelfHosted
		}
		nodes = append(nodes, n)
	}
	expires := now.Add(relaylimits.MaxGrantLifetime) // as long as the grants of the relays it names
	return s.signer.SignRegionalCandidates(regionalCandidateSet(s.issuer, accountID, endpointID, generation, now, expires, nodes))
}

// regionalCandidateSet is the signed regional candidate set for one endpoint.
func regionalCandidateSet(issuer, accountID, endpointID string, generation int64, now, expires time.Time, nodes []peernetwork.RegionalNode) peernetwork.RegionalCandidates {
	return peernetwork.RegionalCandidates{Schema: peernetwork.RegionalCandidatesSchema, Issuer: issuer, Audience: peernetwork.RegionalCandidatesAudience, AccountID: accountID, EndpointID: endpointID,
		AuthorizationGeneration: uint64(generation), Generation: uint64(generation), IssuedAt: now.Unix(), ExpiresAt: expires.Unix(), Nodes: nodes}
}

// maximumNetworkGrants bounds the access sessions one endpoint's document carries. A terminal's session projects three
// scopes (terminal, file_transfer, private_access), so 96 access scopes held only 32 terminals per client (F21-164): the
// bound is sized for 256 sessions of three scopes, and a machine holds 100 terminals for each of two busy clients.
const maximumNetworkGrants = 256

// Application access scopes (newest first) leave room for the durable machine mesh, so
// accumulated sessions can never push it out of the configuration.
const (
	maximumNetworkScopes       = 3*maximumNetworkGrants + 32
	maximumNetworkAccessScopes = maximumNetworkScopes - 32
	maximumNetworkAccessPeers  = 48
)

// boolRank orders true before false.
func boolRank(first bool) int {
	if first {
		return 0
	}
	return 1
}

func boundGrants[T any](grants []T) []T {
	if len(grants) <= maximumNetworkGrants {
		return grants
	}
	//paperboat:allow-source-policy sensitive-log owner=peersessions reason=counts-only-no-secret-values
	slog.Warn("peer network configuration dropped the oldest access sessions", "kept", maximumNetworkGrants, "candidates", len(grants))
	return grants[:maximumNetworkGrants]
}

func validWireGuardPublicKey(key []byte) bool {
	if len(key) != curve25519.PointSize {
		return false
	}
	// X25519 rejects the low-order inputs that would produce an all-zero
	// shared secret. The scalar is validation-only and carries no identity.
	var scalar [curve25519.ScalarSize]byte
	for i := range scalar {
		scalar[i] = byte(i + 1)
	}
	_, err := curve25519.X25519(scalar[:], key)
	return err == nil
}

// admittedSeenEvery bounds the writes markAdmittedSessions makes: one per expired admitted session per interval.
const admittedSeenEvery = 10 * time.Minute

// markAdmittedSessions records that this endpoint still names its expired access sessions as admitted (an open stream
// outlives the credential), so the usermachines sweeper keeps them; one that nobody names is retired once a whole
// document lifetime has passed without it. Only the endpoint's own sessions, and only expired ones, are written.
func (s *NetworkService) markAdmittedSessions(ctx context.Context, in NetworkConfigRequest, ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_access_sessions SET admitted_seen_at=$1
 WHERE id=ANY($2::text[]) AND state='active' AND expires_at<$1 AND (admitted_seen_at IS NULL OR admitted_seen_at<$1::timestamptz-make_interval(secs=>$5))
 AND ((cli_client_session_id=$3 AND user_id=$4) OR user_machine_id=$3)`, now, ids, in.EndpointID, in.UserID, admittedSeenEvery.Seconds())
	return err
}
