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
	"strings"
	"time"

	"go4.org/mem"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=relay-grant-key-custody
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

// Grant is a verified relay grant: the shared signed claim set (decoded strictly) and what the relay derives from it.
type Grant struct {
	peernetwork.RelayGrant
	signedHash [sha256.Size]byte
	// index finds a grant's memberships by resource without scanning them; every datagram checks its pair.
	index *grantIndex
}

type grantIndex struct {
	memberships membershipIndex
}

// withIndex returns g with its lookup index built; a verified or merged grant is not changed after.
func (g Grant) withIndex() Grant {
	g.index = &grantIndex{memberships: newMembershipIndex(g.Memberships)}
	return g
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
	return base64.RawURLEncoding.EncodeToString(k.AppendTo(nil))
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

func allowed(a, b Grant, now time.Time) bool { return allowedPairExpiry(a, b, now) > now.Unix() }

// A pair can continue while any exact reciprocal scope remains live. Its
// lifetime never exceeds either endpoint's signed grant.
func allowedPairExpiry(a, b Grant, now time.Time) int64 {
	if a.ExpiresAt <= now.Unix() || b.ExpiresAt <= now.Unix() {
		return 0
	}
	var latest int64
	reciprocalMemberships(a, b, func(s, t Scope) {
		if s.ExpiresAt > now.Unix() && t.ExpiresAt > now.Unix() {
			latest = max(latest, min(s.ExpiresAt, t.ExpiresAt, a.ExpiresAt, b.ExpiresAt))
		}
	})
	return latest
}

// sameGrantIdentity reports whether two grants were issued for the same endpoint, relay process and
// authorization generation, so their peers may be combined.
func sameGrantIdentity(a, b Grant) bool {
	return a.Issuer == b.Issuer && a.Generation == b.Generation && a.AccountID == b.AccountID && a.EndpointID == b.EndpointID &&
		a.WireGuardPublicKey == b.WireGuardPublicKey && a.DiscoPublicKey == b.DiscoPublicKey && a.CertificateFingerprint == b.CertificateFingerprint &&
		a.QUICPublicKey == b.QUICPublicKey && a.NodeID == b.NodeID && a.NodeGeneration == b.NodeGeneration && a.ProcessEpoch == b.ProcessEpoch
}

// mergeGrants adds the memberships of a further grant part of the same identity; a membership named again keeps the
// newest expiry, and the result stays within peernetwork.MaxMergedMemberships. current is never changed: a connection's
// grant, its fence and pair leases may still hold it, so the result gets its own list and index. On an error nothing
// is changed.
func mergeGrants(current, next Grant) (Grant, error) {
	memberships := mergeMemberships(current.Memberships, next.Memberships)
	if len(memberships) > peernetwork.MaxMergedMemberships {
		return Grant{}, ErrAdmission
	}
	merged := next
	merged.Memberships = memberships
	if merged.PeerRelay == nil {
		merged.PeerRelay = current.PeerRelay
	}
	merged.ExpiresAt = max(current.ExpiresAt, next.ExpiresAt)
	return merged.withIndex(), nil
}

func allowedAdmitted(a, b Grant) bool {
	matched := false
	reciprocalMemberships(a, b, func(Scope, Scope) { matched = true })
	return matched
}
func controlAdmitted(a, b Grant) bool {
	return a.AccountID == b.AccountID && a.networkMember() && b.networkMember()
}

func ParseDiscoKey(value string) (key.DiscoPublic, error) {
	node, err := ParseKey(value)
	if err != nil {
		return key.DiscoPublic{}, err
	}
	return key.DiscoPublicFromRaw32(mem.B(node.AppendTo(nil))), nil
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
