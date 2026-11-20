package derpquic

import (
	"container/list"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=keyed-peer-connection-registry
	"tailscale.com/types/key"
)

// Control-stream frames of a relay-to-relay mesh link (nodemesh length-prefixed JSON, one frame at most
// nodemesh.MaxFrame). Prior art: tailscale derp FramePeerPresent / FramePeerGone / FrameWatchConns
// (derp/derp.go), here as JSON because they are not the hot path (forwarded packets are QUIC datagrams).
const (
	meshPresent   = "present"    // keys: route keys now connected to the sender
	meshGone      = "gone"       // keys: route keys no longer connected to the sender
	meshSnapshot  = "snapshot"   // keys: the sender's connected route keys in chunks (More set until the last); sent on link up
	meshNeedGrant = "need_grant" // key: the receiver dropped a forwarded packet from this route key and wants its signed grants
	meshGrant     = "grant"      // key + namespace + one signed grant token of that route key's carrier
)

// meshKeysPerFrame bounds the keys one frame carries.
const meshKeysPerFrame = nodemesh.MaxNamesPerFrame

type meshFrame struct {
	Kind      string   `json:"kind"`
	Keys      []string `json:"keys,omitempty"`
	More      bool     `json:"more,omitempty"`
	Key       string   `json:"key,omitempty"`
	Namespace string   `json:"namespace,omitempty"` // hex of the carrier namespace (grant only)
	Token     string   `json:"token,omitempty"`
}

var errMeshFrame = errors.New("invalid mesh frame")

// Validate bounds a received frame before it is acted on.
func (f meshFrame) Validate() error {
	switch f.Kind {
	case meshPresent, meshGone, meshSnapshot:
		if len(f.Keys) > meshKeysPerFrame || f.Key != "" || f.Token != "" {
			return errMeshFrame
		}
	case meshNeedGrant:
		if f.Key == "" || len(f.Keys) != 0 || f.Token != "" {
			return errMeshFrame
		}
	case meshGrant:
		if f.Key == "" || f.Token == "" || len(f.Token) > nodemesh.MaxFrame || len(f.Keys) != 0 || len(f.Namespace) != 2*sha256.Size {
			return errMeshFrame
		}
	default:
		return errMeshFrame
	}
	return nil
}

// routeKeys decodes the keys of a presence frame.
func (f meshFrame) routeKeys() ([]key.NodePublic, error) {
	out := make([]key.NodePublic, 0, len(f.Keys))
	for _, s := range f.Keys {
		k, err := ParseKey(s)
		if err != nil {
			return nil, errMeshFrame
		}
		out = append(out, k)
	}
	return out, nil
}

// remoteSource is a sender connected to another relay: the signed grant that relay's carrier presented, verified
// here with the same issuer keys as a local one. It is immutable once cached (a merge makes a new one), so the
// pair memo below, guarded by Server.mu, never outlives the grant it was computed from.
type remoteSource struct {
	grant     Grant
	id        string // account + endpoint, the fence key
	root      key.NodePublic
	route     key.NodePublic
	namespace [32]byte
	via       *meshLink // the link of the relay that vouched for it: a grant is valid only from there
	expires   int64
	dead      atomic.Bool
	paired    pairMemo
}

// verifyRemoteGrant checks a grant token a mesh peer relays for one of its carriers, exactly as Verifier.Verify
// checks a local one except what only the carrier's own handshake can prove: the grant must name the relay that sent it
// (node id, issuer, signature, bounds) and the key it vouches for; the TLS binding was checked by that relay when it
// admitted the carrier, and its process epoch is not known here.
func (v Verifier) verifyRemoteGrant(token string, keys map[string]ed25519.PublicKey, from string, route key.NodePublic, namespace [32]byte, now time.Time) (Grant, error) {
	var g Grant
	parts := strings.Split(token, ".")
	if len(token) > peernetwork.MaxGrantBytes || len(parts) != 3 {
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
	pub := keys[header.KeyID]
	if len(pub) != ed25519.PublicKeySize {
		return g, fmt.Errorf("%w: %w", ErrAdmission, errUnknownKey)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) || peernetwork.DecodeSigned(b, &g.RelayGrant) != nil {
		return Grant{}, ErrAdmission
	}
	g.DropUnknownScopes()
	if err := g.Check(now); err != nil {
		return Grant{}, fmt.Errorf("%w: %w", err, ErrAdmission)
	}
	root, err := ParseKey(g.WireGuardPublicKey)
	switch {
	case g.Issuer != v.Issuer:
		return Grant{}, fmt.Errorf("mesh grant issuer: %w", ErrAdmission)
	case g.NodeID != from:
		return Grant{}, fmt.Errorf("mesh grant names another relay: %w", ErrAdmission)
	case err != nil || routeKey(root, namespace) != route:
		return Grant{}, fmt.Errorf("mesh grant is for another key: %w", ErrAdmission)
	case g.ExpiresAt <= now.Unix():
		return Grant{}, ErrExpired
	}
	g.signedHash = sha256.Sum256(b)
	return g.withIndex(), nil
}

func newRemoteSource(g Grant, route key.NodePublic, namespace [32]byte, via *meshLink) *remoteSource {
	root, _ := ParseKey(g.WireGuardPublicKey)
	return &remoteSource{grant: g, id: g.AccountID + "\x00" + g.EndpointID, root: root, route: route, namespace: namespace, via: via, expires: g.ExpiresAt}
}

// remoteCache is the bounded LRU of remote senders' grants, fetched on demand (NeedGrant). A grant is large (up to
// 256 memberships), so the bound is the node's connection capacity, not the whole mesh. Guarded by Mesh.mu.
type remoteCache struct {
	limit int
	list  *list.List // front = most recently used; values are *remoteSource
	index map[key.NodePublic]*list.Element
}

func newRemoteCache(limit int) *remoteCache {
	return &remoteCache{limit: limit, list: list.New(), index: make(map[key.NodePublic]*list.Element)}
}

// get returns the live entry of a route key, dropping an expired one.
func (c *remoteCache) get(k key.NodePublic, now time.Time) *remoteSource {
	e := c.index[k]
	if e == nil {
		return nil
	}
	rs := e.Value.(*remoteSource)
	if rs.dead.Load() || rs.expires <= now.Unix() {
		c.remove(k)
		return nil
	}
	c.list.MoveToFront(e)
	return rs
}

func (c *remoteCache) put(rs *remoteSource) {
	if old := c.index[rs.route]; old != nil {
		old.Value.(*remoteSource).dead.Store(true)
		c.list.Remove(old)
	}
	c.index[rs.route] = c.list.PushFront(rs)
	for c.list.Len() > c.limit {
		c.remove(c.list.Back().Value.(*remoteSource).route)
	}
}

func (c *remoteCache) remove(k key.NodePublic) {
	if e := c.index[k]; e != nil {
		e.Value.(*remoteSource).dead.Store(true)
		c.list.Remove(e)
		delete(c.index, k)
	}
}

// removeVia drops every entry a link vouched for (the link broke).
func (c *remoteCache) removeVia(l *meshLink) {
	for k, e := range c.index {
		if e.Value.(*remoteSource).via == l {
			c.remove(k)
		}
	}
}

func (c *remoteCache) len() int { return c.list.Len() }

// namespaceHex and parseNamespace carry a carrier namespace in a grant frame.
func namespaceHex(n [32]byte) string { return hex.EncodeToString(n[:]) }

func parseNamespace(s string) ([32]byte, bool) {
	var n [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(n) {
		return n, false
	}
	copy(n[:], b)
	return n, true
}

// authorizedRemoteLocked is authorizedLocked for a packet that arrived over the mesh: q is the local destination, the
// source is a verified remote grant. The delivering relay judges the pair and the revocation fences itself.
func (s *Server) authorizedRemoteLocked(rs *remoteSource, q *peerConn, control bool, now time.Time) bool {
	if rs == nil || q == nil || rs.dead.Load() || rs.expires <= now.Unix() || s.peers[routeKey(q.key, q.namespace)] != q || s.draining && !now.Before(s.deadline) {
		return false
	}
	if f := s.fences[rs.id]; f != nil && rs.grant.Generation < f.Generation {
		return false
	}
	if rs.paired.peer != q || rs.paired.theirs != q.grant.signedHash {
		rs.paired = pairMemo{peer: q, theirs: q.grant.signedHash, allowed: rs.namespace == q.namespace && allowedAdmitted(rs.grant, q.grant)}
	}
	return rs.paired.allowed || control && controlAdmitted(rs.grant, q.grant)
}

// MeshPeers returns the relay directory the mesh holds in node id order; nil when this relay has no mesh.
func (s *Server) MeshPeers() []nodemesh.Peer {
	if s.mesh == nil {
		return nil
	}
	return s.mesh.peerSet.Load().Peers()
}
