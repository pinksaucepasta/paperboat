package peernetwork

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"net/netip"
	"strings"
	"time"
)

// The signed network configuration: the server builds a Configuration, the mint signs it, and every CLI and daemon
// decodes it with DecodeSigned and checks it with Check. One type and one rule on both sides, as for RelayGrant.
const (
	// ConfigurationType is the JWT typ of a network configuration.
	ConfigurationType = "paperboat-network-config+jwt"
	// ConfigurationAudience is a network configuration's aud.
	ConfigurationAudience = "paperboat-network"
	// MaxSafeInteger is the largest generation a document carries (exact in every JSON reader).
	MaxSafeInteger = 1<<53 - 1
	// MaxPeerEndpoints bounds the candidate addresses one peer entry carries (and an endpoint reports).
	MaxPeerEndpoints = 8
)

// Endpoint roles: a signed-in CLI session or an enrolled machine.
const (
	RoleCLI     = "cli"
	RoleMachine = "machine"
)

// Binding is one endpoint's signed identity in a configuration: its own (self) or a peer's.
type Binding struct {
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

// Peer is one other endpoint of a configuration with the scopes of the pair.
type Peer struct {
	Identity Binding `json:"identity"`
	Scopes   []Scope `json:"scopes"`
	// Endpoints are the peer's own candidate UDP addresses ("ip:port": its LAN interfaces, STUN-mapped and
	// port-mapped addresses), so the engine can reach it directly with no relay (PLAN 3b "Direct paths without a
	// relay"); absent until the peer reported some.
	Endpoints []string `json:"endpoints,omitempty"`
}

// Configuration is a signed network configuration's claims: an endpoint's identity and every peer it may reach.
type Configuration struct {
	Version    int     `json:"version"`
	Issuer     string  `json:"iss"`
	Audience   string  `json:"aud"`
	IssuedAt   int64   `json:"iat"`
	ExpiresAt  int64   `json:"exp"`
	Generation uint64  `json:"generation"`
	Self       Binding `json:"self"`
	Peers      []Peer  `json:"peers"`
}

// DropUnknownScopes removes the scopes whose kind or capability this build does not know, and the peers left with
// none (RelayGrant.DropUnknownScopes). Receivers call it before Check; the mint never does.
func (c *Configuration) DropUnknownScopes() {
	if c.Peers == nil {
		return
	}
	peers := c.Peers[:0:0]
	for _, p := range c.Peers {
		if p.Scopes = knownScopes(p.Scopes); len(p.Scopes) > 0 {
			peers = append(peers, p)
		}
	}
	c.Peers = peers
}

// Check applies every rule of a configuration that does not depend on who verifies it (which issuer and local
// identity): every CLI and daemon calls it after the signature, the mint before signing. Expiry against now is the
// caller's (a restored configuration is used past its exp, F21-01).
func (c Configuration) Check(now time.Time) error {
	if c.Version != 1 || c.Issuer == "" || c.Audience != ConfigurationAudience {
		return invalid("configuration issuer")
	}
	if c.Generation == 0 || c.Generation > MaxSafeInteger || c.IssuedAt <= 0 || c.IssuedAt > now.Add(MaxClockSkew).Unix() || c.ExpiresAt <= c.IssuedAt {
		return invalid("configuration lifetime")
	}
	s := c.Self
	if !s.Valid() || len(c.Peers) > MaxEndpoints {
		return invalid("configuration self")
	}
	ids, addresses, keys := map[string]bool{s.EndpointID: true}, map[string]bool{s.VirtualAddress: true}, map[string]bool{s.WireGuardPublicKey: true}
	scopes := 0
	for _, p := range c.Peers {
		b := p.Identity
		if !b.Valid() || s.Role == RoleCLI && b.Role == RoleCLI || ids[b.EndpointID] || addresses[b.VirtualAddress] || keys[b.WireGuardPublicKey] || len(p.Scopes) == 0 || !ValidPeerEndpoints(p.Endpoints) {
			return invalid("configuration peer")
		}
		ids[b.EndpointID], addresses[b.VirtualAddress], keys[b.WireGuardPublicKey] = true, true, true
		seen := map[string]bool{}
		for _, scope := range p.Scopes {
			k := scope.ResourceID + "\x00" + scope.Capability + "\x00" + scope.Direction
			if scopes++; scopes > MaxScopes || !configurationScope(s, b, scope) || seen[k] {
				return invalid("configuration scope")
			}
			seen[k] = true
		}
	}
	return nil
}

// configurationScope is the rule for one scope of peer b in self's configuration.
func configurationScope(self, b Binding, s Scope) bool {
	// The account mesh pair is taken from the endpoint's own account only.
	networkOnly := s.ResourceKind == "machine_network" && s.Capability == "connect" && b.AccountID == self.AccountID
	validResource := networkOnly || s.ResourceKind != "machine_network" && ValidScope(s.ResourceKind, s.Capability)
	// Until-revoked scopes (expires_at 0, F21-01/F21-11): the network pair, a host's offline session rights
	// (machine_access accept) and the mirror in a member's CLI document (machine_access dial, named by the host's
	// endpoint id), whose generation is the deciding policy version.
	sessionRight := s.ResourceKind == "machine_access" && s.Capability != "private_access" && (s.Direction == "accept" || s.ResourceID == b.EndpointID)
	validGeneration := s.ResourceGeneration == 1 || sessionRight && s.ResourceGeneration >= 1 && s.ResourceGeneration <= MaxSafeInteger
	switch {
	case !validResource || !ValidID(s.ResourceID) || !validGeneration || s.Port != NetworkPort || s.ExpiresAt < 0:
		return false
	case s.ExpiresAt == 0 && !networkOnly && !sessionRight:
		return false
	case s.Direction != "dial" && s.Direction != "accept" || self.Role == RoleCLI && s.Direction != "dial":
		return false
	}
	return networkOnly || self.Role != b.Role && (self.Role != RoleMachine || s.Direction == "accept")
}

// ValidID reports whether id is a bounded identifier of the documents' alphabet.
func ValidID(id string) bool {
	if len(id) == 0 || len(id) > MaxResourceIDBytes {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return false
		}
	}
	return true
}

// ValidLocal reports whether b is a well-formed identity before key registration: ids, generations, certificate
// and role (a CLI has no machine, a machine is its own endpoint).
func (b Binding) ValidLocal() bool {
	hash, err := hex.DecodeString(b.QUICCertificateFingerprint)
	if !ValidID(b.AccountID) || !ValidID(b.EndpointID) || b.EndpointGeneration == 0 || b.EndpointGeneration > MaxSafeInteger || err != nil || len(hash) != 32 || hex.EncodeToString(hash) != b.QUICCertificateFingerprint || !ValidKey(b.QUICPublicKey) {
		return false
	}
	return b.Role == RoleCLI && b.MachineID == "" && b.MachineGeneration == 0 || b.Role == RoleMachine && b.MachineID == b.EndpointID && b.MachineGeneration > 0 && b.MachineGeneration <= MaxSafeInteger
}

// Valid reports whether b is a complete registered identity: ValidLocal, a key generation, a usable WireGuard key,
// a disco key and a canonical virtual address of the peer network.
func (b Binding) Valid() bool {
	addr, err := netip.ParseAddr(b.VirtualAddress)
	return b.ValidLocal() && b.KeyGeneration > 0 && b.KeyGeneration <= MaxSafeInteger && ValidWireGuardKey(b.WireGuardPublicKey) && ValidKey(b.DiscoPublicKey) &&
		err == nil && addr.String() == b.VirtualAddress && peerRelayPrefix.Contains(addr)
}

// ValidWireGuardKey reports whether s is a ValidKey that X25519 accepts (no low-order point, whose shared secret
// would be all zero).
func ValidWireGuardKey(s string) bool {
	if !ValidKey(s) {
		return false
	}
	raw, _ := base64.RawURLEncoding.DecodeString(s)
	var scalar [32]byte
	scalar[0] = 1
	validation, _ := ecdh.X25519().NewPrivateKey(scalar[:])
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return false
	}
	_, err = validation.ECDH(pub)
	return err == nil
}

// ValidPeerEndpoint is the rule for one candidate address: a unicast ip:port with a port that is not loopback,
// unspecified, multicast, zoned or IPv4-mapped. The server stores reported addresses normalized to it.
func ValidPeerEndpoint(addr netip.AddrPort) bool {
	ip := addr.Addr()
	return addr.Port() != 0 && ip.IsValid() && ip.Zone() == "" && !ip.Is4In6() && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast()
}

// ValidPeerEndpoints reports whether endpoints are at most MaxPeerEndpoints distinct canonical ValidPeerEndpoint
// addresses.
func ValidPeerEndpoints(endpoints []string) bool {
	if len(endpoints) > MaxPeerEndpoints {
		return false
	}
	seen := make(map[netip.AddrPort]bool, len(endpoints))
	for _, raw := range endpoints {
		addr, err := netip.ParseAddrPort(raw)
		if err != nil || !ValidPeerEndpoint(addr) || addr.String() != raw || seen[addr] {
			return false
		}
		seen[addr] = true
	}
	return true
}
