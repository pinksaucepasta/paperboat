package tailnet

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=network-key-custody
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=network-fault-injection
	"tailscale.com/types/nettype"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=network-fault-injection
	"tailscale.com/wgengine/magicsock"
)

// A configuration names every other endpoint of the account with its scopes. Its bounds are the ones
// the server issues under (shared/peernetwork). Engine socket limits (MaxFlows) and the client's own
// scope request limit (api.MaxAdmittedNetworkScopes) are not configuration bounds.
const (
	MaxConfigurationBytes  = peernetwork.MaxConfigurationBytes
	MaxConfigurationPeers  = peernetwork.MaxEndpoints
	MaxConfigurationScopes = peernetwork.MaxScopes
)
const RefreshInterval = 30 * time.Second
const NetworkPort = 443

var ErrAuthority = errors.New("peer network authority is invalid")
var ErrStaleAuthority = errors.New("peer network authority is stale")
var ErrExpiredAuthority = errors.New("peer network authority expired; reconnect to Paperboat")

type NetworkBinding struct {
	AccountID                  string `json:"account_id"`
	EndpointID                 string `json:"endpoint_id"`
	Role                       string `json:"role"`
	MachineID                  string `json:"machine_id"`
	EndpointGeneration         uint64 `json:"endpoint_generation"`
	MachineGeneration          uint64 `json:"machine_generation"`
	KeyGeneration              uint64 `json:"key_generation"`
	WireGuardPublicKey         string `json:"wireguard_public_key"`
	DiscoPublicKey             string `json:"disco_public_key"`
	QUICCertificateFingerprint string `json:"quic_certificate_fingerprint"`
	QUICPublicKey              string `json:"quic_public_key"`
	VirtualAddress             string `json:"virtual_address"`
}
type NetworkScope struct {
	ResourceKind       string `json:"resource_kind"`
	ResourceID         string `json:"resource_id"`
	ResourceGeneration uint64 `json:"resource_generation"`
	Capability         string `json:"capability"`
	Direction          string `json:"direction"`
	Port               uint16 `json:"port"`
	ExpiresAt          int64  `json:"expires_at"`
}
type NetworkPeer struct {
	Identity NetworkBinding `json:"identity"`
	Scopes   []NetworkScope `json:"scopes"`
	// Endpoints are the peer's own candidate UDP addresses ("ip:port": its LAN interfaces, STUN-mapped and
	// port-mapped addresses), so the engine can reach it directly with no relay (PLAN 3b "Direct paths
	// without a relay"). Absent while the server does not send them.
	Endpoints []string `json:"endpoints,omitempty"`
}

// MaxPeerEndpoints bounds the candidate addresses one peer entry may carry.
const MaxPeerEndpoints = 8

// validPeerEndpoints reports whether endpoints are at most MaxPeerEndpoints distinct, canonical unicast
// ip:port addresses with a nonzero port (the server stores them normalized the same way).
func validPeerEndpoints(endpoints []string) bool {
	if len(endpoints) > MaxPeerEndpoints {
		return false
	}
	seen := make(map[netip.AddrPort]bool, len(endpoints))
	for _, raw := range endpoints {
		addr, err := netip.ParseAddrPort(raw)
		if err != nil || !reportableEndpoint(addr) || addr.String() != raw || seen[addr] {
			return false
		}
		seen[addr] = true
	}
	return true
}

// reportableEndpoint is the server's rule for a stored endpoint: a unicast address that is not loopback,
// unspecified, multicast, zoned or IPv4-mapped, with a port.
func reportableEndpoint(addr netip.AddrPort) bool {
	ip := addr.Addr()
	return addr.Port() != 0 && ip.IsValid() && ip.Zone() == "" && !ip.Is4In6() && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast()
}

type NetworkConfiguration struct {
	Version    int            `json:"version"`
	Issuer     string         `json:"iss"`
	Audience   string         `json:"aud"`
	IssuedAt   int64          `json:"iat"`
	ExpiresAt  int64          `json:"exp"`
	Generation uint64         `json:"generation"`
	Self       NetworkBinding `json:"self"`
	Peers      []NetworkPeer  `json:"peers"`
}

// NetworkKeys is satisfied by the existing authenticated, bounded JWKS cache.
type NetworkKeys interface {
	Lookup(context.Context, string) (ed25519.PublicKey, bool, error)
}
type AuthorityOptions struct {
	Store  config.ProfileStore
	Issuer string
	// Self pins locally approved application identity, not fields from the reply.
	Self NetworkBinding
	Keys NetworkKeys
	// TestOnlyPacketListener supplies real filtered UDP sockets to network fault tests.
	TestOnlyPacketListener nettype.PacketListener
	// TestOnlyDERPCarrier wraps the authenticated carrier for network fault tests.
	TestOnlyDERPCarrier func(magicsock.DERPCarrier) magicsock.DERPCarrier
}

// Authority owns verified configuration and every engine attached through it.
// Application operation credentials and QUIC certificate validation remain mandatory.
type Authority struct {
	streamScopes bool
	scopeChanges chan struct{}
	admitted     map[api.AdmittedNetworkScope]int
	// expiresAt gates new flows; admitted flows are fenced by revocation.
	expiresAt    atomic.Int64
	mu           sync.Mutex
	updateMu     sync.Mutex // custody I/O must never hold the expiry/lifecycle lock
	options      AuthorityOptions
	current      *NetworkConfiguration
	private      key.NodePrivate
	stunTimer    *time.Timer
	closed       bool
	expired      bool
	lastVerified time.Time
	relayDenied  *api.APIError
	done         chan struct{}
	server       *UDPServer
	clientEngine *mesh.Server
	// endpointReports is the server's last endpoint_reports answer; reportMu guards the report schedule
	// (endpoint_report.go).
	endpointReports atomic.Bool
	// membershipGrants is the server's last membership_grants answer: renewals then ask for them.
	membershipGrants atomic.Bool
	reportMu         sync.Mutex
	reportTimer      *time.Timer
	lastReport       time.Time
	reported         *[]string // the last endpoint set the server accepted; nil before the first
	clients          map[string]authorizedClient
	clientSlots      chan struct{}
	regional         *RegionalCandidates
	relay            relayAuthority
	recoveryWorkers  sync.WaitGroup
	refreshSlot      chan struct{}
	refreshWake      chan struct{}
	refreshNow       chan struct{}
}

type authorizedClient struct {
	client *UDPClient
	peer   NetworkBinding
	scopes []NetworkScope
	node   key.NodePublic
}

func (a *Authority) EligibleRegionalNodes(role, transport string, allowedRegions map[string]bool, now time.Time) ([]RegionalNode, Redundancy, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.regional == nil || a.closed {
		return nil, RedundancyNone, ErrRegionalAuthority
	}
	return a.regional.Eligible(role, transport, allowedRegions, now)
}

// RequestRefresh asks Run to renew the signed configuration now, for example when a peer's
// regional probe arrives while the relay health observations have gone stale. Requests collapse
// and Run spaces the renewals it starts for them.
func (a *Authority) RequestRefresh() {
	select {
	case a.refreshNow <- struct{}{}:
	default:
	}
}

func NewAuthority(options AuthorityOptions) (*Authority, error) {
	issuer, err := config.NormalizeIssuer(options.Issuer)
	if err != nil || issuer != options.Issuer || options.Keys == nil || !validLocalBinding(options.Self) {
		return nil, ErrAuthority
	}
	return &Authority{options: options, done: make(chan struct{}), refreshSlot: make(chan struct{}, 1), refreshWake: make(chan struct{}, 1), refreshNow: make(chan struct{}, 1)}, nil
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return false
		}
	}
	return true
}
func validLocalBinding(b NetworkBinding) bool {
	hash, err := hex.DecodeString(b.QUICCertificateFingerprint)
	quicPublic, keyErr := base64.RawURLEncoding.Strict().DecodeString(b.QUICPublicKey)
	if !validID(b.AccountID) || !validID(b.EndpointID) || b.EndpointGeneration == 0 || b.EndpointGeneration > 1<<53-1 || err != nil || len(hash) != 32 || hex.EncodeToString(hash) != b.QUICCertificateFingerprint || keyErr != nil || len(quicPublic) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(quicPublic) != b.QUICPublicKey {
		return false
	}
	return b.Role == "cli" && b.MachineID == "" && b.MachineGeneration == 0 || b.Role == "machine" && b.MachineID == b.EndpointID && b.MachineGeneration > 0 && b.MachineGeneration <= 1<<53-1
}
func publicKey(value string) (key.NodePublic, error) {
	var result key.NodePublic
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != value {
		return result, ErrAuthority
	}
	var scalar [32]byte
	scalar[0] = 1
	validation, _ := ecdh.X25519().NewPrivateKey(scalar[:])
	pub, _ := ecdh.X25519().NewPublicKey(raw)
	if _, err := validation.ECDH(pub); err != nil {
		return result, ErrAuthority
	}
	if err := result.UnmarshalText([]byte("nodekey:" + hex.EncodeToString(raw))); err != nil || result.IsZero() {
		return result, ErrAuthority
	}
	return result, nil
}
func privateKey(raw []byte) (key.NodePrivate, error) {
	var result key.NodePrivate
	if len(raw) != 32 || raw[0]&7 != 0 || raw[31]&128 != 0 || raw[31]&64 == 0 || result.UnmarshalText([]byte("privkey:"+hex.EncodeToString(raw))) != nil || result.IsZero() {
		return result, ErrAuthority
	}
	return result, nil
}
func validBinding(b NetworkBinding) bool {
	addr, err := netip.ParseAddr(b.VirtualAddress)
	_, keyErr := publicKey(b.WireGuardPublicKey)
	disco, discoErr := derpquic.ParseDiscoKey(b.DiscoPublicKey)
	return validLocalBinding(b) && b.KeyGeneration > 0 && b.KeyGeneration <= 1<<53-1 && keyErr == nil && discoErr == nil && !disco.IsZero() && err == nil && addr.String() == b.VirtualAddress && netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(addr)
}
func strictDecode(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return ErrAuthority
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return ErrAuthority
	}
	return nil
}

// verify checks a signed configuration. restored is a configuration this authority verified before and
// persisted: its body hash is checked against the persisted one by the caller instead of the signature (the
// signing keys may be unreachable with Paperboat), and it is used past its exp (F21-01).
func (a *Authority) verify(ctx context.Context, token string, now time.Time, restored bool) (NetworkConfiguration, string, error) {
	var cfg NetworkConfiguration
	if len(token) > MaxConfigurationBytes {
		return cfg, "", ErrAuthority
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return cfg, "", ErrAuthority
	}
	decode := func(s string) ([]byte, error) {
		b, e := base64.RawURLEncoding.Strict().DecodeString(s)
		if e != nil || base64.RawURLEncoding.EncodeToString(b) != s {
			return nil, ErrAuthority
		}
		return b, nil
	}
	header, e1 := decode(parts[0])
	body, e2 := decode(parts[1])
	sig, e3 := decode(parts[2])
	var h struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}
	if e1 != nil || e2 != nil || e3 != nil || len(header) > 1024 || strictDecode(header, &h) != nil || h.Algorithm != "EdDSA" || h.Type != "paperboat-network-config+jwt" || !validID(h.KeyID) {
		return cfg, "", ErrAuthority
	}
	if !restored {
		pub, found, err := a.options.Keys.Lookup(ctx, h.KeyID)
		if err != nil || !found || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
			return cfg, "", ErrAuthority
		}
	}
	if strictDecode(body, &cfg) != nil {
		return cfg, "", ErrAuthority
	}
	if cfg.Version != 1 || cfg.Issuer != a.options.Issuer || cfg.Audience != "paperboat-network" || cfg.Generation == 0 || cfg.Generation > 1<<53-1 || cfg.IssuedAt <= 0 || cfg.IssuedAt > now.Add(derpquic.MaxClockSkew).Unix() || cfg.ExpiresAt <= cfg.IssuedAt || !validBinding(cfg.Self) || len(cfg.Peers) > MaxConfigurationPeers {
		return cfg, "", ErrAuthority
	}
	if cfg.ExpiresAt <= now.Unix() && !restored {
		return cfg, "", ErrExpiredAuthority
	}
	s, want := cfg.Self, a.options.Self
	if s.AccountID != want.AccountID || s.EndpointID != want.EndpointID || s.Role != want.Role || s.MachineID != want.MachineID || s.EndpointGeneration != want.EndpointGeneration || s.MachineGeneration != want.MachineGeneration || s.QUICCertificateFingerprint != want.QUICCertificateFingerprint || s.QUICPublicKey != want.QUICPublicKey {
		return cfg, "", ErrAuthority
	}
	ids := map[string]bool{s.EndpointID: true}
	addresses := map[string]bool{s.VirtualAddress: true}
	keys := map[string]bool{s.WireGuardPublicKey: true}
	scopes := 0
	for _, p := range cfg.Peers {
		b := p.Identity
		if !validBinding(b) || (s.Role == "cli" && b.Role == "cli") || ids[b.EndpointID] || addresses[b.VirtualAddress] || keys[b.WireGuardPublicKey] || len(p.Scopes) == 0 || !validPeerEndpoints(p.Endpoints) {
			return cfg, "", ErrAuthority
		}
		ids[b.EndpointID] = true
		addresses[b.VirtualAddress] = true
		keys[b.WireGuardPublicKey] = true
		seen := map[string]bool{}
		for _, scope := range p.Scopes {
			scopes++
			networkOnly := scope.ResourceKind == "machine_network" && scope.Capability == "connect" && b.AccountID == s.AccountID
			validResource := networkOnly || scope.ResourceKind == "config_compare" && scope.Capability == "config_compare" || scope.ResourceKind == "inspector" && scope.Capability == "inspector" || scope.ResourceKind == "machine_access" && (scope.Capability == "terminal" || scope.Capability == "exec" || scope.Capability == "managed_ssh" || scope.Capability == "file_transfer" || scope.Capability == "private_access") || scope.ResourceKind == "codex_session" && scope.Capability == "codex"
			// Until-revoked scopes (expires_at 0, F21-01/F21-11): the network pair, and a host's offline session
			// rights (machine_access accept), whose generation is the deciding policy version.
			sessionRight := scope.ResourceKind == "machine_access" && scope.Direction == "accept" && scope.Capability != "private_access"
			validGeneration := scope.ResourceGeneration == 1 || sessionRight && scope.ResourceGeneration >= 1 && scope.ResourceGeneration <= 1<<53-1
			if scopes > MaxConfigurationScopes || !validResource || !validID(scope.ResourceID) || !validGeneration || scope.Port != NetworkPort || scope.ExpiresAt < 0 || scope.ExpiresAt == 0 && !networkOnly && !sessionRight || scope.Direction != "dial" && scope.Direction != "accept" || s.Role == "cli" && scope.Direction != "dial" || !networkOnly && (s.Role == b.Role || s.Role == "machine" && scope.Direction != "accept") {
				return cfg, "", ErrAuthority
			}
			k := scope.ResourceID + "\x00" + scope.Capability + "\x00" + scope.Direction
			if seen[k] {
				return cfg, "", ErrAuthority
			}
			seen[k] = true
		}
	}
	hash := sha256.Sum256(body)
	return cfg, hex.EncodeToString(hash[:]), nil
}

// PrepareKey returns only the public registration material. Repeated calls reuse
// a pending key, so loss of a registration response cannot lose private custody.
func (a *Authority) PrepareKey(rotate bool) (string, uint64, error) {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return "", 0, ErrAuthority
	}
	var public string
	var generation uint64
	err := a.state(func(s *config.PeerNetworkState) error {
		if s.KeyGeneration >= 1<<53-1 {
			return ErrAuthority
		}
		generation = s.KeyGeneration
		raw := s.PrivateKey
		if rotate || len(raw) == 0 || len(s.PendingKey) != 0 {
			if len(s.PendingKey) == 0 {
				k := key.NewNode().Raw32()
				s.PendingKey = append([]byte(nil), k[:]...)
				clear(k[:])
			}
			raw = s.PendingKey
		}
		k, err := privateKey(raw)
		if err != nil {
			return err
		}
		bytes := k.Public().Raw32()
		public = base64.RawURLEncoding.EncodeToString(bytes[:])
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return public, generation, nil
}
func (a *Authority) state(update func(*config.PeerNetworkState) error) error {
	return a.options.Store.UpdatePeerNetworkState(a.options.Issuer, a.options.Self.AccountID, a.options.Self.EndpointID, update)
}

// Apply validates and persists authority before changing traffic admission.
// RelayDenial reports why the latest refresh withheld hosted relay grants (the workspace
// allowance is used up), or nil. Direct connections keep working; only relayed paths stop.
func (a *Authority) RelayDenial() *api.APIError {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.relayDenied
}

func (a *Authority) setRelayDenial(denial *api.APIError) {
	a.mu.Lock()
	a.relayDenied = denial
	a.mu.Unlock()
}

func (a *Authority) Apply(ctx context.Context, token string) error {
	if ctx == nil {
		return ErrAuthority
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, hash, err := a.verify(ctx, token, time.Now(), false)
	if err != nil {
		return err
	}
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return ErrAuthority
	}
	var selected key.NodePrivate
	err = a.state(func(s *config.PeerNetworkState) error {
		if cfg.Generation < s.Generation {
			return ErrStaleAuthority
		}
		if cfg.Generation == s.Generation && hash != s.ConfigHash {
			return ErrAuthority
		}
		if s.VirtualAddress != "" && s.VirtualAddress != cfg.Self.VirtualAddress {
			return ErrAuthority
		}
		match := func(raw []byte) bool {
			k, e := privateKey(raw)
			if e != nil {
				return false
			}
			pub := k.Public().Raw32()
			if base64.RawURLEncoding.EncodeToString(pub[:]) != cfg.Self.WireGuardPublicKey {
				return false
			}
			selected = k
			return true
		}
		if cfg.Self.KeyGeneration == s.KeyGeneration && match(s.PrivateKey) {
		} else if cfg.Self.KeyGeneration == s.KeyGeneration+1 && match(s.PendingKey) {
			clear(s.PrivateKey)
			s.PrivateKey = append([]byte(nil), s.PendingKey...)
			clear(s.PendingKey)
			s.PendingKey = nil
			s.KeyGeneration = cfg.Self.KeyGeneration
		} else {
			return ErrAuthority
		}
		s.Generation = cfg.Generation
		s.ConfigHash = hash
		s.VirtualAddress = cfg.Self.VirtualAddress
		return nil
	})
	if err == nil {
		// Kept for a restart without Paperboat (its hash is now custody's); a failed write loses only that.
		if saveErr := a.options.Store.SavePeerNetworkConfiguration(a.options.Issuer, a.options.Self.AccountID, a.options.Self.EndpointID, token); saveErr != nil {
			diagnosticlog.TryWarn("peer network configuration was not kept for an offline restart", "error", saveErr)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAuthority
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrStaleAuthority) {
		return err
	}
	if err != nil {
		a.dropLocked()
		return err
	}
	if time.Now().Unix() >= cfg.ExpiresAt {
		return ErrExpiredAuthority
	}
	return a.installLocked(cfg, selected, time.Now())
}

func (a *Authority) installLocked(cfg NetworkConfiguration, selected key.NodePrivate, verifiedAt time.Time) error {
	if a.current != nil && a.current.Self != cfg.Self {
		a.dropLocked()
	}
	a.expired = false
	a.current = &cfg
	a.lastVerified = verifiedAt
	a.expiresAt.Store(cfg.ExpiresAt)
	select {
	case a.refreshWake <- struct{}{}:
	default:
	}
	a.private = selected
	if err := a.replaceLocked(); err != nil {
		a.dropLocked()
		return err
	}
	return nil
}

var errRestoreReadOnly = errors.New("peer network state read only")

// ErrNothingToRestore is Restore's answer when no verified configuration was persisted.
var ErrNothingToRestore = errors.New("no verified peer network configuration to restore")

// Restore installs the configuration this authority last verified and persisted, so a daemon restarted while
// Paperboat is unreachable keeps its peers (F21-01, offline until revoked). It is the exact persisted document:
// same body hash, generation, binding and key as the state it was installed with. A configuration already in
// force is kept. The next successful refresh replaces it as usual.
func (a *Authority) Restore(ctx context.Context) error {
	token, err := a.options.Store.LoadPeerNetworkConfiguration(a.options.Issuer, a.options.Self.AccountID, a.options.Self.EndpointID, MaxConfigurationBytes)
	if err != nil {
		return err
	}
	var hash, address string
	var raw []byte
	var generation, keyGeneration uint64
	err = a.state(func(s *config.PeerNetworkState) error {
		hash, address, generation, keyGeneration = s.ConfigHash, s.VirtualAddress, s.Generation, s.KeyGeneration
		raw = append([]byte(nil), s.PrivateKey...)
		return errRestoreReadOnly
	})
	defer clear(raw)
	if !errors.Is(err, errRestoreReadOnly) {
		return err
	}
	if token == "" {
		return ErrNothingToRestore
	}
	cfg, got, err := a.verify(ctx, token, time.Now(), true)
	if err != nil || got != hash || cfg.Generation != generation || cfg.Self.VirtualAddress != address || cfg.Self.KeyGeneration != keyGeneration || cfg.Self != a.options.Self {
		return errors.Join(ErrAuthority, err)
	}
	selected, err := privateKey(raw)
	if err != nil {
		return err
	}
	if pub := selected.Public().Raw32(); base64.RawURLEncoding.EncodeToString(pub[:]) != cfg.Self.WireGuardPublicKey {
		return ErrAuthority
	}
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAuthority
	}
	if a.current != nil {
		return nil
	}
	return a.installLocked(cfg, selected, time.Unix(cfg.IssuedAt, 0))
}

func (a *Authority) dropLocked() {
	a.expired = false
	if a.stunTimer != nil {
		a.stunTimer.Stop()
		a.stunTimer = nil
	}
	a.expiresAt.Store(0)
	a.relay.mu.Lock()
	recovery := a.relay.recovery
	a.relay.recovery = nil
	a.relay.tokens = nil
	a.relay.grants = nil
	a.relay.usagePeers = nil
	a.relay.mu.Unlock()
	if recovery != nil {
		recovery.stop()
	}
	if a.server != nil {
		_ = a.server.Close()
		a.server = nil
	}
	for id, lease := range a.clients {
		_ = lease.client.Close()
		delete(a.clients, id)
	}
	if a.clientEngine != nil {
		a.clientEngine.Close()
		a.clientEngine = nil
	}
	a.current = nil
	a.notifyScopeChangesLocked()
	a.private = key.NodePrivate{}
}
func (a *Authority) Close() error {
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		close(a.done)
		a.dropLocked()
	}
	a.mu.Unlock()
	a.recoveryWorkers.Wait()
	return nil
}

// OutOfSync reports whether the configuration in force is past its refresh hint: Paperboat has not answered
// since, and the daemon is running on its last verified state (pb status/doctor "out of sync with Paperboat").
func (a *Authority) OutOfSync() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current != nil && time.Now().Unix() >= a.current.ExpiresAt
}

// LastVerifiedAt is when the configuration in force was verified and installed; zero without one.
func (a *Authority) LastVerifiedAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil {
		return time.Time{}
	}
	return a.lastVerified
}

func (a *Authority) usableLocked() bool {
	if a.current == nil {
		return false
	}
	// Past exp the last verified configuration stays in force while Paperboat is unreachable (offline until
	// revoked, F21-01): scopes end at their own expiry or by a newer configuration, never by the refresh hint.
	a.expired = time.Now().Unix() >= a.current.ExpiresAt
	return true
}

// Allows is the network scope check; it does not replace operation-token checks.
func (a *Authority) Allows(peerID, resourceID, capability, direction string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.usableLocked() {
		return false
	}
	for _, p := range a.current.Peers {
		if p.Identity.EndpointID == peerID {
			for _, s := range p.Scopes {
				if s.ResourceKind != "machine_network" && s.ResourceID == resourceID && s.Capability == capability && s.Direction == direction && scopeLive(s, time.Now().Unix()) {
					return true
				}
			}
		}
	}
	return false
}

// Self returns the currently authorized local binding without exposing the
// mutable configuration document. Callers must treat failure as revocation.
func (a *Authority) Self() (NetworkBinding, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.usableLocked() {
		return NetworkBinding{}, ErrAuthority
	}
	return a.current.Self, nil
}

// Peer returns one currently authorized peer binding. The returned value is a
// copy; every subsequent operation must still pass Allows at its own boundary.
func (a *Authority) Peer(endpointID string) (NetworkBinding, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.usableLocked() {
		return NetworkBinding{}, ErrAuthority
	}
	for _, peer := range a.current.Peers {
		if peer.Identity.EndpointID == endpointID && peerAdmissionExpiry(peer) > time.Now().Unix() {
			return peer.Identity, nil
		}
	}
	return NetworkBinding{}, ErrAdmission
}

// PeerAt resolves the signed identity assigned to an accepted virtual address.
func (a *Authority) PeerAt(address netip.Addr) (NetworkBinding, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.usableLocked() || !address.IsValid() {
		return NetworkBinding{}, ErrAuthority
	}
	for _, peer := range a.current.Peers {
		if peer.Identity.VirtualAddress == address.String() {
			return peer.Identity, nil
		}
	}
	return NetworkBinding{}, ErrAdmission
}

// AllowsAdmitted checks explicit resource withdrawal without reapplying the
// admission credential's lifetime to work that has already been accepted.
func (a *Authority) AllowsAdmitted(peerID, resourceID, capability, direction string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.current == nil {
		return false
	}
	for _, p := range a.current.Peers {
		if p.Identity.EndpointID != peerID {
			continue
		}
		for _, scope := range p.Scopes {
			if scope.ResourceKind != "machine_network" && scope.ResourceID == resourceID && scope.Capability == capability && scope.Direction == direction {
				return true
			}
		}
	}
	return false
}
