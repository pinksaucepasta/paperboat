package derpquic

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"tailscale.com/types/key"
)

func fragment(id uint64, k key.NodePublic, data []byte, index int) []byte {
	off := index * fragmentPayload
	end := min(off+fragmentPayload, len(data))
	b := make([]byte, fragmentHeader+end-off)
	binary.BigEndian.PutUint64(b, id)
	copy(b[8:40], k.AppendTo(nil))
	binary.BigEndian.PutUint16(b[40:42], uint16(len(data)))
	b[42] = byte(index)
	copy(b[43:], data[off:end])
	return b
}
func TestReassemblyReorderDuplicateReplay(t *testing.T) {
	var r reassembler
	k := key.NewNode().Public()
	data := bytes.Repeat([]byte{0xa5}, 1312)
	now := time.Now()
	last := fragment(1, k, data, 1)
	for range 2 {
		if _, ok, e := r.accept(last, now, nil); e != nil || ok {
			t.Fatalf("premature completion: %v %v", ok, e)
		}
	}
	p, ok, e := r.accept(fragment(1, k, data, 0), now, nil)
	if e != nil || !ok || p.peer != k || !bytes.Equal(p.data, data) {
		t.Fatal("reordered packet did not preserve bytes and identity")
	}
	for _, i := range []int{0, 1} {
		if _, ok, e := r.accept(fragment(1, k, data, i), now, nil); e != nil || ok {
			t.Fatal("completed packet replay was accepted")
		}
	}
	if _, ok, e := r.accept(fragment(130, k, []byte{1}, 0), now, nil); e != nil || !ok {
		t.Fatal("new sequence rejected")
	}
	if _, ok, e := r.accept(fragment(2, k, []byte{1}, 0), now, nil); e != nil || ok {
		t.Fatal("old sequence replay accepted")
	}
}
func TestReassemblyAuthorizationAndBounds(t *testing.T) {
	now := time.Now()
	k := key.NewNode().Public()
	data := make([]byte, MaxPacket)
	var r reassembler
	if _, ok, e := r.accept(fragment(1, k, data, 0), now, func(key.NodePublic) bool { return false }); e != nil || ok || len(r.pending) != 0 || r.highest != 0 {
		t.Fatal("unauthorized fragment allocated state")
	}
	for id := uint64(1); id <= maxAssemblies+1; id++ {
		if _, ok, e := r.accept(fragment(id, k, data, 0), now, nil); e != nil || ok {
			t.Fatal("incomplete packet unexpectedly completed")
		}
	}
	if len(r.pending) != maxAssemblies {
		t.Fatalf("pending bound: %d", len(r.pending))
	}
	if _, _, e := r.accept(fragment(maxAssemblies+2, k, data, 0), now.Add(3*time.Second), nil); e != nil {
		t.Fatal(e)
	}
	if len(r.pending) != 1 {
		t.Fatalf("expired assemblies retained: %d", len(r.pending))
	}
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
	}{
		{"zero_id", func(b []byte) []byte { clear(b[:8]); return b }},
		{"zero_size", func(b []byte) []byte { clear(b[40:42]); return b }},
		{"oversize", func(b []byte) []byte { binary.BigEndian.PutUint16(b[40:42], MaxPacket+1); return b }},
		{"index", func(b []byte) []byte { b[42] = 2; return b }},
		{"short", func(b []byte) []byte { return b[:len(b)-1] }},
		{"long", func(b []byte) []byte { return append(b, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r reassembler
			if _, _, e := r.accept(tc.edit(fragment(1, k, data, 0)), now, nil); !errors.Is(e, ErrProtocol) {
				t.Fatalf("invalid fragment: %v", e)
			}
		})
	}
	var mismatch reassembler
	mismatch.accept(fragment(1, k, data, 0), now, nil)
	if _, _, e := mismatch.accept(fragment(1, key.NewNode().Public(), data, 1), now, nil); !errors.Is(e, ErrProtocol) {
		t.Fatal("fragment peer mismatch not rejected")
	}
}
func TestFrameAndControlBounds(t *testing.T) {
	var b bytes.Buffer
	if e := writeFrame(&b, frameAuth, make([]byte, MaxControl+1)); !errors.Is(e, ErrProtocol) || b.Len() != 0 {
		t.Fatal("oversized write accepted")
	}
	var h [5]byte
	binary.BigEndian.PutUint32(h[1:], MaxControl+1)
	if _, _, e := readFrame(bytes.NewReader(h[:])); !errors.Is(e, ErrProtocol) {
		t.Fatal("oversized frame accepted")
	}
	for _, n := range []int{0, 32, MaxPacket + 33} {
		if _, e := decodeControl(make([]byte, n)); !errors.Is(e, ErrProtocol) {
			t.Fatalf("control size %d accepted", n)
		}
	}
	p := packet{peer: key.NewNode().Public(), data: []byte("control"), control: true}
	decoded, e := decodeControl(encodeControl(p))
	if e != nil || decoded.peer != p.peer || !bytes.Equal(decoded.data, p.data) || !decoded.control {
		t.Fatal("control round trip failed")
	}
}
func TestBudgetBoundAndRecovery(t *testing.T) {
	b := newBudget(carrierBytesPerSecond)
	now := time.Now()
	for range budgetBurstBytes / budgetMinimumCost {
		if !b.allow(now, 1) {
			t.Fatal("burst ended early")
		}
	}
	if b.allow(now, 1) {
		t.Fatal("burst exceeded bound")
	}
	if !b.allow(now.Add(time.Second), MaxControl) {
		t.Fatal("budget did not recover")
	}
	// A second of traffic is bounded by the rate plus the burst, counted in bytes.
	b, sent := newBudget(carrierBytesPerSecond), 0
	for at := now; at.Before(now.Add(time.Second)); at = at.Add(time.Microsecond) {
		if b.allow(at, 1400) {
			sent += 1400
		}
	}
	if sent > carrierBytesPerSecond+budgetBurstBytes || sent < carrierBytesPerSecond {
		t.Fatalf("one second forwarded %d bytes", sent)
	}
}

func wholeDatagram(id uint64, k key.NodePublic, data []byte) []byte {
	b := fragment(id, k, data[:1], 0)[:fragmentHeader]
	binary.BigEndian.PutUint16(b[40:42], uint16(len(data)))
	return append(b, data...)
}

func TestReassemblyWholePacketInOneDatagram(t *testing.T) {
	var r reassembler
	k := key.NewNode().Public()
	now := time.Now()
	data := bytes.Repeat([]byte{7}, 1312)
	p, ok, e := r.accept(wholeDatagram(1, k, data), now, nil)
	if e != nil || !ok || p.peer != k || !bytes.Equal(p.data, data) || len(r.pending) != 0 {
		t.Fatalf("whole datagram: %v %v", ok, e)
	}
	if _, ok, e := r.accept(wholeDatagram(1, k, data), now, nil); e != nil || ok {
		t.Fatal("whole datagram replay was accepted")
	}
	// The second part can never be the whole packet, and a whole packet is at most MaxPacket.
	bad := wholeDatagram(2, k, data)
	bad[42] = 1
	if _, _, e := r.accept(bad, now, nil); !errors.Is(e, ErrProtocol) {
		t.Fatalf("whole datagram as index 1: %v", e)
	}
	if _, ok, e := r.accept(wholeDatagram(3, k, make([]byte, MaxPacket)), now, nil); e != nil || !ok {
		t.Fatalf("MaxPacket whole datagram: %v %v", ok, e)
	}
}

// carrierPair is a loopback QUIC carrier; the client's sendPacket output arrives as the server's raw datagrams.
func carrierPair(t *testing.T, disablePMTUD bool) (client *quic.Conn, datagrams <-chan []byte) {
	t.Helper()
	cert := certificate(t)
	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	ln, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{ALPN}}, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg := quicConfig()
	cfg.DisablePathMTUDiscovery = disablePMTUD
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err = quic.DialAddr(ctx, ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "localhost", NextProtos: []string{ALPN}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.CloseWithError(0, "") })
	server, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan []byte, 256)
	go func() {
		for {
			b, err := server.ReceiveDatagram(context.Background())
			if err != nil {
				return
			}
			out <- b
		}
	}()
	return client, out
}

// received collects the datagrams of one packet (one or two) and checks they reassemble to data.
func received(t *testing.T, datagrams <-chan []byte, k key.NodePublic, data []byte) (count int) {
	t.Helper()
	var r reassembler
	for {
		select {
		case b := <-datagrams:
			count++
			p, ok, err := r.accept(b, time.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				if p.peer != k || !bytes.Equal(p.data, data) {
					t.Fatal("packet changed in transit")
				}
				return count
			}
		case <-time.After(2 * time.Second):
			t.Fatal("packet not received")
		}
	}
}

func TestSendPacketFragmentsOnlyWhenItDoesNotFit(t *testing.T) {
	k := key.NewNode().Public()
	data := bytes.Repeat([]byte{9}, 1312)
	t.Run("does not fit", func(t *testing.T) {
		// Without path MTU discovery the carrier stays at its initial packet size, below 1312 + header.
		conn, datagrams := carrierPair(t, true)
		var seq atomic.Uint64
		if err := sendPacket(conn, &seq, packet{peer: k, data: data}); err != nil {
			t.Fatal(err)
		}
		if n := received(t, datagrams, k, data); n != 2 {
			t.Fatalf("a packet that does not fit one datagram arrived as %d datagrams, want 2", n)
		}
	})
	t.Run("fits", func(t *testing.T) {
		// quic-go grows the path MTU on loopback to its maximum; once it did, the packet is one datagram.
		conn, datagrams := carrierPair(t, false)
		var seq atomic.Uint64
		deadline := time.Now().Add(10 * time.Second)
		for {
			if err := sendPacket(conn, &seq, packet{peer: k, data: data}); err != nil {
				t.Fatal(err)
			}
			if n := received(t, datagrams, k, data); n == 1 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("path MTU discovery never let a 1312-byte packet go in one datagram")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}
