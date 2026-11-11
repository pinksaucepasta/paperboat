// Package derpquic carries private, key-addressed WireGuard traffic over QUIC.
package derpquic

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/maphash"
	"slices"
	"strings"
	"time"

	"go4.org/mem"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"tailscale.com/types/key"
)

// MaxClockSkew tolerates issuer clock drift without extending signed expiry.
const MaxClockSkew = peernetwork.MaxClockSkew

// MaxGrantLifetime bounds a signed grant's exp - iat.
const MaxGrantLifetime = relaylimits.MaxGrantLifetime

var ErrAdmission = &Error{Code: 1, Message: "relay authorization rejected"}
var ErrProtocol = &Error{Code: 2, Message: "relay protocol rejected"}
var ErrOverload = &Error{Code: 3, Message: "relay capacity exhausted"}

// ErrExpired requires fresh signed authority before reconnecting. It never permits
// traffic under the expired lease and is distinct from a permanent denial.
var ErrExpired = &Error{Code: 4, Message: "relay authorization expired"}

// ErrQuota is the typed, non-fatal refusal for an account whose workspaces are all out of
// bandwidth allowance: the control plane told this relay to refuse it. Clients back off and
// retry; it ends as soon as the control plane stops listing the account.
var ErrQuota = &Error{Code: 5, Message: "relay bandwidth allowance exhausted"}
var ErrClosed = errors.New("relay closed")

type Error struct {
	Code    uint64
	Message string
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Fatal() bool   { return e.Code == 1 || e.Code == 2 }

// Scope is the shared signed access scope (shared/peernetwork), as the server mints it.
type Scope = peernetwork.Scope

// ServiceDescriptor names a peer relay service (shared/peernetwork).
type ServiceDescriptor = peernetwork.RelayDescriptor

// Peer is one peer a grant pairs its endpoint with (shared/peernetwork).
type Peer = peernetwork.RelayPeer

// Grant is a verified relay grant: the shared signed claim set (decoded strictly) and what the relay derives from it.
type Grant struct {
	peernetwork.RelayGrant
	signedHash [sha256.Size]byte
	// index finds one peer's entry and a control peer without scanning the lists; every datagram
	// checks its pair, and a scan of both grants' peers made forwarding cost O(peers) per packet.
	index *grantIndex
}

type grantIndex struct {
	peers       map[string]int
	control     map[string]bool
	memberships membershipIndex
}

// withIndex returns g with its lookup index built; a verified or merged grant is not changed after.
func (g Grant) withIndex() Grant {
	index := &grantIndex{}
	if len(g.Peers) > 0 {
		index.peers = make(map[string]int, len(g.Peers))
	}
	if len(g.RelayControlPeers) > 0 {
		index.control = make(map[string]bool, len(g.RelayControlPeers))
	}
	for i, peer := range g.Peers {
		if _, seen := index.peers[peer.WireGuardPublicKey]; !seen {
			index.peers[peer.WireGuardPublicKey] = i
		}
	}
	for _, peer := range g.RelayControlPeers {
		index.control[peer] = true
	}
	index.memberships = newMembershipIndex(g.Memberships)
	g.index = index
	return g
}

// peerEntry is g's entry for the peer with WireGuard key k.
func (g Grant) peerEntry(k string) (Peer, bool) {
	if g.index != nil {
		i, ok := g.index.peers[k]
		if !ok {
			return Peer{}, false
		}
		return g.Peers[i], true
	}
	for _, peer := range g.Peers {
		if peer.WireGuardPublicKey == k {
			return peer, true
		}
	}
	return Peer{}, false
}

// controls reports whether g names k as a peer-relay control peer.
func (g Grant) controls(k string) bool {
	if g.index != nil {
		return g.index.control[k]
	}
	return slices.Contains(g.RelayControlPeers, k)
}

type Verifier struct {
	Issuer, NodeID, ProcessEpoch string
	NodeGeneration               uint64
	Keys                         map[string]ed25519.PublicKey
}

func ParseKey(s string) (key.NodePublic, error) {
	var k key.NodePublic
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	if e != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != s {
		return k, ErrAdmission
	}
	if k.UnmarshalText([]byte("nodekey:"+hex.EncodeToString(b))) != nil || k.IsZero() {
		return k, ErrAdmission
	}
	return k, nil
}
func KeyString(k key.NodePublic) string {
	b := k.Raw32()
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func (v Verifier) Verify(token string, state tls.ConnectionState, now time.Time) (Grant, error) {
	var g Grant
	if len(token) > peernetwork.MaxGrantBytes || len(state.PeerCertificates) != 1 {
		return g, ErrAdmission
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return g, ErrAdmission
	}
	h, e1 := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	b, e2 := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	sig, e3 := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}
	if e1 != nil || e2 != nil || e3 != nil || len(h) > 1024 || peernetwork.DecodeSigned(h, &header) != nil || header.Algorithm != "EdDSA" || header.Type != peernetwork.RelayGrantType {
		return g, ErrAdmission
	}
	pub := v.Keys[header.KeyID]
	if len(pub) != 32 {
		return Grant{}, fmt.Errorf("%w: %w", ErrAdmission, errUnknownKey)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) || peernetwork.DecodeSigned(b, &g.RelayGrant) != nil {
		return Grant{}, ErrAdmission
	}
	g.DropUnknownScopes()
	if err := g.Check(now); err != nil {
		return Grant{}, fmt.Errorf("%w: %w", err, ErrAdmission)
	}
	leaf := state.PeerCertificates[0]
	quicPublic, _ := base64.RawURLEncoding.Strict().DecodeString(g.QUICPublicKey)
	presented, isEd25519 := leaf.PublicKey.(ed25519.PublicKey)
	// Each refusal names its check (never the grant's content) for the relay log.
	switch {
	case g.Issuer != v.Issuer:
		return Grant{}, fmt.Errorf("grant issuer: %w", ErrAdmission)
	case g.NodeID != v.NodeID:
		return Grant{}, fmt.Errorf("grant relay node: %w", ErrAdmission)
	case g.NodeGeneration != v.NodeGeneration || g.ProcessEpoch != v.ProcessEpoch:
		return Grant{}, fmt.Errorf("grant relay process changed; refresh authority: %w", ErrExpired)
	case !isEd25519 || !bytes.Equal(presented, quicPublic):
		return Grant{}, fmt.Errorf("client certificate key differs from grant: %w", ErrAdmission)
	case now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.NotAfter.Sub(leaf.NotBefore) > 24*time.Hour+time.Minute || leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) != nil:
		return Grant{}, fmt.Errorf("client certificate validity: %w", ErrAdmission)
	}
	if g.ExpiresAt <= now.Unix() {
		return Grant{}, ErrExpired
	}
	g.signedHash = sha256.Sum256(b)
	return g.withIndex(), nil
}

func controlAllowed(a, b Grant, now time.Time) bool {
	return a.ExpiresAt > now.Unix() && b.ExpiresAt > now.Unix() && controlAdmitted(a, b)
}

// controlsPeer reports whether g admits relay-control traffic with the endpoint holding other: by
// membership in its account's mesh, or by naming its key.
func (g Grant) controlsPeer(other Grant) bool {
	if len(g.Memberships) > 0 {
		return g.networkMember()
	}
	return g.controls(other.WireGuardPublicKey)
}
func allowed(a, b Grant, now time.Time) bool { return allowedPairExpiry(a, b, now) > now.Unix() }

// A pair can continue while any exact reciprocal scope remains live. Its
// lifetime never exceeds either endpoint's signed grant.
func allowedPairExpiry(a, b Grant, now time.Time) int64 {
	if a.ExpiresAt <= now.Unix() || b.ExpiresAt <= now.Unix() {
		return 0
	}
	var latest int64
	reciprocalScopes(a, b, func(s, t Scope) {
		if s.ExpiresAt > now.Unix() && t.ExpiresAt > now.Unix() {
			latest = max(latest, min(s.ExpiresAt, t.ExpiresAt, a.ExpiresAt, b.ExpiresAt))
		}
	})
	return latest
}

// MaxMergedPeers bounds the peers one endpoint's grants name together: the account's endpoint bound.
const MaxMergedPeers = peernetwork.MaxEndpoints

// MaxGrantPeers bounds the peers, and the peer-relay control peers, one grant part names; an endpoint
// with more sends one part per chunk of the same generation (the server issues chunks of this size).
const MaxGrantPeers = peernetwork.MaxGrantPeers

// sameGrantIdentity reports whether two grants were issued for the same endpoint, relay process and
// authorization generation, so their peers may be combined.
func sameGrantIdentity(a, b Grant) bool {
	return a.Issuer == b.Issuer && a.Generation == b.Generation && a.AccountID == b.AccountID && a.EndpointID == b.EndpointID &&
		a.WireGuardPublicKey == b.WireGuardPublicKey && a.DiscoPublicKey == b.DiscoPublicKey && a.CertificateFingerprint == b.CertificateFingerprint &&
		a.QUICPublicKey == b.QUICPublicKey && a.NodeID == b.NodeID && a.NodeGeneration == b.NodeGeneration && a.ProcessEpoch == b.ProcessEpoch
}

// mergeGrants adds the peers of a further grant of the same identity. A peer named again takes the
// newer grant's scopes (a refreshed chunk); the result stays within MaxMergedPeers. current is never changed:
// a connection's grant, its fence and pair leases may still hold it, so the result gets its own lists and
// index (copy on write, at most MaxMergedPeers entries). On an error nothing is changed.
func mergeGrants(current, next Grant) (Grant, error) {
	// Parts of one grant share its shape: memberships, or per-peer lists.
	if len(current.Memberships) > 0 && len(next.Peers)+len(next.RelayControlPeers) > 0 || len(next.Memberships) > 0 && len(current.Peers)+len(current.RelayControlPeers) > 0 {
		return Grant{}, ErrAdmission
	}
	memberships := mergeMemberships(current.Memberships, next.Memberships)
	if len(memberships) > MaxGrantMemberships {
		return Grant{}, ErrAdmission
	}
	current.Peers, current.RelayControlPeers = slices.Clone(current.Peers), slices.Clone(current.RelayControlPeers)
	current = current.withIndex()
	index := current.index
	addedPeers := make(map[string]bool, len(next.Peers))
	for _, peer := range next.Peers {
		if _, ok := index.peers[peer.WireGuardPublicKey]; !ok {
			addedPeers[peer.WireGuardPublicKey] = true
		}
	}
	addedControl := make(map[string]bool, len(next.RelayControlPeers))
	for _, peer := range next.RelayControlPeers {
		if !index.control[peer] {
			addedControl[peer] = true
		}
	}
	if len(current.Peers)+len(addedPeers) > MaxMergedPeers || len(current.RelayControlPeers)+len(addedControl) > MaxMergedPeers {
		return Grant{}, ErrAdmission
	}
	// A first part may contain only peers or only discovery-control entries.
	// Allocate the other index only after the merged size has been validated.
	if len(addedPeers) != 0 && index.peers == nil {
		index.peers = make(map[string]int, len(addedPeers))
	}
	if len(addedControl) != 0 && index.control == nil {
		index.control = make(map[string]bool, len(addedControl))
	}
	merged := next
	merged.Peers, merged.RelayControlPeers = current.Peers, current.RelayControlPeers
	for _, peer := range next.Peers {
		if i, ok := index.peers[peer.WireGuardPublicKey]; ok {
			merged.Peers[i] = peer
			continue
		}
		index.peers[peer.WireGuardPublicKey] = len(merged.Peers)
		merged.Peers = append(merged.Peers, peer)
	}
	for _, peer := range next.RelayControlPeers {
		if !index.control[peer] {
			index.control[peer] = true
			merged.RelayControlPeers = append(merged.RelayControlPeers, peer)
		}
	}
	if merged.PeerRelay == nil {
		merged.PeerRelay = current.PeerRelay
	}
	if len(memberships) > 0 {
		merged.Memberships = memberships
		index.memberships = newMembershipIndex(memberships)
	}
	merged.ExpiresAt = max(current.ExpiresAt, next.ExpiresAt)
	merged.index = index
	return merged, nil
}

func allowedAdmitted(a, b Grant) bool {
	matched := false
	reciprocalScopes(a, b, func(Scope, Scope) { matched = true })
	return matched
}
func controlAdmitted(a, b Grant) bool {
	return a.AccountID == b.AccountID && a.controlsPeer(b) && b.controlsPeer(a)
}

// reciprocalScopes calls match for each pair of scopes, one from each grant, that authorize a and b to each
// other. Each grant brings either memberships or its per-peer entry for the other; while grants move from
// one shape to the other, a pair of different shapes matches too.
func reciprocalScopes(a, b Grant, match func(Scope, Scope)) {
	switch {
	case len(a.Memberships) > 0 && len(b.Memberships) > 0:
		reciprocalMemberships(a, b, match)
		return
	case len(a.Memberships) > 0:
		reciprocalScopes(b, a, func(s, t Scope) { match(t, s) })
		return
	case len(b.Memberships) > 0:
		p, ok := a.peerEntry(b.WireGuardPublicKey)
		if !ok {
			return
		}
		for _, s := range p.Scopes {
			if s.ResourceKind == "machine_network" && a.AccountID != b.AccountID {
				continue
			}
			if b.index == nil {
				b = b.withIndex()
			}
			k := keyOf(s)
			b.eachMembership(maphash.Comparable(membershipSeed, k), func(t Scope) {
				if s.Direction != t.Direction && keyOf(t) == k {
					match(s, t)
				}
			})
		}
		return
	}
	p, ok := a.peerEntry(b.WireGuardPublicKey)
	if !ok {
		return
	}
	q, ok := b.peerEntry(a.WireGuardPublicKey)
	if !ok {
		return
	}
	for _, s := range p.Scopes {
		for _, t := range q.Scopes {
			if (s.ResourceKind != "machine_network" || a.AccountID == b.AccountID) && s.ResourceKind == t.ResourceKind && s.ResourceID == t.ResourceID && s.ResourceGeneration == t.ResourceGeneration && s.Capability == t.Capability && s.Port == t.Port && s.Direction != t.Direction {
				match(s, t)
			}
		}
	}
}

func ParseDiscoKey(value string) (key.DiscoPublic, error) {
	node, err := ParseKey(value)
	if err != nil {
		return key.DiscoPublic{}, err
	}
	raw := node.Raw32()
	return key.DiscoPublicFromRaw32(mem.B(raw[:])), nil
}
func DiscoKeyString(k key.DiscoPublic) string {
	return base64.RawURLEncoding.EncodeToString(k.AppendTo(nil))
}

// classifiedFailure preserves a transport or credential cause alongside the
// finite relay decision. Its text never copies remote close reasons or URLs.
type classifiedFailure struct {
	decision *Error
	cause    error
}

func (e classifiedFailure) Error() string   { return e.decision.Error() }
func (e classifiedFailure) Unwrap() []error { return []error{e.decision, e.cause} }
func retainDecision(decision *Error, cause error) error {
	if cause == nil || cause == decision {
		return decision
	}
	return classifiedFailure{decision: decision, cause: cause}
}
