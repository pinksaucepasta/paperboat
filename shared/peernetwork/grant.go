package peernetwork

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

// The relay grant (relaygrant.go) is checked by one rule everywhere: the relay and the CLI call RelayGrant.Check after
// the signature, the mint before signing, so no side can emit or expect a value the other refuses (as Tailscale's
// tailcfg is the one wire contract of control, clients and DERP).
const (
	// MaxClockSkew tolerates issuer clock drift without extending a signed expiry.
	MaxClockSkew = time.Minute
	// MaxGrantMemberships bounds the memberships one grant carries.
	MaxGrantMemberships = 256
	// MaxGrantParts bounds the grants (chunks of MaxGrantPeers peers) one relay credential joins.
	MaxGrantParts = 32
	// MaxGrantIDBytes bounds a grant's account and endpoint ids.
	MaxGrantIDBytes = 256
)

// ErrInvalidDocument means a signed peer network document (a grant or a configuration) breaks the contract.
var ErrInvalidDocument = errors.New("peer network document is invalid")

func invalid(step string) error { return fmt.Errorf("%s: %w", step, ErrInvalidDocument) }

// DecodeSigned decodes the claims of a signed document exactly: an unknown claim could be a restriction this
// build does not understand (AGENTS.md), and trailing data is refused.
func DecodeSigned(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("decode claims: %w: %w", ErrInvalidDocument, err)
	}
	if d.Decode(new(json.RawMessage)) != io.EOF {
		return invalid("trailing data")
	}
	return nil
}

// DropUnknownScopes removes the scopes whose kind or capability this build does not know (a newer server's), and
// the peers left with none, so they authorize nothing here instead of failing the whole grant; Tailscale clients
// ignore capabilities they do not know the same way. Receivers call it before Check; the mint never does, so the
// server cannot sign a scope no receiver of its build knows.
func (g *RelayGrant) DropUnknownScopes() {
	g.Memberships = knownScopes(g.Memberships)
	peers := g.Peers[:0:0]
	for _, p := range g.Peers {
		if p.Scopes = knownScopes(p.Scopes); len(p.Scopes) > 0 {
			peers = append(peers, p)
		}
	}
	if g.Peers != nil {
		g.Peers = peers
	}
}

func knownScopes(scopes []Scope) []Scope {
	if scopes == nil {
		return nil
	}
	out := make([]Scope, 0, len(scopes))
	for _, s := range scopes {
		if ValidScope(s.ResourceKind, s.Capability) {
			out = append(out, s)
		}
	}
	return out
}

// Check applies every value rule of a grant that does not depend on who verifies it (which issuer, relay node and
// process, or TLS certificate): the relay and the CLI call it after verifying the signature, the mint before
// signing. Expiry against now is the caller's (relays answer an expired grant differently).
func (g RelayGrant) Check(now time.Time) error {
	fingerprint, fingerprintErr := hex.DecodeString(g.CertificateFingerprint)
	switch {
	case g.Version != RelayGrantVersion || g.Issuer == "" || g.Audience != RelayGrantAudience:
		return invalid("grant issuer")
	case g.IssuedAt > now.Add(MaxClockSkew).Unix() || g.ExpiresAt <= g.IssuedAt || g.ExpiresAt-g.IssuedAt > int64(relaylimits.MaxGrantLifetime/time.Second):
		return invalid("grant lifetime")
	case g.Generation == 0 || !boundedID(g.AccountID) || !boundedID(g.EndpointID) || g.NodeID == "" || g.NodeGeneration == 0 || g.ProcessEpoch == "":
		return invalid("grant identity")
	case fingerprintErr != nil || len(fingerprint) != 32 || hex.EncodeToString(fingerprint) != g.CertificateFingerprint || !ValidKey(g.QUICPublicKey):
		return invalid("grant key encoding")
	case !ValidKey(g.WireGuardPublicKey) || g.DiscoPublicKey != "" && !ValidKey(g.DiscoPublicKey):
		return invalid("grant endpoint keys")
	case len(g.Peers) > MaxGrantPeers || len(g.RelayControlPeers) > MaxGrantPeers:
		return invalid("grant peer count")
	case g.PeerRelay != nil && (!g.PeerRelay.Valid() || g.DiscoPublicKey == "" || g.PeerRelay.WireGuardPublicKey == g.WireGuardPublicKey):
		return invalid("grant peer relay")
	}
	seen := map[string]bool{}
	for _, peer := range g.RelayControlPeers {
		if !ValidKey(peer) || peer == g.WireGuardPublicKey || seen[peer] {
			return invalid("grant relay control peer")
		}
		seen[peer] = true
	}
	clear(seen)
	scopes := 0
	for _, p := range g.Peers {
		switch {
		case !ValidKey(p.WireGuardPublicKey) || seen[p.WireGuardPublicKey] || p.WireGuardPublicKey == g.WireGuardPublicKey:
			return invalid("grant peer key")
		case p.DiscoPublicKey != "" && !ValidKey(p.DiscoPublicKey):
			return invalid("grant peer disco key")
		case g.PeerRelay != nil && (p.DiscoPublicKey == "" || p.WireGuardPublicKey == g.PeerRelay.WireGuardPublicKey && p.DiscoPublicKey != g.PeerRelay.DiscoPublicKey):
			return invalid("grant peer relay disco key")
		case len(p.Scopes) == 0:
			return invalid("grant peer without scopes")
		}
		seen[p.WireGuardPublicKey] = true
		for _, s := range p.Scopes {
			if scopes++; scopes > MaxGrantScopes || !ValidGrantScope(s) {
				return invalid("grant scope")
			}
		}
	}
	if len(g.Memberships) == 0 {
		return nil
	}
	if len(g.Memberships) > MaxGrantMemberships || len(g.Peers) != 0 || len(g.RelayControlPeers) != 0 {
		return invalid("grant memberships")
	}
	members := make(map[Scope]bool, len(g.Memberships))
	for _, m := range g.Memberships {
		if !ValidGrantScope(m) || members[m] {
			return invalid("grant membership")
		}
		members[m] = true
	}
	return nil
}

func boundedID(s string) bool { return s != "" && len(s) <= MaxGrantIDBytes }

// ValidKey reports whether s is a canonical unpadded base64url 32-byte public key that is not all zero (a
// WireGuard, disco or QUIC key as the server encodes it).
func ValidKey(s string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == s && !bytes.Equal(b, make([]byte, 32))
}

// peerRelayPrefix holds every peer relay service's virtual address.
var peerRelayPrefix = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

// Valid reports whether d names valid keys and a canonical virtual address of the peer relay prefix.
func (d RelayDescriptor) Valid() bool {
	address, err := netip.ParseAddr(d.VirtualAddress)
	return ValidKey(d.WireGuardPublicKey) && ValidKey(d.DiscoPublicKey) && err == nil && address.String() == d.VirtualAddress && peerRelayPrefix.Contains(address)
}
