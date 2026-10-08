package tailnet

import (
	"errors"
	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"testing"
	"time"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-accounting-test
	"tailscale.com/derp"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-accounting-test
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-accounting-test
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-accounting-test
	"tailscale.com/wgengine/magicsock"
)

type usageFakeCarrier struct {
	magicsock.DERPCarrier
	err          error
	packet       derp.ReceivedPacket
	controlCalls int
}

func (*usageFakeCarrier) Transport() string { return "derp_wss" }

func (f *usageFakeCarrier) Send(key.NodePublic, []byte) error              { return f.err }
func (f *usageFakeCarrier) SendControl(key.NodePublic, []byte) error       { f.controlCalls++; return f.err }
func (f *usageFakeCarrier) RecvDetail() (derp.ReceivedMessage, int, error) { return f.packet, 7, f.err }
func TestUsageCarrierActualNodeAndDirection(t *testing.T) {
	peer := key.NewNode().Public()
	node := RegionalNode{NodeID: "actual-reverse-route", NodeGeneration: 2, ProcessEpoch: "boot"}
	r := relayAuthority{usagePeers: map[key.NodePublic]*relayUsageObservation{peer: {}}, nodes: map[tailcfg.DERPRegionID]RegionalNode{regionalID(node.NodeID): node}, grants: map[string]derpquic.Grant{node.NodeID: {NodeID: node.NodeID, NodeGeneration: 2, ProcessEpoch: "boot", ExpiresAt: time.Now().Add(time.Minute).Unix()}}}
	data := []byte{4, 0, 0, 0, 1}
	fake := &usageFakeCarrier{packet: derp.ReceivedPacket{Source: peer, Data: data}}
	c := &usageObservedCarrier{DERPCarrier: fake, authority: &r, node: node}
	if c.Transport() != "derp_wss" {
		t.Fatal("carrier transport diagnostics lost")
	}
	if err := c.Send(peer, data); err != nil {
		t.Fatal(err)
	}
	if got := r.usageNode(peer, "download", time.Now()); got != node.NodeID {
		t.Fatalf("send path = %q", got)
	}
	if got := r.usageNode(peer, "upload", time.Now()); got != "" {
		t.Fatal("send observation attributed to upload")
	}
	if _, generation, err := c.RecvDetail(); err != nil || generation != 7 {
		t.Fatal("receive contract changed")
	}
	if got := r.usageNode(peer, "", time.Now()); got != node.NodeID {
		t.Fatalf("bidirectional actual node = %q", got)
	}
	if got := r.usageNode(peer, "download", time.Now().Add(2*time.Second)); got != "" {
		t.Fatal("stale observation accepted")
	}
	c.record(peer, true, time.Now())
	r.usagePeers[peer].receive.node.NodeID = "another-node"
	if got := r.usageNode(peer, "", time.Now()); got != "" {
		t.Fatal("asymmetric relay paths guessed")
	}
	r.usagePeers[peer].send = relayUsageSample{}
	fake.err = errors.New("send failed")
	_ = c.Send(peer, data)
	if got := r.usageNode(peer, "download", time.Now()); got != "" {
		t.Fatal("failed send recorded")
	}
	fake.err = nil
	_ = c.SendControl(peer, data)
	if fake.controlCalls != 1 || !r.usagePeers[peer].send.at.IsZero() {
		t.Fatal("control packet counted or dispatch lost")
	}
	_ = c.Send(peer, []byte("discovery"))
	_ = c.Send(key.NewNode().Public(), data)
	if len(r.usagePeers) != 1 || !r.usagePeers[peer].send.at.IsZero() {
		t.Fatal("unadmitted/discovery packet recorded")
	}
	c.record(peer, false, time.Now())
	grant := r.grants[node.NodeID]
	grant.NodeGeneration++
	r.grants[node.NodeID] = grant
	if got := r.usageNode(peer, "download", time.Now()); got != "" {
		t.Fatal("stale generation recorded")
	}
}
