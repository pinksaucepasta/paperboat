package tailnet

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"sync"
	"time"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-regional-control
	"tailscale.com/types/key"
)

const regionalControlLifetime = 10 * time.Second
const regionalControlLimit = 64

var regionalControlMagic = []byte("pb-region\x01")

// regionalMessage is sealed using the existing upstream node-key box. Regional
// control grants no access: callers must validate current pair and node authority.
type regionalMessage struct {
	Transport        string   `json:"transport,omitempty"`
	Kind             string   `json:"kind"`
	Epoch            [16]byte `json:"epoch"`
	Sequence         uint64   `json:"sequence"`
	Nonce            [16]byte `json:"nonce"`
	Challenge        [16]byte `json:"challenge"`
	NodeID           string   `json:"node_id"`
	NodeGeneration   uint64   `json:"node_generation"`
	ProcessEpoch     string   `json:"process_epoch"`
	SenderGeneration uint64   `json:"sender_generation"`
}

type regionalControlKey struct {
	peer key.NodePublic
	node string
}
type regionalPending struct {
	done    chan regionalMessage
	key     regionalControlKey
	message regionalMessage
	expires time.Time
}
type regionalProof struct {
	message regionalMessage
	expires time.Time
}
type regionalHostProof struct {
	message         regionalMessage
	expires         time.Time
	localGeneration uint64
	promoted        *regionalMessage
}
type regionalPromotionFence struct {
	epoch    [16]byte
	sequence uint64
	expires  time.Time
}
type regionalControl struct {
	mu       sync.Mutex
	private  key.NodePrivate
	now      func() time.Time
	epoch    [16]byte
	sequence uint64
	pending  map[regionalControlKey]*regionalPending
	proofs   map[regionalControlKey]regionalProof
	host     map[regionalControlKey]regionalHostProof
	fences   map[key.NodePublic]regionalPromotionFence
}

func newRegionalControl(private key.NodePrivate, now func() time.Time) *regionalControl {
	if now == nil {
		now = time.Now
	}
	r := &regionalControl{private: private, now: now}
	rand.Read(r.epoch[:])
	r.reset()
	return r
}

// reset invalidates all local and remote proofs when the caller replaces authority.
// Pending users retain their own context deadlines; channels are never closed.
func (r *regionalControl) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = make(map[regionalControlKey]*regionalPending)
	r.proofs = make(map[regionalControlKey]regionalProof)
	r.host = make(map[regionalControlKey]regionalHostProof)
	r.fences = make(map[key.NodePublic]regionalPromotionFence)
}

func (r *regionalControl) prune(now time.Time) {
	for k, p := range r.pending {
		if !now.Before(p.expires) {
			delete(r.pending, k)
		}
	}
	for k, p := range r.proofs {
		if !now.Before(p.expires) {
			delete(r.proofs, k)
		}
	}
	for k, p := range r.host {
		if !now.Before(p.expires) {
			delete(r.host, k)
		}
	}
	for k, p := range r.fences {
		if !now.Before(p.expires) {
			delete(r.fences, k)
		}
	}
}

func (r *regionalControl) admits(k regionalControlKey) bool {
	peers := map[key.NodePublic]bool{k.peer: true}
	nodes := map[string]bool{k.node: true}
	for existing := range r.pending {
		peers[existing.peer] = true
		nodes[existing.node] = true
	}
	for existing := range r.proofs {
		peers[existing.peer] = true
		nodes[existing.node] = true
	}
	for existing := range r.host {
		peers[existing.peer] = true
		nodes[existing.node] = true
	}
	for peer := range r.fences {
		peers[peer] = true
	}
	return len(peers) <= 32 && len(nodes) <= 32
}

func (r *regionalControl) seal(peer key.NodePublic, m regionalMessage) []byte {
	b, _ := json.Marshal(m)
	return append(bytes.Clone(regionalControlMagic), r.private.SealTo(peer, b)...)
}

func regionalNodeMatches(m regionalMessage, n RegionalNode) bool {
	return n.NodeID != "" && n.NodeGeneration != 0 && n.ProcessEpoch != "" && m.NodeID == n.NodeID && m.NodeGeneration == n.NodeGeneration && m.ProcessEpoch == n.ProcessEpoch
}

func (r *regionalControl) request(peer key.NodePublic, node RegionalNode, generation uint64, promote bool) ([]byte, *regionalPending, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.prune(now)
	k := regionalControlKey{peer, node.NodeID}
	if r.private.IsZero() || peer.IsZero() || peer == r.private.Public() || generation == 0 || node.NodeID == "" || len(node.NodeID) > 256 || node.NodeGeneration == 0 || node.ProcessEpoch == "" || len(node.ProcessEpoch) > 256 || r.pending[k] != nil || len(r.pending) >= regionalControlLimit || !r.admits(k) || r.sequence == ^uint64(0) {
		return nil, nil, ErrRegionalAuthority
	}
	r.sequence++
	m := regionalMessage{Kind: "probe", Epoch: r.epoch, Sequence: r.sequence, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, SenderGeneration: generation}
	rand.Read(m.Nonce[:])
	if promote {
		proof, ok := r.proofs[k]
		if !ok || !regionalNodeMatches(proof.message, node) || proof.message.SenderGeneration != generation {
			return nil, nil, ErrRegionalAuthority
		}
		m.Kind = "promote"
		m.Challenge = proof.message.Challenge
		delete(r.proofs, k)
	}
	p := &regionalPending{done: make(chan regionalMessage, 1), key: k, message: m, expires: now.Add(regionalControlLifetime)}
	r.pending[k] = p
	return r.seal(peer, m), p, nil
}

func (r *regionalControl) cancel(p *regionalPending) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[p.key] == p {
		delete(r.pending, p.key)
	}
}

// handle must run only after caller authorization. promote must not reenter r.
// A reply echoes the sender's generation, which need not equal ours.
func (r *regionalControl) handle(source key.NodePublic, node RegionalNode, generation uint64, packet []byte, promote func() error, transport ...string) ([]byte, bool) {
	if !bytes.HasPrefix(packet, regionalControlMagic) {
		return nil, false
	}
	if len(packet) > 2048 || r.private.IsZero() || source.IsZero() || source == r.private.Public() || generation == 0 {
		return nil, true
	}
	clear, ok := r.private.OpenFrom(source, packet[len(regionalControlMagic):])
	if !ok {
		return nil, true
	}
	var m regionalMessage
	if strictDecode(clear, &m) != nil || !regionalNodeMatches(m, node) || m.SenderGeneration == 0 || m.Sequence == 0 || m.Epoch == [16]byte{} || m.Nonce == [16]byte{} {
		return nil, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.prune(now)
	k := regionalControlKey{source, node.NodeID}
	switch m.Kind {
	case "probe_ack", "promote_ack":
		p := r.pending[k]
		if p == nil || p.message.SenderGeneration != generation {
			return nil, true
		}
		expected := p.message
		expected.Kind += "_ack"
		expected.Transport = m.Transport
		if m.Transport != "" && m.Transport != "derp_quic" && m.Transport != "derp_wss" {
			return nil, true
		}
		if expected.Kind == "probe_ack" {
			expected.Challenge = m.Challenge
		}
		if expected != m || m.Challenge == [16]byte{} {
			return nil, true
		}
		delete(r.pending, k)
		if m.Kind == "probe_ack" {
			if len(r.proofs) >= regionalControlLimit {
				delete(r.proofs, k)
				if len(r.proofs) >= regionalControlLimit {
					return nil, true
				}
			}
			// Use request creation time, never extend proof validity by delayed ACK.
			r.proofs[k] = regionalProof{m, p.expires}
		}
		p.done <- m
		return nil, true
	case "probe":
		if m.Transport != "" {
			return nil, true
		}
		if m.Challenge != [16]byte{} || !r.admits(k) {
			return nil, true
		}
		// A new coordinator incarnation invalidates every old candidate proof.
		fence := r.fences[source]
		if fence.epoch != m.Epoch {
			for existing := range r.host {
				if existing.peer == source {
					delete(r.host, existing)
				}
			}
			fence = regionalPromotionFence{epoch: m.Epoch}
		}
		h, exists := r.host[k]
		if exists {
			original := h.message
			original.Challenge = [16]byte{}
			if h.localGeneration == generation && original == m {
				ack := h.message
				ack.Kind = "probe_ack"
				if len(transport) > 0 {
					ack.Transport = transport[0]
				}
				return r.seal(source, ack), true
			}
			if h.localGeneration == generation && h.message.Epoch == m.Epoch && m.Sequence <= h.message.Sequence {
				return nil, true
			}
		} else if len(r.host) >= regionalControlLimit {
			return nil, true
		}
		rand.Read(m.Challenge[:])
		fence.expires = now.Add(regionalControlLifetime)
		r.fences[source] = fence
		r.host[k] = regionalHostProof{message: m, expires: now.Add(regionalControlLifetime), localGeneration: generation}
		m.Kind = "probe_ack"
		if len(transport) > 0 {
			m.Transport = transport[0]
		}
		return r.seal(source, m), true
	case "promote":
		if m.Transport != "" {
			return nil, true
		}
		h, ok := r.host[k]
		if !ok || h.localGeneration != generation || !regionalNodeMatches(h.message, node) || h.message.Epoch != m.Epoch || h.message.SenderGeneration != m.SenderGeneration || h.message.Challenge != m.Challenge || m.Challenge == [16]byte{} || m.Sequence <= h.message.Sequence {
			return nil, true
		}
		fence := r.fences[source]
		if fence.epoch != m.Epoch || m.Sequence < fence.sequence || m.Sequence == fence.sequence && h.promoted == nil {
			return nil, true
		}
		if h.promoted != nil {
			if *h.promoted != m {
				return nil, true
			}
		} else {
			if promote == nil || promote() != nil {
				return nil, true
			}
			committed := m
			h.promoted = &committed
			r.host[k] = h
			fence.sequence = m.Sequence
			r.fences[source] = fence
		}
		m.Kind = "promote_ack"
		if len(transport) > 0 {
			m.Transport = transport[0]
		}
		return r.seal(source, m), true
	}
	return nil, true
}
