// Package nodemesh is the wire contract for the links between Paperboat's own managed nodes: edge nodes
// forwarding a visitor to the edge that holds the carrier, and relay nodes forwarding packets to the relay
// that holds the destination peer.
//
// Trust: a node authenticates another node by the SPKI pin the server published for it in the peer
// directory (tls.go), the same anchor the relay uses for clients (relay/nodetls). The package defines no
// PKI. Prior art: tailscale derp mesh (derp/derpserver `clientsMesh`, `AddPacketForwarder`), teleport proxy
// peering (lib/proxy/peer, `FromPeerProxy` = hop limit 1).
package nodemesh

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

const (
	// EdgeForwardALPN is negotiated on an edge's public HTTPS port by the node-to-node forward link.
	EdgeForwardALPN = "paperboat-edge-forward-v1"
	// RelayMeshALPN is negotiated on a relay's QUIC port by the relay-to-relay mesh link.
	RelayMeshALPN = "paperboat-derp-mesh-v1"

	// PeersPath and HoldersPath are the edge-authenticated server routes (node bearer, like /v1/nodes/heartbeat).
	PeersPath   = "/v1/edge/forward/peers"
	HoldersPath = "/v1/edge/forward/holders"

	// MaxPeers bounds the peer directory a node holds.
	MaxPeers = 256
	// MaxNameLength is the longest DNS name a forward carries (RFC 1035).
	MaxNameLength = 253
	// MaxFrame bounds one length-prefixed link message.
	MaxFrame = 64 << 10
	// MaxNamesPerFrame bounds the names in one watch frame.
	MaxNamesPerFrame = 512
	// MaxHolderPage bounds one page of the server's holder snapshot.
	MaxHolderPage = 500
	// DefaultHolderPage is the page size when a request names none.
	DefaultHolderPage = MaxHolderPage
	// ForwardHop is the only hop count a holder accepts: a forwarded visitor is never forwarded again.
	ForwardHop = 1
)

var (
	ErrInvalid = errors.New("invalid node mesh message")
	ErrFrame   = errors.New("node mesh frame is out of bounds")
)

// Refusal reasons a holder answers an Open with.
const (
	RefusalNotHolder  = "not_holder"
	RefusalDraining   = "draining"
	RefusalLoop       = "loop"
	RefusalBadRequest = "bad_request"
	RefusalBusy       = "busy"
)

// Watch kinds: a node streams what it holds to each linked peer, as a chunked snapshot then deltas.
const (
	WatchSnapshot = "snapshot"
	WatchEnd      = "end"
	WatchAdd      = "add"
	WatchRemove   = "remove"
)

// Peer is one managed node of the same operator that a node may link to.
type Peer struct {
	NodeID       string `json:"node_id"`
	Region       string `json:"region"`
	Host         string `json:"host"`
	Port         uint16 `json:"port"`
	SPKISHA256   string `json:"spki_sha256"` // "sha256:" + hex of the node's certificate SubjectPublicKeyInfo
	ProcessEpoch string `json:"process_epoch,omitempty"`
	Draining     bool   `json:"draining,omitempty"`
}

func (p Peer) Validate() error {
	if p.NodeID == "" || len(p.NodeID) > 128 || p.Region == "" || len(p.Region) > 64 || p.Host == "" || len(p.Host) > MaxNameLength || p.Port == 0 || len(p.ProcessEpoch) > 128 {
		return fmt.Errorf("%w: peer", ErrInvalid)
	}
	if _, err := ParsePin(p.SPKISHA256); err != nil {
		return err
	}
	return nil
}

// Address is the host:port a node dials.
func (p Peer) Address() string {
	host := p.Host
	if ip, err := netip.ParseAddr(host); err == nil && ip.Is6() {
		return fmt.Sprintf("[%s]:%d", host, p.Port)
	}
	return fmt.Sprintf("%s:%d", host, p.Port)
}

// PeersRequest asks the server for the caller's peer directory (strict decode on the server).
type PeersRequest struct {
	NodeID       string `json:"edge_node_id"`
	ProcessEpoch string `json:"process_epoch"`
}

func (r PeersRequest) Validate() error {
	if r.NodeID == "" || r.ProcessEpoch == "" {
		return fmt.Errorf("%w: peers request", ErrInvalid)
	}
	return nil
}

// PeersResponse is the directory: managed edge nodes other than the caller, ready or draining.
type PeersResponse struct {
	Peers      []Peer    `json:"peers"`
	ObservedAt time.Time `json:"observed_at"`
}

// HoldersRequest pages the server's snapshot of which nodes hold which names.
type HoldersRequest struct {
	NodeID       string `json:"edge_node_id"`
	ProcessEpoch string `json:"process_epoch"`
	Page         string `json:"page,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

func (r HoldersRequest) Validate() error {
	if r.NodeID == "" || r.ProcessEpoch == "" || len(r.Page) > MaxNameLength+16 || r.Limit < 0 || r.Limit > MaxHolderPage {
		return fmt.Errorf("%w: holders request", ErrInvalid)
	}
	return nil
}

// PageLimit is the page size the request asks for.
func (r HoldersRequest) PageLimit() int {
	if r.Limit == 0 {
		return DefaultHolderPage
	}
	return r.Limit
}

// Holder is one name and the nodes whose carrier serves it.
type Holder struct {
	Name    string   `json:"name"`
	NodeIDs []string `json:"node_ids"`
}

func (h Holder) Validate() error {
	if err := ValidName(h.Name); err != nil || len(h.NodeIDs) == 0 || len(h.NodeIDs) > MaxPeers+1 {
		return fmt.Errorf("%w: holder", ErrInvalid)
	}
	return nil
}

// HoldersResponse is one page; Next, when set, is sent back as Page for the rest.
type HoldersResponse struct {
	Holders []Holder `json:"holders"`
	Next    string   `json:"next,omitempty"`
}

// Open is the first message on a forward stream, sent by the node the visitor reached.
type Open struct {
	V       int    `json:"v"`
	Name    string `json:"name"`    // TLS server name the visitor asked for
	Visitor string `json:"visitor"` // visitor ip:port as the forwarding node saw it
	From    string `json:"from"`    // forwarding node id; the holder checks it against the authenticated peer
	Hop     int    `json:"hop"`
}

func (o Open) Validate() error {
	if o.V != 1 || o.Hop != ForwardHop || o.From == "" || len(o.From) > 128 {
		return fmt.Errorf("%w: open", ErrInvalid)
	}
	if err := ValidName(o.Name); err != nil {
		return err
	}
	if _, err := netip.ParseAddrPort(o.Visitor); err != nil {
		return fmt.Errorf("%w: visitor address", ErrInvalid)
	}
	return nil
}

// OpenAck answers an Open: OK, or a Refusal reason.
type OpenAck struct {
	OK      bool   `json:"ok"`
	Refusal string `json:"refusal,omitempty"`
}

// Watch is one frame of a node's holdings: a snapshot in chunks (More set until the last, then WatchEnd),
// then add/remove deltas.
type Watch struct {
	Kind  string   `json:"kind"`
	Names []string `json:"names,omitempty"`
	More  bool     `json:"more,omitempty"`
}

func (w Watch) Validate() error {
	switch w.Kind {
	case WatchSnapshot, WatchAdd, WatchRemove:
	case WatchEnd:
		if len(w.Names) != 0 {
			return fmt.Errorf("%w: watch end carries names", ErrInvalid)
		}
		return nil
	default:
		return fmt.Errorf("%w: watch kind", ErrInvalid)
	}
	if len(w.Names) > MaxNamesPerFrame {
		return fmt.Errorf("%w: watch names", ErrFrame)
	}
	for _, n := range w.Names {
		if err := ValidName(n); err != nil {
			return err
		}
	}
	return nil
}

// Stats are the counters a node reports to the server in its heartbeat (edge) or observation (relay).
type Stats struct {
	Links       int    `json:"links"`
	Names       int    `json:"names"`
	Forwarded   uint64 `json:"forwarded"`
	Accepted    uint64 `json:"accepted"`
	HolderMiss  uint64 `json:"holder_miss"`
	Unreachable uint64 `json:"unreachable"`
	LoopRefused uint64 `json:"loop_refused"`
	BytesIn     uint64 `json:"bytes_in"`
	BytesOut    uint64 `json:"bytes_out"`
}

// ValidName accepts a lower-case DNS name (labels of a-z, 0-9, '-' and '_', at most 63 bytes each, 253 in all).
func ValidName(name string) error {
	if name == "" || len(name) > MaxNameLength || name != strings.ToLower(name) || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("%w: name", ErrInvalid)
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("%w: name label", ErrInvalid)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return fmt.Errorf("%w: name character", ErrInvalid)
			}
		}
	}
	return nil
}
