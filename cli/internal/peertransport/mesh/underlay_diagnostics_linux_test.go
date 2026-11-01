//go:build linux

package mesh

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/net/batching"
	"tailscale.com/net/packet"
	"tailscale.com/types/nettype"
)

func TestUnderlayDiagnosticsPreserveBatchedUDP(t *testing.T) {
	listener := underlayListener{PacketListener: nettype.Std{}, engineLabel: "fixture"}
	socket, err := listener.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	batch, ok := socket.(batching.Conn)
	if !ok {
		t.Fatal("diagnostics disabled magicsock UDP batching")
	}
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	deadline := time.Now().Add(time.Second)
	_ = socket.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	payload := make([]byte, 32)
	binary.LittleEndian.PutUint32(payload, 4)
	destination := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	if err := batch.WriteBatchTo([][]byte{payload, payload}, destination, packet.GeneveHeader{}, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		var b [64]byte
		n, _, err := peer.ReadFromUDP(b[:])
		if err != nil || n != len(payload) {
			t.Fatalf("batch datagram lost: n=%d err=%v", n, err)
		}
	}
	source := socket.LocalAddr().(*net.UDPAddr).AddrPort()
	source = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), source.Port())
	if _, err := peer.WriteToUDPAddrPort(payload, source); err != nil {
		t.Fatal(err)
	}
	slab := make([]byte, batching.ReadSlabMultiple)
	packets := make([]batching.ReceivedPacket, batching.MinimumReadBatchSize)
	n, err := batch.ReadBatch(slab, packets)
	if err != nil || n != 1 || packets[0].Size != len(payload) {
		t.Fatalf("batched receive lost: n=%d err=%v", n, err)
	}
}
