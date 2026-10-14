package tailnet

import (
	"testing"
	"time"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-regional-control-test
	"tailscale.com/types/key"
)

func TestRegionalControlProofPromotionAndLostAck(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := newRegionalControl(key.NewNode(), func() time.Time { return now }), newRegionalControl(key.NewNode(), func() time.Time { return now })
	node := RegionalNode{NodeID: "one", NodeGeneration: 1, ProcessEpoch: "boot"}
	count := 0
	promote := func() error { count++; return nil }
	packet, p, err := a.request(b.private.Public(), node, 7, false)
	if err != nil {
		t.Fatal(err)
	}
	ack, handled := b.handle(a.private.Public(), node, 9, packet, promote)
	if !handled || len(ack) == 0 || count != 0 {
		t.Fatal("probe changed route or failed")
	}
	a.handle(b.private.Public(), node, 7, ack, nil)
	select {
	case <-p.done:
	default:
		t.Fatal("probe missing")
	}
	packet, p, err = a.request(b.private.Public(), node, 7, true)
	if err != nil {
		t.Fatal(err)
	}
	ack, _ = b.handle(a.private.Public(), node, 9, packet, promote)
	if len(ack) == 0 || count != 1 {
		t.Fatal("promotion failed")
	}
	ack, _ = b.handle(a.private.Public(), node, 9, packet, promote)
	if len(ack) == 0 || count != 1 {
		t.Fatal("retry repeated promotion")
	}
	a.handle(b.private.Public(), node, 7, ack, nil)
	select {
	case <-p.done:
	default:
		t.Fatal("promotion ack missing")
	}
	now = now.Add(regionalControlLifetime)
	if reply, _ := b.handle(a.private.Public(), node, 9, packet, promote); len(reply) != 0 || count != 1 {
		t.Fatal("expired promotion accepted")
	}
}

func TestRegionalControlRejectsTamperWrongSourceNodeAndGeneration(t *testing.T) {
	a, b := newRegionalControl(key.NewNode(), nil), newRegionalControl(key.NewNode(), nil)
	node := RegionalNode{NodeID: "one", NodeGeneration: 1, ProcessEpoch: "boot"}
	packet, p, err := a.request(b.private.Public(), node, 7, false)
	if err != nil {
		t.Fatal(err)
	}
	ack, _ := b.handle(a.private.Public(), node, 9, packet, nil)
	bad := append([]byte(nil), ack...)
	bad[len(bad)-1] ^= 1
	a.handle(b.private.Public(), node, 7, bad, nil)
	a.handle(key.NewNode().Public(), node, 7, ack, nil)
	wrong := node
	wrong.NodeID = "two"
	a.handle(b.private.Public(), wrong, 7, ack, nil)
	wrong = node
	wrong.NodeGeneration++
	a.handle(b.private.Public(), wrong, 7, ack, nil)
	wrong = node
	wrong.ProcessEpoch = "other"
	a.handle(b.private.Public(), wrong, 7, ack, nil)
	a.handle(b.private.Public(), node, 8, ack, nil)
	select {
	case <-p.done:
		t.Fatal("invalid reply accepted")
	default:
	}
	a.handle(b.private.Public(), node, 7, ack, nil)
	select {
	case <-p.done:
	default:
		t.Fatal("valid reply rejected")
	}
	a.handle(b.private.Public(), node, 7, ack, nil)
	select {
	case <-p.done:
		t.Fatal("replayed reply delivered")
	default:
	}
}

func TestRegionalControlNewProbeInvalidatesOldPromotion(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := newRegionalControl(key.NewNode(), func() time.Time { return now }), newRegionalControl(key.NewNode(), func() time.Time { return now })
	node := RegionalNode{NodeID: "one", NodeGeneration: 1, ProcessEpoch: "boot"}
	probe, _, _ := a.request(b.private.Public(), node, 1, false)
	ack, _ := b.handle(a.private.Public(), node, 2, probe, nil)
	a.handle(b.private.Public(), node, 1, ack, nil)
	old, p, _ := a.request(b.private.Public(), node, 1, true)
	a.cancel(p)
	probe, _, _ = a.request(b.private.Public(), node, 1, false)
	b.handle(a.private.Public(), node, 2, probe, nil)
	count := 0
	if reply, _ := b.handle(a.private.Public(), node, 2, old, func() error { count++; return nil }); len(reply) != 0 || count != 0 {
		t.Fatal("old proof promoted")
	}
	now = now.Add(regionalControlLifetime)
	if _, _, err := a.request(b.private.Public(), node, 1, true); err == nil {
		t.Fatal("expired proof accepted")
	}
}

func TestRegionalControlBoundsAndReset(t *testing.T) {
	a := newRegionalControl(key.NewNode(), nil)
	node := RegionalNode{NodeID: "one", NodeGeneration: 1, ProcessEpoch: "boot"}
	peer := key.NewNode().Public()
	_, p, err := a.request(peer, node, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.request(peer, node, 1, false); err == nil {
		t.Fatal("duplicate outstanding allowed")
	}
	a.cancel(p)
	for i := 0; i < 32; i++ {
		if _, _, err := a.request(key.NewNode().Public(), node, 1, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := a.request(peer, node, 1, false); err == nil {
		t.Fatal("peer bound exceeded")
	}
	a.reset()
	if _, _, err := a.request(peer, node, 1, false); err != nil {
		t.Fatal(err)
	}
}

func TestRegionalControlFencesPromotionsAcrossCandidates(t *testing.T) {
	a, b := newRegionalControl(key.NewNode(), nil), newRegionalControl(key.NewNode(), nil)
	one := RegionalNode{NodeID: "one", NodeGeneration: 1, ProcessEpoch: "boot"}
	two := one
	two.NodeID = "two"
	for _, node := range []RegionalNode{one, two} {
		packet, _, _ := a.request(b.private.Public(), node, 1, false)
		ack, _ := b.handle(a.private.Public(), node, 2, packet, nil)
		a.handle(b.private.Public(), node, 1, ack, nil)
	}
	old, _, _ := a.request(b.private.Public(), one, 1, true)
	newer, _, _ := a.request(b.private.Public(), two, 1, true)
	count := 0
	promote := func() error { count++; return nil }
	if ack, _ := b.handle(a.private.Public(), two, 2, newer, promote); len(ack) == 0 {
		t.Fatal("new promotion failed")
	}
	if ack, _ := b.handle(a.private.Public(), one, 2, old, promote); len(ack) != 0 || count != 1 {
		t.Fatal("out of order promotion applied")
	}
}

func TestRegionalControlNewEpochAndAuthorityInvalidateProof(t *testing.T) {
	private := key.NewNode()
	a, b := newRegionalControl(private, nil), newRegionalControl(key.NewNode(), nil)
	one := RegionalNode{NodeID: "one", NodeGeneration: 1, ProcessEpoch: "boot"}
	two := one
	two.NodeID = "two"
	packet, _, _ := a.request(b.private.Public(), one, 1, false)
	ack, _ := b.handle(a.private.Public(), one, 2, packet, nil)
	a.handle(b.private.Public(), one, 1, ack, nil)
	old, _, _ := a.request(b.private.Public(), one, 1, true)
	restarted := newRegionalControl(private, nil)
	packet, _, _ = restarted.request(b.private.Public(), two, 1, false)
	ack, _ = b.handle(private.Public(), two, 2, packet, nil)
	restarted.handle(b.private.Public(), two, 1, ack, nil)
	count := 0
	promote := func() error { count++; return nil }
	if reply, _ := b.handle(private.Public(), one, 2, old, promote); len(reply) != 0 || count != 0 {
		t.Fatal("previous epoch promoted")
	}
	packet, _, _ = restarted.request(b.private.Public(), two, 1, true)
	if reply, _ := b.handle(private.Public(), two, 3, packet, promote); len(reply) != 0 || count != 0 {
		t.Fatal("old authority proof promoted")
	}
	b.reset()
	if reply, _ := b.handle(private.Public(), two, 2, packet, promote); len(reply) != 0 || count != 0 {
		t.Fatal("reset proof promoted")
	}
}
