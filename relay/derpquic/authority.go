// Package derpquic carries private, key-addressed WireGuard traffic over QUIC.
package derpquic

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go4.org/mem"
	"io"
	"net/netip"
	"slices"
	"strings"
	"time"

	"tailscale.com/types/key"
)

// MaxClockSkew tolerates issuer clock drift without extending signed expiry.
const MaxClockSkew = time.Minute

var ErrAdmission = &Error{Code: 1, Message: "relay authorization rejected"}
var ErrProtocol = &Error{Code: 2, Message: "relay protocol rejected"}
var ErrOverload = &Error{Code: 3, Message: "relay capacity exhausted"}

// ErrExpired requires fresh signed authority before reconnecting. It never permits
// traffic under the expired lease and is distinct from a permanent denial.
var ErrExpired = &Error{Code: 4, Message: "relay authorization expired"}
var ErrClosed = errors.New("relay closed")

type Error struct {
	Code    uint64
	Message string
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Fatal() bool   { return e.Code == 1 || e.Code == 2 }

type Scope struct {
	ResourceKind       string `json:"resource_kind"`
	ResourceID         string `json:"resource_id"`
	ResourceGeneration uint64 `json:"resource_generation"`
	Capability         string `json:"capability"`
	Direction          string `json:"direction"`
	Port               uint16 `json:"port"`
	ExpiresAt          int64  `json:"expires_at"`
}
type ServiceDescriptor struct {
	WireGuardPublicKey string `json:"wireguard_public_key"`
	DiscoPublicKey     string `json:"disco_public_key"`
	VirtualAddress     string `json:"virtual_address"`
}

type Peer struct {
	WireGuardPublicKey string  `json:"wireguard_public_key"`
	DiscoPublicKey     string  `json:"disco_public_key,omitempty"`
	Scopes             []Scope `json:"scopes"`
}
type Grant struct {
	Version                int                `json:"version"`
	Issuer                 string             `json:"iss"`
	Audience               string             `json:"aud"`
	IssuedAt               int64              `json:"iat"`
	ExpiresAt              int64              `json:"exp"`
	Generation             uint64             `json:"generation"`
	AccountID              string             `json:"account_id"`
	EndpointID             string             `json:"endpoint_id"`
	WireGuardPublicKey     string             `json:"wireguard_public_key"`
	DiscoPublicKey         string             `json:"disco_public_key,omitempty"`
	CertificateFingerprint string             `json:"quic_certificate_fingerprint"`
	QUICPublicKey          string             `json:"quic_public_key"`
	NodeID                 string             `json:"node_id"`
	NodeGeneration         uint64             `json:"node_generation"`
	ProcessEpoch           string             `json:"process_epoch"`
	Peers                  []Peer             `json:"peers"`
	PeerRelay              *ServiceDescriptor `json:"peer_relay,omitempty"`
	RelayControlPeers      []string           `json:"relay_control_peers,omitempty"`
}
type Verifier struct {
	Issuer, NodeID, ProcessEpoch string
	NodeGeneration               uint64
	Keys                         map[string]ed25519.PublicKey
}

func strictJSON(b []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return ErrProtocol
	}
	return nil
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
	if len(token) > MaxControl || len(state.PeerCertificates) != 1 {
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
	if e1 != nil || e2 != nil || e3 != nil || len(h) > 1024 || strictJSON(h, &header) != nil || header.Algorithm != "EdDSA" || header.Type != "paperboat-relay-grant+jwt" {
		return g, ErrAdmission
	}
	pub := v.Keys[header.KeyID]
	if len(pub) != 32 || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) || strictJSON(b, &g) != nil {
		return Grant{}, ErrAdmission
	}
	leaf := state.PeerCertificates[0]
	fingerprint, fingerprintErr := hex.DecodeString(g.CertificateFingerprint)
	quicPublic, publicErr := base64.RawURLEncoding.Strict().DecodeString(g.QUICPublicKey)
	presented, isEd25519 := leaf.PublicKey.(ed25519.PublicKey)
	if g.Version != 1 || g.Issuer != v.Issuer || g.Audience != "paperboat-relay" || g.NodeID != v.NodeID || g.NodeGeneration != v.NodeGeneration || g.ProcessEpoch != v.ProcessEpoch || fingerprintErr != nil || len(fingerprint) != 32 || hex.EncodeToString(fingerprint) != g.CertificateFingerprint || publicErr != nil || len(quicPublic) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(quicPublic) != g.QUICPublicKey || !isEd25519 || !bytes.Equal(presented, quicPublic) || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.NotAfter.Sub(leaf.NotBefore) > 24*time.Hour+time.Minute || leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) != nil || g.IssuedAt > now.Add(MaxClockSkew).Unix() || g.ExpiresAt <= g.IssuedAt || g.ExpiresAt-g.IssuedAt > 60 || g.Generation == 0 || g.AccountID == "" || g.EndpointID == "" || len(g.AccountID) > 256 || len(g.EndpointID) > 256 || len(g.Peers) > 64 || len(g.RelayControlPeers) > 16 {
		return Grant{}, ErrAdmission
	}
	if _, err := ParseKey(g.WireGuardPublicKey); err != nil {
		return Grant{}, err
	}
	if g.DiscoPublicKey != "" {
		if _, err := ParseDiscoKey(g.DiscoPublicKey); err != nil {
			return Grant{}, err
		}
	}
	if g.PeerRelay != nil {
		if !g.PeerRelay.Valid() || g.DiscoPublicKey == "" || g.PeerRelay.WireGuardPublicKey == g.WireGuardPublicKey {
			return Grant{}, ErrAdmission
		}
	}
	seen := map[string]bool{}
	for _, peer := range g.RelayControlPeers {
		if _, err := ParseKey(peer); err != nil || peer == g.WireGuardPublicKey || seen[peer] {
			return Grant{}, ErrAdmission
		}
		seen[peer] = true
	}
	clear(seen)
	scopes := 0
	for _, p := range g.Peers {
		if _, err := ParseKey(p.WireGuardPublicKey); err != nil || seen[p.WireGuardPublicKey] || p.WireGuardPublicKey == g.WireGuardPublicKey {
			return Grant{}, ErrAdmission
		}
		seen[p.WireGuardPublicKey] = true
		if p.DiscoPublicKey != "" {
			if _, err := ParseDiscoKey(p.DiscoPublicKey); err != nil {
				return Grant{}, err
			}
		}
		if g.PeerRelay != nil && p.DiscoPublicKey == "" {
			return Grant{}, ErrAdmission
		}
		if g.PeerRelay != nil && p.WireGuardPublicKey == g.PeerRelay.WireGuardPublicKey && p.DiscoPublicKey != g.PeerRelay.DiscoPublicKey {
			return Grant{}, ErrAdmission
		}
		if len(p.Scopes) == 0 {
			return Grant{}, ErrAdmission
		}
		for _, s := range p.Scopes {
			scopes++
			validResource := s.ResourceKind == "machine_network" && s.Capability == "connect" || s.ResourceKind == "inspector" && s.Capability == "inspector" || s.ResourceKind == "machine_access" && (s.Capability == "terminal" || s.Capability == "exec" || s.Capability == "managed_ssh" || s.Capability == "file_transfer" || s.Capability == "private_access") || s.ResourceKind == "codex_session" && s.Capability == "codex"
			if scopes > 128 || !validResource || s.ResourceID == "" || len(s.ResourceID) > 256 || s.ResourceGeneration == 0 || s.Port != 443 || (s.Direction != "dial" && s.Direction != "accept") || s.ExpiresAt < g.ExpiresAt {
				return Grant{}, ErrAdmission
			}
		}
	}
	if g.ExpiresAt <= now.Unix() {
		return Grant{}, ErrExpired
	}
	return g, nil
}

func controlAllowed(a, b Grant, now time.Time) bool {
	return a.AccountID == b.AccountID && a.ExpiresAt > now.Unix() && b.ExpiresAt > now.Unix() && slices.Contains(a.RelayControlPeers, b.WireGuardPublicKey) && slices.Contains(b.RelayControlPeers, a.WireGuardPublicKey)
}
func allowed(a, b Grant, now time.Time) bool {
	if a.ExpiresAt <= now.Unix() || b.ExpiresAt <= now.Unix() {
		return false
	}
	for _, p := range a.Peers {
		if p.WireGuardPublicKey != b.WireGuardPublicKey {
			continue
		}
		for _, q := range b.Peers {
			if q.WireGuardPublicKey != a.WireGuardPublicKey {
				continue
			}
			for _, s := range p.Scopes {
				for _, t := range q.Scopes {
					if (s.ResourceKind != "machine_network" || a.AccountID == b.AccountID) && s.ResourceKind == t.ResourceKind && s.ResourceID == t.ResourceID && s.ResourceGeneration == t.ResourceGeneration && s.Capability == t.Capability && s.Port == t.Port && s.Direction != t.Direction && s.ExpiresAt > now.Unix() && t.ExpiresAt > now.Unix() {
						return true
					}
				}
			}
		}
	}
	return false
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
func (d ServiceDescriptor) Valid() bool {
	_, e1 := ParseKey(d.WireGuardPublicKey)
	_, e2 := ParseDiscoKey(d.DiscoPublicKey)
	address, e3 := netip.ParseAddr(d.VirtualAddress)
	return e1 == nil && e2 == nil && e3 == nil && address.String() == d.VirtualAddress && netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(address)
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
