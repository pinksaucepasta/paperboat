// Package forward links this managed edge node to the other managed edge nodes so that a visitor who reaches a
// node that does not hold the carrier is passed on, as raw bytes, to the node that does.
//
// The forwarding node never terminates TLS: it copies the visitor's ClientHello and every later byte over an
// authenticated node link (TLS 1.3 + yamux, peers pinned by the server's peer directory, see shared/nodemesh),
// and the holder terminates TLS and serves exactly as for a direct visitor. A forwarded visitor is never
// forwarded again (hop limit 1, enforced by the holder's listener, not by a counter alone).
// Prior art: tailscale DERP mesh (derp/derpserver), teleport proxy peering (lib/proxy/peer).
package forward

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// maxKeysPerPeer bounds what one authenticated peer may claim to hold (a variable only so a test can lower it).
var maxKeysPerPeer = 1 << 20

var errPeerTooManyKeys = errors.New("forward peer claims more names than a node can hold")

// Holder is one node that serves a key (a hostname or a raw TCP key).
type Holder struct {
	NodeID   string
	Draining bool
}

// Directory is the in-memory holder map: key -> the peer nodes serving it. Two sources feed it. A peer's watch
// stream (snapshot then deltas, withdrawn when the link breaks) is authoritative while it is complete; the
// server's seed fills the gap for peers whose watch is not complete and expires unless refreshed.
type Directory struct {
	mu     sync.RWMutex
	now    func() time.Time
	region string
	ttl    time.Duration
	peers  map[string]nodemesh.Peer
	watch  map[string]*peerWatch
	byKey  map[string]map[string]struct{}
	seed   map[string]map[string]time.Time
}

type peerWatch struct {
	names        map[string]struct{}
	complete     bool
	snapshotting bool
}

// NewDirectory returns an empty directory for a node in region; seed entries live for seedTTL.
func NewDirectory(region string, seedTTL time.Duration, now func() time.Time) *Directory {
	if now == nil {
		now = time.Now
	}
	return &Directory{now: now, region: region, ttl: seedTTL, peers: map[string]nodemesh.Peer{}, watch: map[string]*peerWatch{}, byKey: map[string]map[string]struct{}{}, seed: map[string]map[string]time.Time{}}
}

// SetPeers replaces the peer directory. What a peer that left it held is forgotten.
func (d *Directory) SetPeers(peers []nodemesh.Peer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	next := make(map[string]nodemesh.Peer, len(peers))
	for _, p := range peers {
		next[p.NodeID] = p
	}
	for id := range d.peers {
		if _, ok := next[id]; !ok {
			d.dropLocked(id)
		}
	}
	d.peers = next
}

// ApplyWatch applies one frame of peer's holdings. A snapshot replaces what the peer held and is complete at its
// last chunk; add and remove are deltas.
func (d *Directory) ApplyWatch(peer string, w nodemesh.Watch) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.peers[peer]; !ok {
		return nodemesh.ErrPeerUnknown
	}
	state := d.watch[peer]
	if state == nil {
		state = &peerWatch{names: map[string]struct{}{}}
		d.watch[peer] = state
	}
	switch w.Kind {
	case nodemesh.WatchSnapshot:
		if !state.snapshotting {
			d.clearNamesLocked(peer, state)
			state.snapshotting, state.complete = true, false
		}
		if err := d.addLocked(peer, state, w.Names); err != nil {
			return err
		}
		if !w.More {
			state.snapshotting, state.complete = false, true
		}
	case nodemesh.WatchEnd:
		state.snapshotting, state.complete = false, true
	case nodemesh.WatchAdd:
		return d.addLocked(peer, state, w.Names)
	case nodemesh.WatchRemove:
		for _, name := range w.Names {
			d.removeLocked(peer, state, name)
		}
	}
	return nil
}

func (d *Directory) addLocked(peer string, state *peerWatch, names []string) error {
	for _, name := range names {
		if _, dup := state.names[name]; dup {
			continue
		}
		if len(state.names) >= maxKeysPerPeer {
			return errPeerTooManyKeys
		}
		state.names[name] = struct{}{}
		holders := d.byKey[name]
		if holders == nil {
			holders = map[string]struct{}{}
			d.byKey[name] = holders
		}
		holders[peer] = struct{}{}
	}
	return nil
}

func (d *Directory) removeLocked(peer string, state *peerWatch, name string) {
	delete(state.names, name)
	if holders := d.byKey[name]; holders != nil {
		delete(holders, peer)
		if len(holders) == 0 {
			delete(d.byKey, name)
		}
	}
}

func (d *Directory) clearNamesLocked(peer string, state *peerWatch) {
	for name := range state.names {
		d.removeLocked(peer, state, name)
	}
}

func (d *Directory) dropLocked(peer string) {
	if state := d.watch[peer]; state != nil {
		d.clearNamesLocked(peer, state)
		delete(d.watch, peer)
	}
}

// DropPeer withdraws everything peer's watch said: its link broke. The server seed covers the gap.
func (d *Directory) DropPeer(peer string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropLocked(peer)
}

// Complete reports whether peer's watch snapshot has arrived whole.
func (d *Directory) Complete(peer string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	state := d.watch[peer]
	return state != nil && state.complete
}

// ApplySeed records a page of the server's holder snapshot; each entry expires after the seed TTL.
func (d *Directory) ApplySeed(page []nodemesh.Holder) {
	expires := d.now().Add(d.ttl)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, h := range page {
		nodes := d.seed[h.Name]
		if nodes == nil {
			nodes = make(map[string]time.Time, len(h.NodeIDs))
			d.seed[h.Name] = nodes
		}
		for _, id := range h.NodeIDs {
			nodes[id] = expires
		}
	}
}

// Refused forgets that node holds key: it answered that it does not.
func (d *Directory) Refused(key, node string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if state := d.watch[node]; state != nil {
		d.removeLocked(node, state, key)
	}
	if nodes := d.seed[key]; nodes != nil {
		delete(nodes, node)
		if len(nodes) == 0 {
			delete(d.seed, key)
		}
	}
}

// Sweep deletes expired seed entries so the seed map stays bounded by what the server last said.
func (d *Directory) Sweep() {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, nodes := range d.seed {
		for id, expires := range nodes {
			if !now.Before(expires) {
				delete(nodes, id)
			}
		}
		if len(nodes) == 0 {
			delete(d.seed, key)
		}
	}
}

// Holders lists the peers serving key: watch-sourced ones, plus seeded ones whose watch is not complete.
// Order: not draining first, then the node's own region, then node id.
func (d *Directory) Holders(key string) []Holder {
	now := d.now()
	d.mu.RLock()
	defer d.mu.RUnlock()
	ids := map[string]struct{}{}
	for id := range d.byKey[key] {
		ids[id] = struct{}{}
	}
	for id, expires := range d.seed[key] {
		if state := d.watch[id]; now.Before(expires) && (state == nil || !state.complete) {
			ids[id] = struct{}{}
		}
	}
	out := make([]Holder, 0, len(ids))
	for id := range ids {
		if peer, ok := d.peers[id]; ok {
			out = append(out, Holder{NodeID: id, Draining: peer.Draining})
		}
	}
	slices.SortFunc(out, func(a, b Holder) int {
		if a.Draining != b.Draining {
			if a.Draining {
				return 1
			}
			return -1
		}
		ar, br := d.peers[a.NodeID].Region == d.region, d.peers[b.NodeID].Region == d.region
		if ar != br {
			if ar {
				return -1
			}
			return 1
		}
		return strings.Compare(a.NodeID, b.NodeID)
	})
	return out
}

// Names counts the keys with at least one holder in the directory.
func (d *Directory) Names() int {
	now := d.now()
	d.mu.RLock()
	defer d.mu.RUnlock()
	n := len(d.byKey)
	for key, nodes := range d.seed {
		if _, watched := d.byKey[key]; watched {
			continue
		}
		for _, expires := range nodes {
			if now.Before(expires) {
				n++
				break
			}
		}
	}
	return n
}

// TCPPorts lists the public ports of the raw TCP tunnels some peer holds (keys tcp:<port>), ascending. It scans the
// key space once per call; the listener asks every reconcile tick (2 s), which a few hundred thousand keys serve.
// shortcut: a separate port index would make this O(ports), when a node holds more than ~1M names.
func (d *Directory) TCPPorts() []uint16 {
	now := d.now()
	d.mu.RLock()
	defer d.mu.RUnlock()
	seen := map[uint16]struct{}{}
	for key := range d.byKey {
		if port, ok := nodemesh.ParseTCPKey(key); ok {
			seen[port] = struct{}{}
		}
	}
	for key, nodes := range d.seed {
		port, ok := nodemesh.ParseTCPKey(key)
		if !ok {
			continue
		}
		for id, expires := range nodes {
			if state := d.watch[id]; now.Before(expires) && (state == nil || !state.complete) {
				if _, known := d.peers[id]; known {
					seen[port] = struct{}{}
					break
				}
			}
		}
	}
	ports := make([]uint16, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	slices.Sort(ports)
	return ports
}
