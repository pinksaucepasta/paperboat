package tailnet

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"net/netip"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/relay/nodetls"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-derp-carrier
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-derp-carrier
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-derp-carrier
	"tailscale.com/wgengine/magicsock"
)

type relayAuthority struct {
	mu     sync.Mutex
	tokens map[string]string
	grants map[string]peernetwork.RelayGrant
	// retained is the previous grant set, still presented to the relay while a configuration that only withdrew peers
	// is within its withdrawal grace (withdrawal.go): the narrowed grant would make the relay drop the withdrawn pair at
	// once and eat the closes of its sessions, which the grace keeps the path open for. Cleared when the last withdrawn
	// peer is cut. Guarded by mu.
	retained     *retainedRelayGrants
	config       *tls.Config
	node         RegionalNode
	factory      magicsock.DERPCarrierFactory
	peerNodes    []*tailcfg.Node
	controlPeers []key.NodePublic
	nodes        map[tailcfg.DERPRegionID]RegionalNode
	// listed is the last relay each region was listed with. magicsock builds a region's carrier lazily and keeps
	// it while the region is unchanged, so a carrier built while its relay was momentarily missing from nodes
	// took an empty relay (no address, no pin) for good (pbmac 2026-11-05). Written by setRelayNodesLocked.
	listed          map[tailcfg.DERPRegionID]RegionalNode
	usagePeers      map[key.NodePublic]*relayUsageObservation
	generationUsage map[[32]byte]map[key.NodePublic]*relayUsageObservation
	// hostedData holds one channel per peer that is closed by the next WireGuard data crossing a
	// relay that is not self-hosted (PathEligibility).
	hostedData map[key.NodePublic]chan struct{}
	recovery   *regionalRecovery
	// regionalRecoveryConfigured distinguishes dynamic regional recovery from
	// the single fixed-node mode when a stopped worker has been detached.
	regionalRecoveryConfigured bool
}

// ApplyRelayGrants consumes only signed grants from the same network refresh.
// Invalid or absent grants remove cached relay authority; they cannot extend a lease. A grant relays would refuse
// is left out and the others apply; when none applies it returns ErrAuthority.
func (a *Authority) ApplyRelayGrants(ctx context.Context, tokens []string) (resultErr error) {
	a.mu.Lock()
	if a.current == nil || !a.usableLocked() {
		a.mu.Unlock()
		return ErrAuthority
	}
	cfg := *a.current
	regional := a.regional
	a.mu.Unlock()
	next := map[string]string{}
	grants := map[string]peernetwork.RelayGrant{}
	controlSet := map[key.NodePublic]bool{}
	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current == nil || a.current.Generation != cfg.Generation {
			next = nil
			grants = nil
			resultErr = ErrStaleAuthority
		}
		a.relay.mu.Lock()
		removed, added := relayGrantDiff(a.relay.grants, grants)
		moved := relaysMoved(a.relay.grants, grants)
		if added {
			a.relay.retained = nil // a grant that adds a pair is never held back
		} else if removed && len(a.withdrawing) > 0 && a.relay.retained == nil {
			a.relay.retained = &retainedRelayGrants{tokens: a.relay.tokens, grants: a.relay.grants}
		}
		a.relay.tokens = next
		a.relay.grants = grants
		a.relay.controlPeers = a.relay.controlPeers[:0]
		for peer := range controlSet {
			a.relay.controlPeers = append(a.relay.controlPeers, peer)
		}
		controlPeers := append([]key.NodePublic(nil), a.relay.controlPeers...)
		if recovery := a.relay.recovery; recovery != nil && resultErr == nil && moved {
			recovery.relaysMoved()
		}
		if recovery := a.relay.recovery; recovery != nil && resultErr == nil && (recovery.pair() != "" || recovery.repair.Load()) {
			// Tailscale re-STUNs at once on a DERP map update; do not wait out the probe delay.
			select {
			case recovery.grantWake <- struct{}{}:
			default:
			}
		}
		a.relay.mu.Unlock()
		a.notifyScopeChangesLocked()
		if a.server != nil {
			_ = a.server.server.SetRelayControlPeers(controlPeers)
		}
		if a.clientEngine != nil {
			_ = a.clientEngine.SetRelayControlPeers(controlPeers)
		}
	}()
	if len(tokens) > peernetwork.MaxRegionalCandidates*peernetwork.MaxGrantParts || regional == nil {
		return ErrRegionalAuthority
	}
	nodes, _, err := eligible(regional, "relay", "derp_quic", nil, serverclock.Now())
	if err != nil {
		return err
	}
	for _, token := range tokens {
		parts := strings.Split(token, ".")
		if len(parts) != 3 || len(token) > MaxConfigurationBytes {
			next = nil
			grants = nil
			return ErrAuthority
		}
		header, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
		var h struct {
			KeyID string `json:"kid"`
		}
		if err != nil || json.Unmarshal(header, &h) != nil {
			next = nil
			grants = nil
			return ErrAuthority
		}
		body, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
		var g peernetwork.RelayGrant
		if err != nil || peernetwork.DecodeSigned(body, &g) != nil {
			next = nil
			grants = nil
			return ErrAuthority
		}
		var node *RegionalNode
		for i := range nodes {
			if nodes[i].NodeID == g.NodeID {
				node = &nodes[i]
				break
			}
		}
		if node == nil {
			next = nil
			grants = nil
			return ErrRegionalAuthority
		}
		pub, found, err := a.options.Keys.Lookup(ctx, h.KeyID)
		if err != nil || !found {
			next = nil
			grants = nil
			return ErrAuthority
		}
		// The real certificate is required when configuring the carrier. Before that,
		// verify the signature and all claims here, then compare its signed fingerprint
		// with the already approved local binding.
		if len(pub) != 32 || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), mustDecodeRelaySignature(parts[2])) || h.KeyID == "" {
			next = nil
			grants = nil
			return ErrAuthority
		}
		var fullHeader struct {
			Algorithm string `json:"alg"`
			Type      string `json:"typ"`
			KeyID     string `json:"kid"`
		}
		g.DropUnknownScopes()
		if peernetwork.DecodeSigned(header, &fullHeader) != nil || fullHeader.Algorithm != "EdDSA" || fullHeader.Type != peernetwork.RelayGrantType || g.Issuer != a.options.Issuer || g.AccountID != cfg.Self.AccountID || g.EndpointID != cfg.Self.EndpointID || g.WireGuardPublicKey != cfg.Self.WireGuardPublicKey || g.CertificateFingerprint != cfg.Self.QUICCertificateFingerprint || g.QUICPublicKey != cfg.Self.QUICPublicKey || g.Generation != cfg.Generation || g.NodeGeneration != node.NodeGeneration || g.ProcessEpoch != node.ProcessEpoch || g.ExpiresAt <= serverclock.Now().Unix() {
			next = nil
			grants = nil
			return ErrAuthority
		}
		// A grant relays would refuse makes only its relay unusable; the others still apply.
		if err := g.Check(serverclock.Now()); err != nil {
			diagnosticlog.TryWarn("a relay grant relays would refuse is not used", "node", g.NodeID, "error", err)
			continue
		}
		// A grant names at most a bounded subset of this endpoint's peers (the relay's per-grant
		// bound); every peer it names must be a configured peer with exactly its configured scopes.
		if len(g.Peers) > len(cfg.Peers) {
			next = nil
			grants = nil
			return ErrAuthority
		}
		for _, peerKey := range g.RelayControlPeers {
			peer, err := publicKey(peerKey)
			if err != nil {
				next = nil
				grants = nil
				return ErrAuthority
			}
			controlSet[peer] = true
		}
		// A membership grant names no peers: each membership must be a scope this configuration gives the
		// endpoint (same resource and direction), and its account mesh membership makes every configured peer
		// of the account a relay-control peer, as the relay itself decides (derpquic membership.go).
		if len(g.Memberships) > 0 {
			if len(g.Peers)+len(g.RelayControlPeers) > 0 || !membershipsConfigured(g.Memberships, cfg) {
				next = nil
				grants = nil
				return ErrAuthority
			}
			for _, m := range g.Memberships {
				if m.ResourceKind != "machine_network" {
					continue
				}
				for _, p := range cfg.Peers {
					if peer, err := publicKey(p.Identity.WireGuardPublicKey); err == nil && p.Identity.AccountID == cfg.Self.AccountID {
						controlSet[peer] = true
					}
				}
				break
			}
		}
		// An endpoint with more peers than one grant names gets one grant per chunk of peers for a node;
		// together they name each configured peer at most once.
		earlier, chunk := grants[g.NodeID], next[g.NodeID] != ""
		if chunk && (len(strings.Split(next[g.NodeID], "\n")) >= derpquic.MaxGrantParts || earlier.ExpiresAt != g.ExpiresAt || earlier.IssuedAt != g.IssuedAt) {
			next = nil
			grants = nil
			return ErrAuthority
		}
		configured := make(map[string]string, len(cfg.Peers))
		for _, peer := range cfg.Peers {
			// Until-revoked scopes (expires_at 0, F21-01) differ in a grant: relays and the mint refuse peer scopes
			// without expiry, so a kept scope appears with the grant's own expiry and the rest is left out
			// (peernetwork.RelayGrantKeepsStanding, the rule the server applies). A peer with nothing kept is not
			// named. Everything else must match exactly.
			mesh := slices.ContainsFunc(peer.Scopes, func(s NetworkScope) bool { return s.ResourceKind == "machine_network" })
			var scopes []NetworkScope
			for _, scope := range peer.Scopes {
				if scope.ExpiresAt == 0 {
					if !peernetwork.RelayGrantKeepsStanding(scope, mesh) {
						continue
					}
					scope.ExpiresAt = g.ExpiresAt
				}
				scopes = append(scopes, scope)
			}
			if len(scopes) == 0 {
				continue
			}
			encoded, _ := json.Marshal(scopes)
			configured[peer.Identity.WireGuardPublicKey] = string(encoded)
		}
		seenPeer := make(map[string]bool, len(g.Peers)+len(earlier.Peers))
		if chunk {
			for _, p := range earlier.Peers {
				seenPeer[p.WireGuardPublicKey] = true
			}
		}
		for _, p := range g.Peers {
			expected, ok := configured[p.WireGuardPublicKey]
			actual, _ := json.Marshal(p.Scopes)
			if !ok || seenPeer[p.WireGuardPublicKey] || expected != string(actual) {
				next = nil
				grants = nil
				return ErrAuthority
			}
			seenPeer[p.WireGuardPublicKey] = true
		}
		if chunk {
			merged := earlier
			merged.Peers = append(append([]peernetwork.RelayPeer(nil), earlier.Peers...), g.Peers...)
			merged.RelayControlPeers = append(append([]string(nil), earlier.RelayControlPeers...), g.RelayControlPeers...)
			merged.Memberships = append(append([]derpquic.Scope(nil), earlier.Memberships...), g.Memberships...)
			if merged.PeerRelay == nil {
				merged.PeerRelay = g.PeerRelay
			}
			next[g.NodeID] += "\n" + token
			grants[g.NodeID] = merged
			continue
		}
		next[g.NodeID] = token
		grants[g.NodeID] = g
	}
	if len(tokens) > 0 && len(grants) == 0 {
		return ErrAuthority // every grant was refused; the empty set is still installed
	}
	return nil
}
func mustDecodeRelaySignature(s string) []byte {
	b, _ := base64.RawURLEncoding.Strict().DecodeString(s)
	return b
}

// ConfigureRelay is the single-candidate form of ConfigureRegionalRelays.
func (a *Authority) ConfigureRelay(nodeID string, config *tls.Config) (*tailcfg.DERPRegion, error) {
	regions, err := a.configureRegionalRelays(config, nodeID)
	if err != nil {
		return nil, err
	}
	return regions[0], nil
}

// ConfigureRegionalRelays installs only verified relay candidates. The factory
// resolves each magicsock region independently; it never recreates an endpoint
// engine or selects native DERP/TCP. Call before opening the endpoint engine.
func (a *Authority) ConfigureRegionalRelays(config *tls.Config) ([]*tailcfg.DERPRegion, error) {
	return a.configureRegionalRelays(config, "")
}
func regionalID(node string) tailcfg.DERPRegionID {
	digest := sha256.Sum256([]byte(node))
	return tailcfg.DERPRegionID(1 + uint32(binary.BigEndian.Uint16(digest[:2]))%65535)
}
func regionForNode(node RegionalNode) *tailcfg.DERPRegion {
	id := regionalID(node.NodeID)
	return &tailcfg.DERPRegion{RegionID: id, RegionCode: node.Region, RegionName: fmt.Sprintf("%s/%d/%s", node.NodeID, node.NodeGeneration, node.ProcessEpoch), Nodes: []*tailcfg.DERPNode{{Name: node.NodeID, RegionID: id, HostName: node.EndpointHost, DERPPort: int(node.EndpointQUICPort), STUNPort: int(node.EndpointTCPPort)}}}
}
func (a *Authority) configureRegionalRelays(config *tls.Config, only string) ([]*tailcfg.DERPRegion, error) {
	if config == nil || len(config.Certificates) != 1 || len(config.Certificates[0].Certificate) != 1 {
		return nil, ErrAuthority
	}
	cert, err := x509.ParseCertificate(config.Certificates[0].Certificate[0])
	if err != nil {
		return nil, ErrAuthority
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.usableLocked() || a.server != nil || a.clientEngine != nil {
		return nil, ErrAuthority
	}
	// A restored network can serve direct peers before fresh relay discovery.
	// No regional node is usable until signed candidates and grants arrive.
	var nodes []RegionalNode
	if a.regional != nil {
		nodes, _, err = eligible(a.regional, "relay", "derp_quic", nil, serverclock.Now())
		if err != nil {
			return nil, err
		}
	} else if only != "" {
		return nil, ErrRegionalAuthority
	}
	a.relay.mu.Lock()
	defer a.relay.mu.Unlock()
	selected := make(map[tailcfg.DERPRegionID]RegionalNode)
	controlSet := map[key.NodePublic]bool{}
	var regions []*tailcfg.DERPRegion
	var services []*tailcfg.Node
	for _, node := range nodes {
		if only != "" && only != node.NodeID {
			continue
		}
		if node.EndpointQUICPort == 0 {
			continue
		}
		g, ok := a.relay.grants[node.NodeID]
		if !ok || g.ExpiresAt <= serverclock.Now().Unix() {
			continue
		}
		// A grant the relay would refuse makes only that relay unusable: the engine still starts with the
		// others, or direct-only, instead of failing the whole runtime (a refused grant once stopped pairing).
		if e := a.verifyRelayGrantsLocked(node, cert); e != nil {
			if only != "" {
				return nil, e
			}
			diagnosticlog.TryWarn("a relay grant would be refused; that relay is not used until the next grant", "node", node.NodeID, "error", e)
			continue
		}
		for _, value := range g.RelayControlPeers {
			peer, parseErr := publicKey(value)
			if parseErr != nil {
				return nil, parseErr
			}
			controlSet[peer] = true
		}
		id := regionalID(node.NodeID)
		if _, exists := selected[id]; exists {
			return nil, errors.New("regional node identifier collision")
		}
		service, e := a.relayServiceLocked(node, g)
		if e != nil {
			return nil, e
		}
		if service != nil {
			services = append(services, service)
		}
		selected[id] = node
		regions = append(regions, regionForNode(node))
	}
	if len(regions) == 0 && only != "" {
		return nil, ErrRegionalAuthority
	}
	a.setRelayNodesLocked(selected)
	a.relay.updateUsagePeersLocked(a.current.Peers)
	a.relay.node = RegionalNode{}
	if len(regions) != 0 {
		a.relay.node = selected[regions[0].RegionID]
	}
	a.relay.peerNodes = services
	a.relay.controlPeers = a.relay.controlPeers[:0]
	for peer := range controlSet {
		a.relay.controlPeers = append(a.relay.controlPeers, peer)
	}
	sort.Slice(a.relay.controlPeers, func(i, j int) bool { return a.relay.controlPeers[i].Compare(a.relay.controlPeers[j]) < 0 })
	a.relay.config = config.Clone()
	if only == "" {
		a.relay.regionalRecoveryConfigured = true
		a.relay.recovery = a.newRegionalRecoveryLocked()
	}
	a.relay.factory = a.regionalCarrierFactory(config, [32]byte{}, key.DiscoPublic{})
	return regions, nil
}

// relayServiceLocked adapts a signed discovery service without granting it
// application access. a.mu is held by callers.
func (a *Authority) relayServiceLocked(node RegionalNode, g peernetwork.RelayGrant) (*tailcfg.Node, error) {
	if g.PeerRelay == nil {
		return nil, nil
	}
	ownDisco := mesh.DiscoPublicForNode(a.private).DiscoPublic
	if g.DiscoPublicKey != derpquic.DiscoKeyString(ownDisco) || !g.PeerRelay.Valid() {
		return nil, ErrAuthority
	}
	relayKey, e := derpquic.ParseKey(g.PeerRelay.WireGuardPublicKey)
	if e != nil {
		return nil, e
	}
	discoKey, e := derpquic.ParseDiscoKey(g.PeerRelay.DiscoPublicKey)
	if e != nil {
		return nil, e
	}
	if g.PeerRelay.VirtualAddress == a.current.Self.VirtualAddress || g.PeerRelay.WireGuardPublicKey == a.current.Self.WireGuardPublicKey {
		return nil, ErrAuthority
	}
	for _, p := range a.current.Peers {
		sameAddress := p.Identity.VirtualAddress == g.PeerRelay.VirtualAddress
		sameKey := p.Identity.WireGuardPublicKey == g.PeerRelay.WireGuardPublicKey
		if sameAddress || sameKey {
			if !sameAddress || !sameKey || p.Identity.DiscoPublicKey != g.PeerRelay.DiscoPublicKey {
				return nil, ErrAuthority
			}
		}
	}
	return &tailcfg.Node{ID: 1 << 60, StableID: tailcfg.StableNodeID(node.NodeID), Name: node.NodeID, Key: relayKey, DiscoKey: discoKey, Addresses: []netip.Prefix{netip.PrefixFrom(netip.MustParseAddr(g.PeerRelay.VirtualAddress), 128)}, HomeDERP: regionalID(node.NodeID), Cap: tailcfg.CurrentCapabilityVersion}, nil
}

// DiscoveryPublicKey returns Tailcat's deterministic discovery identity for the
// staged registration key, or the current committed key. Private bytes stay local.
func (a *Authority) DiscoveryPublicKey() (string, error) { return a.discoveryPublicKey("") }
func (a *Authority) discoveryPublicKey(expected string) (string, error) {
	var result string
	err := a.state(func(s *config.PeerNetworkState) error {
		raw := s.PrivateKey
		if len(s.PendingKey) != 0 {
			raw = s.PendingKey
		}
		private, err := privateKey(raw)
		if err != nil {
			return err
		}
		if expected != "" && derpquic.KeyString(private.Public()) != expected {
			return ErrStaleAuthority
		}
		result = derpquic.DiscoKeyString(mesh.DiscoPublicForNode(private).DiscoPublic)
		return nil
	})
	return result, err
}

// membershipsConfigured reports whether every membership of a relay grant is a scope the signed configuration
// gives this endpoint toward some peer: the same resource kind, id, generation, capability, port and direction.
func membershipsConfigured(memberships []derpquic.Scope, cfg NetworkConfiguration) bool {
	type tuple struct {
		kind, id, capability, direction string
		generation                      uint64
		port                            uint16
	}
	configured := make(map[tuple]bool)
	for _, p := range cfg.Peers {
		for _, s := range p.Scopes {
			configured[tuple{s.ResourceKind, s.ResourceID, s.Capability, s.Direction, s.ResourceGeneration, s.Port}] = true
		}
	}
	for _, m := range memberships {
		if !configured[tuple{m.ResourceKind, m.ResourceID, m.Capability, m.Direction, m.ResourceGeneration, m.Port}] {
			return false
		}
	}
	return true
}

// verifyRelayGrantsLocked checks every grant part of node exactly as the relay will.
func (a *Authority) verifyRelayGrantsLocked(node RegionalNode, cert *x509.Certificate) error {
	for _, token := range strings.Split(a.relay.tokens[node.NodeID], "\n") {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			return ErrAuthority
		}
		header, _ := base64.RawURLEncoding.DecodeString(parts[0])
		var h struct {
			KeyID string `json:"kid"`
		}
		_ = json.Unmarshal(header, &h)
		pub, found, err := a.options.Keys.Lookup(context.Background(), h.KeyID)
		if err != nil || !found {
			return ErrAuthority
		}
		verifier := derpquic.Verifier{Issuer: a.options.Issuer, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Keys: map[string]ed25519.PublicKey{h.KeyID: pub}}
		if _, err := verifier.Verify(token, tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}, serverclock.Now()); err != nil {
			return err
		}
	}
	return nil
}

// relayCredential is the grant a carrier for node presents on each dial. A new dial that meets an expired grant
// asks for a fresh one on demand (spaced and coalesced by Run) and fails retryably; the relay region is never
// locked out.
func (a *Authority) relayCredential(node RegionalNode) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		token, err := a.relayToken(node)
		if errors.Is(err, ErrRelayGrantExpired) || errors.Is(err, errRelayNotCurrent) {
			a.RequestRefresh()
		}
		return token, err
	}
}

// errRelayNotCurrent is a relay missing from this machine's current relay set, or a grant not yet issued for
// its current process: a refresh that landed in a heartbeat gap, a relay marked draining or full, or a set that
// expired before its refresh. derpquic latches every credential error except ErrExpired for the carrier's
// lifetime and magicsock's reader stops on it, so this was a relay dead until a restart (pbmac 2026-11-05,
// F3-15 N13). As expired authority it is refetched and retried under magicsock's relay reconnect backoff, as
// Tailscale's DERP client redials after every failure (wgengine/magicsock/derp.go runDerpReader). A relay
// that is listed again under another identity stays refused: its region changes and gets a new carrier.
var errRelayNotCurrent = fmt.Errorf("relay is not in this machine's current relay set; waiting for fresh relay authority: %w", derpquic.ErrExpired)

// relayToken is node's current grant, refused when the relay moved or its grant does not match.
func (a *Authority) relayToken(node RegionalNode) (string, error) {
	a.relay.mu.Lock()
	defer a.relay.mu.Unlock()
	grant, tokens := a.relay.grants[node.NodeID], a.relay.tokens
	if kept := a.relay.retained; kept != nil {
		if old, ok := kept.grants[node.NodeID]; ok && old.ExpiresAt > serverclock.Now().Unix() {
			grant, tokens = old, kept.tokens
		}
	}
	current, listed := a.relay.nodes[regionalID(node.NodeID)]
	if node.NodeID == "" || listed && (current.NodeID != node.NodeID || current.NodeGeneration != node.NodeGeneration || current.ProcessEpoch != node.ProcessEpoch || current.TLSSPKISHA256 != node.TLSSPKISHA256) {
		return "", ErrAuthority // the relay moved: its region is redefined and magicsock builds a new carrier
	}
	if !listed || grant.NodeGeneration != node.NodeGeneration || grant.ProcessEpoch != node.ProcessEpoch {
		return "", errRelayNotCurrent
	}
	if grant.ExpiresAt <= serverclock.Now().Unix() {
		return "", ErrRelayGrantExpired
	}
	return tokens[node.NodeID], nil
}

type retainedRelayGrants struct {
	tokens map[string]string
	grants map[string]peernetwork.RelayGrant
}

// relaysMoved reports whether next grants a relay process previous did not: a relay added, restarted or moved.
func relaysMoved(previous, next map[string]peernetwork.RelayGrant) bool {
	for node, g := range next {
		if old, ok := previous[node]; !ok || old.NodeGeneration != g.NodeGeneration || old.ProcessEpoch != g.ProcessEpoch {
			return true
		}
	}
	return false
}

// relayGrantDiff reports whether next drops some peer scope of previous and whether it adds one; a renewal of the same
// scopes is neither.
func relayGrantDiff(previous, next map[string]peernetwork.RelayGrant) (removed, added bool) {
	scopes := func(grants map[string]peernetwork.RelayGrant) map[string]bool {
		set := map[string]bool{}
		for node, g := range grants {
			for _, peer := range g.Peers {
				for _, s := range peer.Scopes {
					set[fmt.Sprint(node, "|", peer.WireGuardPublicKey, "|", s.ResourceKind, "|", s.ResourceID, "|", s.ResourceGeneration, "|", s.Capability, "|", s.Direction, "|", s.Port)] = true
				}
			}
		}
		return set
	}
	before, after := scopes(previous), scopes(next)
	for k := range before {
		removed = removed || !after[k]
	}
	for k := range after {
		added = added || !before[k]
	}
	return removed, added
}

// setRelayNodesLocked installs the current relay set and remembers each region's relay; a.relay.mu held.
func (a *Authority) setRelayNodesLocked(nodes map[tailcfg.DERPRegionID]RegionalNode) {
	a.relay.nodes = nodes
	if len(a.relay.listed) > 2*MaxRegionalCandidates {
		a.relay.listed = nil // bounded: relays long gone are forgotten, the current set is kept below
	}
	if a.relay.listed == nil {
		a.relay.listed = make(map[tailcfg.DERPRegionID]RegionalNode, len(nodes))
	}
	for id, node := range nodes {
		a.relay.listed[id] = node
	}
}

func (a *Authority) regionalCarrierFactory(config *tls.Config, namespace [32]byte, disco key.DiscoPublic) magicsock.DERPCarrierFactory {
	return func(_ key.NodePrivate, region func() *tailcfg.DERPRegion) magicsock.DERPCarrier {
		r := region()
		a.relay.mu.Lock()
		var node RegionalNode
		if r != nil {
			node = a.relay.nodes[r.RegionID]
			// The relay this region was defined for, while it is momentarily unlisted; its dials then wait as
			// errRelayNotCurrent until it is listed again. Another identity under the same ID is not used.
			if listed := a.relay.listed[r.RegionID]; node.NodeID == "" && regionForNode(listed).RegionName == r.RegionName {
				node = listed
			}
		}
		a.relay.mu.Unlock()
		credential := a.relayCredential(node)
		nodeTLS, pinErr := nodetls.ClientConfig(config, node.TLSSPKISHA256)
		if pinErr != nil {
			// Signed candidates are validated on ingestion; fail closed if unavailable.
			nodeTLS = config.Clone()
			nodeTLS.VerifyConnection = func(tls.ConnectionState) error { return nodetls.ErrIdentity }
		}
		client := derpquic.NewClient(derpquic.ClientConfig{Address: net.JoinHostPort(node.EndpointHost, strconv.Itoa(int(node.EndpointQUICPort))), TLS: nodeTLS.Clone(), Credential: credential, Namespace: namespace, GenerationDisco: disco})
		var carrier magicsock.DERPCarrier = client
		if node.EndpointTCPPort != 0 && contains(node.Transports, "derp_wss") {
			u := (&url.URL{Scheme: "wss", Host: net.JoinHostPort(node.EndpointHost, strconv.Itoa(int(node.EndpointTCPPort))), Path: "/derp"}).String()
			carrier = derpquic.NewFallbackCarrier(client, derpquic.NewWSSClient(derpquic.WSSClientConfig{URL: u, TLS: nodeTLS.Clone(), Credential: credential, Namespace: namespace, GenerationDisco: disco}), 5*time.Second)
		}
		if a.options.TestOnlyDERPCarrier != nil {
			carrier = a.options.TestOnlyDERPCarrier(carrier)
		}
		return &usageObservedCarrier{DERPCarrier: carrier, authority: &a.relay, node: node, namespace: namespace}
	}
}
