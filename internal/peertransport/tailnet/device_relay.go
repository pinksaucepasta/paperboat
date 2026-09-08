package tailnet

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	relayservice "github.com/pinksaucepasta/paperboat-relay/peerrelay"
	"github.com/tailscale/tailcat"
	"tailscale.com/disco"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

type deviceRelay struct {
	authority *Authority
	server    *relayservice.Server
	mu        sync.RWMutex
	pairs     []RelayPair
	inbox     chan deviceRelayInbound
	ctx       context.Context
	cancel    context.CancelFunc
}
type deviceRelayInbound struct {
	region  tailcfg.DERPRegionID
	peer    key.NodePublic
	payload []byte
}

type deviceRelayRequest struct {
	device  *deviceRelay
	peer    key.NodePublic
	payload []byte
}

func (r deviceRelayRequest) ControlPayload() []byte { return r.payload }
func (r deviceRelayRequest) SourceDisco() (key.DiscoPublic, error) {
	r.device.mu.RLock()
	defer r.device.mu.RUnlock()
	for _, pair := range r.device.pairs {
		for _, binding := range []NetworkBinding{pair.First, pair.Second} {
			if public, err := publicKey(binding.WireGuardPublicKey); err == nil && public == r.peer {
				return derpquicDisco(binding.DiscoPublicKey)
			}
		}
	}
	return key.DiscoPublic{}, ErrAdmission
}
func (r deviceRelayRequest) AuthorizePair(first, second key.DiscoPublic) (relayservice.PairLease, error) {
	r.device.mu.RLock()
	defer r.device.mu.RUnlock()
	now := time.Now().Unix()
	for _, pair := range r.device.pairs {
		a, errA := derpquicDisco(pair.First.DiscoPublicKey)
		b, errB := derpquicDisco(pair.Second.DiscoPublicKey)
		if errA != nil || errB != nil || pair.ExpiresAt <= now || !((a == first && b == second) || (a == second && b == first)) {
			continue
		}
		sourceA, _ := publicKey(pair.First.WireGuardPublicKey)
		sourceB, _ := publicKey(pair.Second.WireGuardPublicKey)
		if r.peer != sourceA && r.peer != sourceB {
			return nil, ErrAdmission
		}
		return &devicePairLease{device: r.device, resourceID: pair.ResourceID, first: a, second: b, account: pair.First.AccountID}, nil
	}
	return nil, ErrAdmission
}
func derpquicDisco(value string) (key.DiscoPublic, error) { return derpquic.ParseDiscoKey(value) }

type devicePairLease struct {
	device        *deviceRelay
	resourceID    string
	first, second key.DiscoPublic
	account       string
}

func (l *devicePairLease) AccountID() string { return l.account }
func (l *devicePairLease) Valid() (time.Time, bool) {
	l.device.mu.RLock()
	defer l.device.mu.RUnlock()
	now := time.Now().Unix()
	for _, pair := range l.device.pairs {
		a, ea := derpquicDisco(pair.First.DiscoPublicKey)
		b, eb := derpquicDisco(pair.Second.DiscoPublicKey)
		if ea == nil && eb == nil && pair.ResourceID == l.resourceID && pair.First.AccountID == l.account && pair.ExpiresAt > now && ((a == l.first && b == l.second) || (a == l.second && b == l.first)) {
			return time.Unix(pair.ExpiresAt, 0), true
		}
	}
	return time.Time{}, false
}

func (d *deviceRelay) handle(region tailcfg.DERPRegionID, peer key.NodePublic, payload []byte) bool {
	if len(payload) < len(disco.Magic) || string(payload[:len(disco.Magic)]) != disco.Magic {
		return false
	}
	select {
	case d.inbox <- deviceRelayInbound{region, peer, append([]byte(nil), payload...)}:
	default:
	}
	return true
}
func (d *deviceRelay) run() {
	for {
		select {
		case <-d.ctx.Done():
			return
		case in := <-d.inbox:
			reply, err := d.server.HandleControl(d.ctx, deviceRelayRequest{device: d, peer: in.peer, payload: in.payload})
			clear(in.payload)
			if err == nil {
				d.authority.mu.Lock()
				var engine relayEngine
				if d.authority.server != nil {
					engine = d.authority.server.server
				} else if d.authority.clientEngine != nil {
					engine = d.authority.clientEngine
				}
				d.authority.mu.Unlock()
				if engine != nil {
					_ = engine.SendRelayControl(in.peer, in.region, reply)
				}
			}
			clear(reply)
		}
	}
}
func (d *deviceRelay) update(pairs []RelayPair) {
	d.mu.Lock()
	d.pairs = append(d.pairs[:0], pairs...)
	d.mu.Unlock()
}
func (d *deviceRelay) close() error { d.cancel(); return d.server.Close() }

func (a *Authority) ConfigureDeviceRelay(addresses []netip.AddrPort) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.current == nil || len(addresses) == 0 || len(addresses) > 4 {
		return ErrAuthority
	}
	port := addresses[0].Port()
	for _, address := range addresses {
		if address.Port() != port {
			return ErrAuthority
		}
	}
	a.relay.deviceAddresses = append(a.relay.deviceAddresses[:0], addresses...)
	if len(a.current.RelayPairs) == 0 {
		return nil
	}
	return a.startDeviceRelayLocked(addresses)
}

func (a *Authority) startDeviceRelayLocked(addresses []netip.AddrPort) error {
	port := addresses[0].Port()
	discoPrivate := tailcat.DiscoPrivateForNode(a.private)
	descriptor := derpquic.ServiceDescriptor{WireGuardPublicKey: derpquic.KeyString(a.private.Public()), DiscoPublicKey: derpquic.DiscoKeyString(discoPrivate.Public()), VirtualAddress: a.current.Self.VirtualAddress}
	server, err := relayservice.New(relayservice.Config{Service: descriptor, DiscoPrivate: discoPrivate, Port: port, Addresses: addresses})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &deviceRelay{authority: a, server: server, pairs: append([]RelayPair(nil), a.current.RelayPairs...), inbox: make(chan deviceRelayInbound, 64), ctx: ctx, cancel: cancel}
	a.relay.mu.Lock()
	old := a.relay.device
	a.relay.device = d
	a.relay.mu.Unlock()
	if old != nil {
		_ = old.close()
	}
	go d.run()
	return nil
}
