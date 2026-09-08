package tailnet

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/tailscale/tailcat"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=regional-recovery
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=regional-recovery
	"tailscale.com/types/key"
)

// Regional recovery is metadata coordination around magicsock, not a packet
// scheduler. Existing magicsock preference and discovery own all data paths.
const regionalProbeTimeout = 5 * time.Second
const regionalProbeInterval = 3 * time.Second
const regionalDwell = 10 * time.Second
const regionalShortlist = 4

type RegionalStatus struct {
	NodeID         string
	Region         string
	BackupNodeID   string
	CombinedRTT    time.Duration
	Redundancy     Redundancy
	LocalTransport string
	PeerTransport  string
	Reason         string
	ProbeFailures  map[string]string
}

type relayEngine interface {
	SetRelayRegions([]*tailcfg.DERPRegion) error
	SetPeerRelayNodes([]*tailcfg.Node) error
	RelayTransport(tailcfg.DERPRegionID) string
	PrepareRelay(context.Context, tailcfg.DERPRegionID) error
	SetPeerRelayRegion(key.NodePublic, tailcfg.DERPRegionID) error
	SendRelayControl(key.NodePublic, tailcfg.DERPRegionID, []byte) error
}
type regionalInbound struct {
	region tailcfg.DERPRegionID
	peer   key.NodePublic
	packet []byte
}
type regionalRecovery struct {
	authority *Authority
	control   *regionalControl
	mu        sync.Mutex
	engine    relayEngine
	peerID    string
	cancel    context.CancelFunc
	done      chan struct{}
	ready     chan struct{}
	readyOnce sync.Once
	inbox     chan regionalInbound
	status    RegionalStatus
}

// newRegionalRecoveryLocked returns recovery state bound to the current node
// private key. Callers hold both Authority.mu and relay.mu.
func (a *Authority) newRegionalRecoveryLocked() *regionalRecovery {
	return &regionalRecovery{authority: a, control: newRegionalControl(a.private, time.Now), done: make(chan struct{}), ready: make(chan struct{}), inbox: make(chan regionalInbound, 64)}
}

func (a *Authority) regionalRecoveryLocked() (*regionalRecovery, bool) {
	r := a.relay.recovery
	configured := a.relay.regionalRecoveryConfigured
	if r == nil && configured {
		r = a.newRegionalRecoveryLocked()
		a.relay.recovery = r
	}
	return r, configured
}

func (a *Authority) relayControl(id tailcfg.DERPRegionID, peer key.NodePublic, packet []byte) bool {
	a.relay.mu.Lock()
	r := a.relay.recovery
	a.relay.mu.Unlock()
	if r == nil || !bytes.HasPrefix(packet, regionalControlMagic) {
		return false
	}
	if len(packet) > 2048 {
		return true
	}
	select {
	case r.inbox <- regionalInbound{id, peer, append([]byte(nil), packet...)}:
	default:
	}
	return true
}

// PrepareRegional starts a single authority-owned recovery worker. A client
// waits for an acknowledged common route before its bootstrap handshake. A host
// prepares independently so initial discovery does not depend on a working pair.
func (a *Authority) PrepareRegional(ctx context.Context, peerID string) error {
	a.mu.Lock()
	if a.closed || !a.usableLocked() {
		a.mu.Unlock()
		return ErrAuthority
	}
	var engine relayEngine
	if a.server != nil {
		engine = a.server.server
	} else if a.clientEngine != nil {
		engine = a.clientEngine
	}
	if engine == nil {
		a.mu.Unlock()
		return ErrAuthority
	}
	a.relay.mu.Lock()
	r, configuredRecovery := a.regionalRecoveryLocked()
	fixedNode := a.relay.node
	a.relay.mu.Unlock()
	if r == nil && !configuredRecovery {
		regionID := regionalID(fixedNode.NodeID)
		var peer key.NodePublic
		var disco key.DiscoPublic
		if peerID != "" && a.current != nil {
			for _, candidate := range a.current.Peers {
				if candidate.Identity.EndpointID == peerID {
					peer, _ = publicKey(candidate.Identity.WireGuardPublicKey)
					disco, _ = derpquic.ParseDiscoKey(candidate.Identity.DiscoPublicKey)
					break
				}
			}
		}
		a.mu.Unlock()
		if fixedNode.NodeID == "" {
			return nil
		}
		if err := engine.PrepareRelay(ctx, regionID); err != nil {
			return err
		}
		if peerID == "" {
			return nil
		}
		server, ok := engine.(*tailcat.Server)
		if !ok || peer.IsZero() || disco.IsZero() {
			return ErrRegionalAuthority
		}
		if err := server.EnsureRelayPeer(peer, disco); err != nil {
			return err
		}
		return engine.SetPeerRelayRegion(peer, regionID)
	}
	r.mu.Lock()
	if r.engine == nil {
		r.engine = engine
		r.peerID = peerID
		lifetime, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		a.recoveryWorkers.Add(1)
		go func() { defer a.recoveryWorkers.Done(); r.run(lifetime) }()
	}
	ready := r.ready
	r.mu.Unlock()
	a.mu.Unlock()
	if peerID == "" {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return ErrRegionalAuthority
	case <-ready:
		return nil
	}
}
func (a *Authority) RegionalStatus() RegionalStatus {
	a.relay.mu.Lock()
	r := a.relay.recovery
	a.relay.mu.Unlock()
	if r == nil {
		return RegionalStatus{Redundancy: RedundancyNone}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.status
	status.ProbeFailures = maps.Clone(status.ProbeFailures)
	return status
}
func (r *regionalRecovery) stop() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
}

func (r *regionalRecovery) snapshot() ([]RegionalNode, map[key.NodePublic]NetworkPeer, uint64, error) {
	a := r.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil || a.closed || a.current.ExpiresAt <= time.Now().Unix() {
		return nil, nil, 0, ErrExpiredAuthority
	}
	nodes, _, err := a.regional.Eligible("relay", "derp_quic", nil, time.Now())
	if err != nil {
		return nil, nil, 0, err
	}
	a.relay.mu.Lock()
	defer a.relay.mu.Unlock()
	var eligible []RegionalNode
	next := make(map[tailcfg.DERPRegionID]RegionalNode)
	for _, n := range nodes {
		g := a.relay.grants[n.NodeID]
		id := regionalID(n.NodeID)
		if old, ok := next[id]; ok && old.NodeID != n.NodeID {
			return nil, nil, 0, ErrRegionalAuthority
		}
		if g.ExpiresAt > time.Now().Unix() && g.NodeGeneration == n.NodeGeneration && g.ProcessEpoch == n.ProcessEpoch {
			next[id] = n
			eligible = append(eligible, n)
		}
	}
	a.relay.nodes = next
	peers := make(map[key.NodePublic]NetworkPeer)
	for _, p := range a.current.Peers {
		active := false
		for _, scope := range p.Scopes {
			if scope.Port == NetworkPort && scope.ExpiresAt > time.Now().Unix() {
				active = true
				break
			}
		}
		k, e := publicKey(p.Identity.WireGuardPublicKey)
		if e == nil && active {
			peers[k] = p
		}
	}
	return eligible, peers, a.current.Generation, nil
}
func (r *regionalRecovery) receive(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case in := <-r.inbox:
			nodes, peers, generation, err := r.snapshot()
			if err != nil {
				continue
			}
			if _, ok := peers[in.peer]; !ok {
				continue
			}
			var node RegionalNode
			for _, n := range nodes {
				if regionalID(n.NodeID) == in.region {
					node = n
					break
				}
			}
			if node.NodeID == "" {
				continue
			}
			reply, _ := r.control.handle(in.peer, node, generation, in.packet, func() error {
				if server, ok := r.engine.(*tailcat.Server); ok {
					r.authority.relay.mu.Lock()
					grant := r.authority.relay.grants[node.NodeID]
					r.authority.relay.mu.Unlock()
					var disco key.DiscoPublic
					for _, p := range grant.Peers {
						if p.WireGuardPublicKey == peers[in.peer].Identity.WireGuardPublicKey {
							disco, _ = derpquic.ParseDiscoKey(p.DiscoPublicKey)
						}
					}
					if disco.IsZero() {
						return ErrAuthority
					}
					if err := server.EnsureRelayPeer(in.peer, disco); err != nil {
						return err
					}
				}
				return r.engine.SetPeerRelayRegion(in.peer, in.region)
			}, r.engine.RelayTransport(in.region))
			if len(reply) > 0 {
				_ = r.engine.SendRelayControl(in.peer, in.region, reply)
			}
		}
	}
}
func (r *regionalRecovery) exchange(ctx context.Context, peer key.NodePublic, node RegionalNode, generation uint64, promote bool, observed *string) error {
	packet, pending, err := r.control.request(peer, node, generation, promote)
	if err != nil {
		return err
	}
	defer r.control.cancel(pending)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	send := func() error { return r.engine.SendRelayControl(peer, regionalID(node.NodeID), packet) }
	if err = send(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case message := <-pending.done:
			if observed != nil {
				*observed = message.Transport
			}
			return nil
		case <-ticker.C:
			if err = send(); err != nil {
				return err
			}
		}
	}
}

type regionalScore struct {
	node          RegionalNode
	rtt           time.Duration
	peerTransport string
}

func rankRegional(scores []regionalScore) {
	sort.Slice(scores, func(i, j int) bool {
		// Capacity is fresh and prefiltered; use it as a proportional queueing
		// penalty instead of conducting throughput tests on users' networks.
		a, b := regionalQuality(scores[i]), regionalQuality(scores[j])
		if a == b {
			return scores[i].node.NodeID < scores[j].node.NodeID
		}
		return a < b
	})
}
func (r *regionalRecovery) run(ctx context.Context) {
	defer close(r.done)
	receiveCtx, cancel := context.WithCancel(ctx)
	receiveDone := make(chan struct{})
	go func() { defer close(receiveDone); r.receive(receiveCtx) }()
	defer func() { cancel(); <-receiveDone }()
	var selection regionalSelection
	var deniedGeneration uint64
	cursor := 0
	backupID := ""
	retries := make(map[string]regionalRetry)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.authority.done:
			return
		case <-timer.C:
		}
		nodes, peers, generation, err := r.snapshot()
		if err != nil || len(nodes) == 0 {
			_ = r.engine.SetRelayRegions(nil)
			_ = r.engine.SetPeerRelayNodes(nil)
			r.mu.Lock()
			r.status = RegionalStatus{Redundancy: RedundancyNone, Reason: "no_authorized_common_node"}
			r.mu.Unlock()
			timer.Reset(regionalProbeDelay())
			continue
		}
		if deniedGeneration != 0 && generation <= deniedGeneration {
			timer.Reset(regionalProbeDelay())
			continue
		}
		// A stable bounded shortlist is refreshed in the background, not per stream.
		// Retain the selected node while rotating other candidates after failures.
		shortlist := append([]RegionalNode(nil), nodes...)
		if len(shortlist) > regionalShortlist {
			shortlist = nil
			for _, n := range nodes {
				if n.NodeID == selection.current() || n.NodeID == backupID || n.NodeID == selection.candidate {
					shortlist = append(shortlist, n)
				}
			}
			for i := 0; len(shortlist) < regionalShortlist && i < len(nodes); i++ {
				n := nodes[(cursor+i)%len(nodes)]
				present := false
				for _, retained := range shortlist {
					present = present || retained.NodeID == n.NodeID
				}
				if !present {
					shortlist = append(shortlist, n)
				}
			}
		}
		regions := make([]*tailcfg.DERPRegion, 0, len(nodes))
		for _, n := range nodes {
			regions = append(regions, regionForNode(n))
		}
		if err = r.engine.SetRelayRegions(regions); err != nil {
			timer.Reset(regionalProbeDelay())
			continue
		}
		if err = r.updateServices(nodes); err != nil {
			timer.Reset(regionalProbeDelay())
			continue
		}
		if client, ok := r.engine.(*tailcat.Client); ok {
			if err = client.StartNetwork(ctx); err != nil {
				timer.Reset(regionalProbeDelay())
				continue
			}
		}
		var peer key.NodePublic
		for k, p := range peers {
			if p.Identity.EndpointID == r.peerID {
				peer = k
				break
			}
		}
		if r.peerID != "" && peer.IsZero() {
			r.mu.Lock()
			r.status = RegionalStatus{Redundancy: RedundancyNone, Reason: "pair_authority_expired"}
			r.mu.Unlock()
			timer.Reset(regionalProbeDelay())
			continue
		}
		cursor = (cursor + regionalShortlist) % len(nodes)
		for id := range retries {
			present := false
			for _, n := range nodes {
				present = present || n.NodeID == id
			}
			if !present {
				delete(retries, id)
			}
		}
		results := make(chan regionalScore, len(shortlist))
		failures := make(map[string]string)
		var failureMu sync.Mutex
		var denied atomic.Bool
		fail := func(node, stage string) { failureMu.Lock(); failures[node] = stage; failureMu.Unlock() }
		// Two simultaneous preparations bound work while direct discovery continues
		// independently in magicsock. There is no serial four-carrier dial chain.
		slots := make(chan struct{}, 2)
		var workers sync.WaitGroup
		for _, n := range shortlist {
			if retry := retries[n.NodeID]; retry.generation == n.NodeGeneration && retry.epoch == n.ProcessEpoch && time.Now().Before(retry.next) {
				fail(n.NodeID, "retry_backoff")
				continue
			}
			workers.Add(1)
			go func(n RegionalNode) {
				defer workers.Done()
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-slots }()
				probe, stop := context.WithTimeout(ctx, regionalProbeTimeout)
				defer stop()
				if probeErr := r.engine.PrepareRelay(probe, regionalID(n.NodeID)); probeErr != nil {
					var fatal interface{ Fatal() bool }
					if errors.As(probeErr, &fatal) && fatal.Fatal() {
						denied.Store(true)
					}
					fail(n.NodeID, "prepare: "+probeErr.Error())
					return
				}
				start := time.Now()
				var peerTransport string
				if !peer.IsZero() {
					if r.exchange(probe, peer, n, generation, false, &peerTransport) != nil {
						fail(n.NodeID, "pair_probe")
						return
					}
				}
				results <- regionalScore{node: n, rtt: time.Since(start), peerTransport: peerTransport}
			}(n)
		}
		workers.Wait()
		close(results)
		var scores []regionalScore
		for s := range results {
			scores = append(scores, s)
		}
		for _, n := range shortlist {
			if stage, failed := failures[n.NodeID]; failed && stage != "retry_backoff" {
				retries[n.NodeID] = retries[n.NodeID].failed(n, time.Now())
			} else if !failed {
				delete(retries, n.NodeID)
			}
		}
		if denied.Load() {
			deniedGeneration = generation
			_ = r.engine.SetRelayRegions(nil)
			r.mu.Lock()
			r.status = RegionalStatus{Redundancy: RedundancyNone, Reason: "regional_admission_denied", ProbeFailures: failures}
			r.mu.Unlock()
			timer.Reset(regionalProbeDelay())
			continue
		}
		if r.peerID == "" {
			timer.Reset(regionalProbeDelay())
			continue
		}
		if len(scores) == 0 {
			r.mu.Lock()
			r.status = RegionalStatus{Redundancy: RedundancyNone, Reason: "no_mutually_reachable_node", ProbeFailures: failures}
			r.mu.Unlock()
			timer.Reset(regionalProbeDelay())
			continue
		}

		now := time.Now()
		chosen, reason := selection.choose(now, scores)
		if chosen.node.NodeID == "" {
			timer.Reset(regionalProbeDelay())
			continue
		}
		if chosen.node.NodeID != selection.current() {
			// Refreshes advance authority generations independently of long failed
			// preparations. Validate the candidate again and use current authority for
			// the fresh proof rather than carrying the sweep's stale generation.
			fresh, freshPeers, currentGeneration, snapshotErr := r.snapshot()
			valid := false
			for _, n := range fresh {
				if n.NodeID == chosen.node.NodeID && n.NodeGeneration == chosen.node.NodeGeneration && n.ProcessEpoch == chosen.node.ProcessEpoch {
					valid = true
					break
				}
			}
			freshPeer, peerValid := freshPeers[peer]
			if snapshotErr != nil || !valid || freshPeer.Identity.EndpointID != r.peerID || !peerValid {
				timer.Reset(regionalProbeDelay())
				continue
			}
			generation = currentGeneration
			promote, stop := context.WithTimeout(ctx, regionalProbeTimeout)
			// Other concurrent preparations may consume the earlier proof's lease.
			// Reconfirm the selected exact pair immediately before committing it.
			err = r.engine.PrepareRelay(promote, regionalID(chosen.node.NodeID))
			if err == nil {
				err = r.exchange(promote, peer, chosen.node, generation, false, &chosen.peerTransport)
			}
			if err == nil {
				err = r.exchange(promote, peer, chosen.node, generation, true, nil)
			}
			stop()
			if err == nil {
				// A shared authority-mode server does not install an outbound peer
				// until its first meow exchange. Regional selection must precede
				// that exchange, so install the still-authorized signed peer locally
				// before assigning its selected region.
				if server, ok := r.engine.(*tailcat.Server); ok {
					disco, discoErr := derpquic.ParseDiscoKey(freshPeer.Identity.DiscoPublicKey)
					if discoErr != nil {
						err = discoErr
					} else {
						err = server.EnsureRelayPeer(peer, disco)
					}
				}
			}
			if err == nil {
				err = r.engine.SetPeerRelayRegion(peer, regionalID(chosen.node.NodeID))
			}
			if err != nil {
				r.mu.Lock()
				r.status = RegionalStatus{Redundancy: RedundancyNone, Reason: "promotion_not_acknowledged", ProbeFailures: map[string]string{chosen.node.NodeID: err.Error()}}
				r.mu.Unlock()
				timer.Reset(regionalProbeDelay())
				continue
			}
			selection.commit(chosen.node.NodeID, time.Now())
		}
		backup, redundancy := regionalBackup(chosen.node, scores)
		backupID = backup
		status := RegionalStatus{NodeID: selection.current(), Region: chosen.node.Region, CombinedRTT: chosen.rtt, BackupNodeID: backup, Redundancy: redundancy, Reason: reason, LocalTransport: r.engine.RelayTransport(regionalID(chosen.node.NodeID)), PeerTransport: chosen.peerTransport}
		r.mu.Lock()
		r.status = status
		r.mu.Unlock()
		r.readyOnce.Do(func() { close(r.ready) })
		timer.Reset(regionalProbeDelay())
	}
}

func regionalProbeDelay() time.Duration {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return regionalProbeInterval
	}
	return 2400*time.Millisecond + time.Duration(b[0])*1200*time.Millisecond/255
}

func (r *regionalRecovery) updateServices(nodes []RegionalNode) error {
	a := r.authority
	a.mu.Lock()
	if a.current == nil {
		a.mu.Unlock()
		return ErrAuthority
	}
	a.relay.mu.Lock()
	var services []*tailcfg.Node
	for _, n := range nodes {
		service, err := a.relayServiceLocked(n, a.relay.grants[n.NodeID])
		if err != nil {
			a.relay.mu.Unlock()
			a.mu.Unlock()
			return err
		}
		if service != nil {
			services = append(services, service)
		}
	}
	a.relay.mu.Unlock()
	a.mu.Unlock()
	return r.engine.SetPeerRelayNodes(services)
}

// Failed nodes back off independently. The sweep timer adds jitter; the cap
// keeps recovery within a signed health lease without fleet-wide retry bursts.
type regionalRetry struct {
	generation uint64
	epoch      string
	attempts   uint8
	next       time.Time
}

func (r regionalRetry) failed(n RegionalNode, now time.Time) regionalRetry {
	if r.generation != n.NodeGeneration || r.epoch != n.ProcessEpoch {
		r = regionalRetry{generation: n.NodeGeneration, epoch: n.ProcessEpoch}
	}
	if r.attempts < 3 {
		r.attempts++
	}
	r.next = now.Add(regionalProbeInterval * time.Duration(1<<(r.attempts-1)))
	return r
}
