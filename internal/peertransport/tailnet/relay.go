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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/tailscale/tailcat"
	"net/netip"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-derp-carrier
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-derp-carrier
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-derp-carrier
	"tailscale.com/wgengine/magicsock"
)

type relayAuthority struct {
	mu           sync.Mutex
	tokens       map[string]string
	grants       map[string]derpquic.Grant
	config       *tls.Config
	node         RegionalNode
	factory      magicsock.DERPCarrierFactory
	peerNodes    []*tailcfg.Node
	controlPeers []key.NodePublic
	nodes        map[tailcfg.DERPRegionID]RegionalNode
	recovery     *regionalRecovery
	// regionalRecoveryConfigured distinguishes dynamic regional recovery from
	// the single fixed-node mode when a stopped worker has been detached.
	regionalRecoveryConfigured bool
	device                     *deviceRelay
	deviceAddresses            []netip.AddrPort
}

// ApplyRelayGrants consumes only signed grants from the same network refresh.
// Invalid or absent grants remove cached relay authority; they cannot extend a lease.
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
	grants := map[string]derpquic.Grant{}
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
		a.relay.tokens = next
		a.relay.grants = grants
		a.relay.controlPeers = a.relay.controlPeers[:0]
		for peer := range controlSet {
			a.relay.controlPeers = append(a.relay.controlPeers, peer)
		}
		controlPeers := append([]key.NodePublic(nil), a.relay.controlPeers...)
		a.relay.mu.Unlock()
		if a.server != nil {
			_ = a.server.server.SetRelayControlPeers(controlPeers)
		}
		if a.clientEngine != nil {
			_ = a.clientEngine.SetRelayControlPeers(controlPeers)
		}
	}()
	if len(tokens) > MaxRegionalCandidates || regional == nil {
		return ErrRegionalAuthority
	}
	nodes, _, err := regional.Eligible("relay", "derp_quic", nil, time.Now())
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
		var g derpquic.Grant
		if err != nil || json.Unmarshal(body, &g) != nil {
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
		if strictDecode(header, &fullHeader) != nil || fullHeader.Algorithm != "EdDSA" || fullHeader.Type != "paperboat-relay-grant+jwt" || strictDecode(body, &g) != nil || g.Version != 1 || g.Issuer != a.options.Issuer || g.Audience != "paperboat-relay" || g.AccountID != cfg.Self.AccountID || g.EndpointID != cfg.Self.EndpointID || g.WireGuardPublicKey != cfg.Self.WireGuardPublicKey || g.CertificateFingerprint != cfg.Self.QUICCertificateFingerprint || g.QUICPublicKey != cfg.Self.QUICPublicKey || g.Generation != cfg.Generation || g.NodeGeneration != node.NodeGeneration || g.ProcessEpoch != node.ProcessEpoch || g.IssuedAt > time.Now().Unix() || g.ExpiresAt <= time.Now().Unix() || g.ExpiresAt-g.IssuedAt > 60 || g.ExpiresAt > cfg.ExpiresAt || g.ExpiresAt > node.ExpiresAt || next[g.NodeID] != "" {
			next = nil
			grants = nil
			return ErrAuthority
		}
		if len(g.Peers) != len(cfg.Peers) {
			next = nil
			grants = nil
			return ErrAuthority
		}
		if len(g.RelayControlPeers) > 16 {
			return ErrAuthority
		}
		seenControl := map[string]bool{}
		for _, peerKey := range g.RelayControlPeers {
			peer, err := publicKey(peerKey)
			if err != nil || peerKey == g.WireGuardPublicKey || seenControl[peerKey] {
				return ErrAuthority
			}
			seenControl[peerKey] = true
			controlSet[peer] = true
		}
		for i, p := range g.Peers {
			if p.WireGuardPublicKey != cfg.Peers[i].Identity.WireGuardPublicKey {
				next = nil
				grants = nil
				return ErrAuthority
			}
			expected, _ := json.Marshal(cfg.Peers[i].Scopes)
			actual, _ := json.Marshal(p.Scopes)
			if string(expected) != string(actual) {
				next = nil
				grants = nil
				return ErrAuthority
			}
		}
		next[g.NodeID] = token
		grants[g.NodeID] = g
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
	return &tailcfg.DERPRegion{RegionID: id, RegionCode: node.Region, RegionName: fmt.Sprintf("%s/%d/%s", node.NodeID, node.NodeGeneration, node.ProcessEpoch), Nodes: []*tailcfg.DERPNode{{Name: node.NodeID, RegionID: id, HostName: node.EndpointHost, DERPPort: int(node.EndpointQUICPort), STUNPort: -1}}}
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
	if !a.usableLocked() || a.server != nil || a.clientEngine != nil || a.regional == nil {
		return nil, ErrAuthority
	}
	nodes, _, err := a.regional.Eligible("relay", "derp_quic", nil, time.Now())
	if err != nil {
		return nil, err
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
		if !ok || g.ExpiresAt <= time.Now().Unix() {
			continue
		}
		for _, value := range g.RelayControlPeers {
			peer, parseErr := publicKey(value)
			if parseErr != nil {
				return nil, parseErr
			}
			controlSet[peer] = true
		}
		parts := strings.Split(a.relay.tokens[node.NodeID], ".")
		if len(parts) != 3 {
			return nil, ErrAuthority
		}
		header, _ := base64.RawURLEncoding.DecodeString(parts[0])
		var h struct {
			KeyID string `json:"kid"`
		}
		_ = json.Unmarshal(header, &h)
		pub, found, e := a.options.Keys.Lookup(context.Background(), h.KeyID)
		if e != nil || !found {
			return nil, ErrAuthority
		}
		verifier := derpquic.Verifier{Issuer: a.options.Issuer, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Keys: map[string]ed25519.PublicKey{h.KeyID: pub}}
		if _, e = verifier.Verify(a.relay.tokens[node.NodeID], tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}, time.Now()); e != nil {
			return nil, e
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
	a.relay.nodes = selected
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
	a.relay.factory = func(_ key.NodePrivate, region func() *tailcfg.DERPRegion) magicsock.DERPCarrier {
		r := region()
		a.relay.mu.Lock()
		var node RegionalNode
		if r != nil {
			node = a.relay.nodes[r.RegionID]
		}
		a.relay.mu.Unlock()
		credential := func(context.Context) (string, error) {
			a.relay.mu.Lock()
			defer a.relay.mu.Unlock()
			grant := a.relay.grants[node.NodeID]
			if node.NodeID == "" || grant.ExpiresAt <= time.Now().Unix() || grant.NodeGeneration != node.NodeGeneration || grant.ProcessEpoch != node.ProcessEpoch {
				return "", ErrAuthority
			}
			return a.relay.tokens[node.NodeID], nil
		}
		client := derpquic.NewClient(derpquic.ClientConfig{Address: net.JoinHostPort(node.EndpointHost, strconv.Itoa(int(node.EndpointQUICPort))), TLS: config.Clone(), Credential: credential})
		var carrier magicsock.DERPCarrier = client
		if node.EndpointTCPPort != 0 && contains(node.Transports, "derp_wss") {
			u := (&url.URL{Scheme: "wss", Host: net.JoinHostPort(node.EndpointHost, strconv.Itoa(int(node.EndpointTCPPort))), Path: "/derp"}).String()
			carrier = derpquic.NewFallbackCarrier(client, derpquic.NewWSSClient(derpquic.WSSClientConfig{URL: u, TLS: config.Clone(), Credential: credential}), 5*time.Second)
		}
		if a.options.TestOnlyDERPCarrier != nil {
			return a.options.TestOnlyDERPCarrier(carrier)
		}
		return carrier
	}
	return regions, nil
}

// relayServiceLocked adapts a signed discovery service without granting it
// application access. a.mu is held by callers.
func (a *Authority) relayServiceLocked(node RegionalNode, g derpquic.Grant) (*tailcfg.Node, error) {
	if g.PeerRelay == nil {
		return nil, nil
	}
	ownDisco := tailcat.DiscoPublicForNode(a.private).DiscoPublic
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
		result = derpquic.DiscoKeyString(tailcat.DiscoPublicForNode(private).DiscoPublic)
		return nil
	})
	return result, err
}
