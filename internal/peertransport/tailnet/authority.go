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
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/tailscale/tailcat"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=network-key-custody
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=network-fault-injection
	"tailscale.com/types/nettype"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=network-fault-injection
	"tailscale.com/wgengine/magicsock"
)

const MaxConfigurationBytes = 128 << 10
const ConfigurationTTL = 5 * time.Minute
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
}
type RelayPair struct {
	ResourceKind       string         `json:"resource_kind"`
	ResourceID         string         `json:"resource_id"`
	ResourceGeneration uint64         `json:"resource_generation"`
	First              NetworkBinding `json:"first"`
	Second             NetworkBinding `json:"second"`
	ExpiresAt          int64          `json:"expires_at"`
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
	RelayPairs []RelayPair    `json:"relay_pairs,omitempty"`
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
	mu              sync.Mutex
	updateMu        sync.Mutex // custody I/O must never hold the expiry/lifecycle lock
	options         AuthorityOptions
	current         *NetworkConfiguration
	private         key.NodePrivate
	timer           *time.Timer
	closed          bool
	done            chan struct{}
	server          *UDPServer
	clientEngine    *tailcat.Server
	clients         map[string]authorizedClient
	clientSlots     chan struct{}
	regional        *RegionalCandidates
	relay           relayAuthority
	recoveryWorkers sync.WaitGroup
	refreshSlot     chan struct{}
}

type authorizedClient struct {
	client     *UDPClient
	peer       NetworkBinding
	descriptor [sha256.Size]byte
	scopes     []NetworkScope
	node       key.NodePublic
}

func (a *Authority) EligibleRegionalNodes(role, transport string, allowedRegions map[string]bool, now time.Time) ([]RegionalNode, Redundancy, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.regional == nil || a.closed {
		return nil, RedundancyNone, ErrRegionalAuthority
	}
	return a.regional.Eligible(role, transport, allowedRegions, now)
}

func NewAuthority(options AuthorityOptions) (*Authority, error) {
	issuer, err := config.NormalizeIssuer(options.Issuer)
	if err != nil || issuer != options.Issuer || options.Keys == nil || !validLocalBinding(options.Self) {
		return nil, ErrAuthority
	}
	return &Authority{options: options, done: make(chan struct{}), refreshSlot: make(chan struct{}, 1)}, nil
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

func (a *Authority) verify(ctx context.Context, token string, now time.Time) (NetworkConfiguration, string, error) {
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
	pub, found, err := a.options.Keys.Lookup(ctx, h.KeyID)
	if err != nil || !found || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) || strictDecode(body, &cfg) != nil {
		return cfg, "", ErrAuthority
	}
	if cfg.Version != 1 || cfg.Issuer != a.options.Issuer || cfg.Audience != "paperboat-network" || cfg.Generation == 0 || cfg.Generation > 1<<53-1 || cfg.IssuedAt <= 0 || cfg.IssuedAt > now.Unix() || cfg.ExpiresAt <= cfg.IssuedAt || cfg.ExpiresAt-cfg.IssuedAt > int64(ConfigurationTTL/time.Second) || !validBinding(cfg.Self) || len(cfg.Peers) > MaxFlows || len(cfg.RelayPairs) > 16 {
		return cfg, "", ErrAuthority
	}
	if cfg.ExpiresAt <= now.Unix() {
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
		if !validBinding(b) || b.Role == s.Role || ids[b.EndpointID] || addresses[b.VirtualAddress] || keys[b.WireGuardPublicKey] || len(p.Scopes) == 0 {
			return cfg, "", ErrAuthority
		}
		ids[b.EndpointID] = true
		addresses[b.VirtualAddress] = true
		keys[b.WireGuardPublicKey] = true
		seen := map[string]bool{}
		for _, scope := range p.Scopes {
			scopes++
			validResource := scope.ResourceKind == "inspector" && scope.Capability == "inspector" || scope.ResourceKind == "machine_access" && (scope.Capability == "terminal" || scope.Capability == "exec" || scope.Capability == "managed_ssh" || scope.Capability == "file_transfer" || scope.Capability == "private_access") || scope.ResourceKind == "codex_session" && scope.Capability == "codex"
			if scopes > 128 || !validResource || !validID(scope.ResourceID) || scope.ResourceGeneration != 1 || scope.Port != NetworkPort || scope.ExpiresAt < cfg.ExpiresAt || scope.Direction != "dial" && scope.Direction != "accept" || s.Role == "cli" && scope.Direction != "dial" || s.Role == "machine" && scope.Direction != "accept" {
				return cfg, "", ErrAuthority
			}
			k := scope.ResourceID + "\x00" + scope.Capability
			if seen[k] {
				return cfg, "", ErrAuthority
			}
			seen[k] = true
		}
	}
	pairs := map[string]bool{}
	for _, pair := range cfg.RelayPairs {
		first, second := pair.First, pair.Second
		pairID := first.DiscoPublicKey + "\x00" + second.DiscoPublicKey
		if s.Role != "machine" || pair.ResourceKind != "machine_access" || !validID(pair.ResourceID) || pair.ResourceGeneration != 1 || pair.ExpiresAt < cfg.ExpiresAt || !validBinding(first) || !validBinding(second) || first.AccountID != s.AccountID || second.AccountID != s.AccountID || first.EndpointID == second.EndpointID || first.EndpointID == s.EndpointID || second.EndpointID == s.EndpointID || first.Role != "cli" || second.Role != "machine" || pairs[pairID] {
			return cfg, "", ErrAuthority
		}
		pairs[pairID] = true
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
func (a *Authority) Apply(ctx context.Context, token string) error {
	if ctx == nil {
		return ErrAuthority
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, hash, err := a.verify(ctx, token, time.Now())
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
		if cfg.Generation < s.Generation || cfg.Generation == s.Generation && hash != s.ConfigHash {
			return ErrStaleAuthority
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAuthority
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		a.dropLocked()
		return err
	}
	if time.Now().Unix() >= cfg.ExpiresAt {
		a.dropLocked()
		return ErrExpiredAuthority
	}
	if a.current != nil && a.current.Self != cfg.Self {
		a.dropLocked()
	}
	a.current = &cfg
	a.private = selected
	a.relay.mu.Lock()
	device, addresses := a.relay.device, append([]netip.AddrPort(nil), a.relay.deviceAddresses...)
	a.relay.mu.Unlock()
	if len(cfg.RelayPairs) == 0 && device != nil {
		_ = device.close()
		a.relay.mu.Lock()
		if a.relay.device == device {
			a.relay.device = nil
		}
		a.relay.mu.Unlock()
	} else if device != nil {
		device.update(cfg.RelayPairs)
	} else if len(cfg.RelayPairs) != 0 && len(addresses) != 0 {
		if err := a.startDeviceRelayLocked(addresses); err != nil {
			a.dropLocked()
			return err
		}
	}
	if err := a.replaceLocked(); err != nil {
		a.dropLocked()
		return err
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	generation := cfg.Generation
	a.timer = time.AfterFunc(time.Until(time.Unix(cfg.ExpiresAt, 0)), func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current != nil && a.current.Generation == generation {
			a.dropLocked()
		}
	})
	return nil
}

func (a *Authority) dropLocked() {
	a.relay.mu.Lock()
	recovery := a.relay.recovery
	a.relay.recovery = nil
	a.relay.tokens = nil
	a.relay.grants = nil
	a.relay.mu.Unlock()
	if recovery != nil {
		recovery.stop()
	}
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
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
	a.relay.mu.Lock()
	if a.relay.device != nil {
		_ = a.relay.device.close()
		a.relay.device = nil
	}
	a.relay.mu.Unlock()
	a.current = nil
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
func (a *Authority) usableLocked() bool {
	if a.current == nil {
		return false
	}
	if time.Now().Unix() >= a.current.ExpiresAt {
		a.dropLocked()
		return false
	}
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
				if s.ResourceID == resourceID && s.Capability == capability && s.Direction == direction {
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
		if peer.Identity.EndpointID == endpointID {
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
