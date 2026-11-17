// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"bytes"
	"testing"

	"tailscale.com/types/key"
)

func TestMeowPingRoundTrip(t *testing.T) {
	nodeKey := key.NewNode().Public()
	discoKey := key.NewDisco().Public()

	pkt := EncodeMeowPing(nodeKey, discoKey, testNonce, testStart)
	if !IsMeowPacket(pkt) {
		t.Fatal("EncodeMeowPing output is not a meow packet")
	}
	if IsMeowedPacket(pkt) {
		t.Error("a ping was reported as a meowed packet")
	}
	gotNode, gotDisco, _, ok := ParseMeowPing(pkt)
	if !ok {
		t.Fatal("ParseMeowPing failed on its own output")
	}
	if gotNode != nodeKey {
		t.Errorf("node key = %v; want %v", gotNode, nodeKey)
	}
	if gotDisco != discoKey {
		t.Errorf("disco key = %v; want %v", gotDisco, discoKey)
	}
}

func TestMeowedRoundTrip(t *testing.T) {
	pkt := EncodeMeowed(testNonce, testStart)
	if !IsMeowPacket(pkt) {
		t.Fatal("EncodeMeowed output is not a meow packet")
	}
	if !IsMeowedPacket(pkt) {
		t.Fatal("EncodeMeowed output is not a meowed packet")
	}
	// A meowed packet must never parse as a ping: the receive paths in
	// tailcat.go check IsMeowedPacket first, but ParseMeowPing has to
	// reject it on its own too.
	if _, _, _, ok := ParseMeowPing(pkt); ok {
		t.Error("ParseMeowPing accepted a meowed packet")
	}
}

func TestIsMeowPacket(t *testing.T) {
	ping := EncodeMeowPing(key.NewNode().Public(), key.NewDisco().Public(), testNonce, testStart)
	tests := []struct {
		name string
		pkt  []byte
		want bool
	}{
		{"nil", nil, false},
		{"empty", []byte{}, false},
		{"short", []byte("meo"), false},
		{"magic_only", []byte("meow"), true},
		{"wrong_magic", []byte("woem\x01"), false},
		{"wireguard_type1", []byte{1, 0, 0, 0}, false},
		{"disco_magic", []byte("TS\xf0\x9f\x92\xac"), false},
		{"ping", ping, true},
		{"meowed", EncodeMeowed(testNonce, testStart), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMeowPacket(tt.pkt); got != tt.want {
				t.Errorf("IsMeowPacket(%q) = %v; want %v", tt.pkt, got, tt.want)
			}
		})
	}
}

func TestIsMeowedPacket(t *testing.T) {
	tests := []struct {
		name string
		pkt  []byte
		want bool
	}{
		{"nil", nil, false},
		{"magic_only", []byte("meow"), false},
		{"meowed", EncodeMeowed(testNonce, testStart), true},
		{"ping_type", []byte("meow\x01"), false},
		{"unknown_type", []byte("meow\x03"), false},
		{"wrong_magic", []byte("woem\x02"), false},
		{"meowed_with_trailer", append(EncodeMeowed(testNonce, testStart), 0xff), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMeowedPacket(tt.pkt); got != tt.want {
				t.Errorf("IsMeowedPacket(%q) = %v; want %v", tt.pkt, got, tt.want)
			}
		})
	}
}

// TestParseMeowPingMalformed checks that ParseMeowPing rejects short and
// malformed packets rather than panicking or returning partial keys. These
// packets arrive over DERP from unauthenticated senders.
func TestParseMeowPingMalformed(t *testing.T) {
	full := EncodeMeowPing(key.NewNode().Public(), key.NewDisco().Public(), testNonce, testStart)
	zeroDisco := EncodeMeowPing(key.NewNode().Public(), key.DiscoPublic{}, testNonce, testStart)
	if len(zeroDisco) != 85 {
		t.Fatalf("zero-disco meow is %d bytes; want 85", len(zeroDisco))
	}

	tests := []struct {
		name string
		pkt  []byte
	}{
		{"nil", nil},
		{"magic_only", []byte("meow")},
		{"type_only", []byte("meow\x01")},
		{"meowed", EncodeMeowed(testNonce, testStart)},
		{"unknown_type", append([]byte("meow\x7f"), full[5:]...)},
		{"truncated_node_key", full[:5+key.NodePublicRawLen-1]},
		{"truncated_disco_key", full[:len(full)-2*meowNonceLen-1]},
		{"no_nonce", full[:len(full)-2*meowNonceLen]},
		{"truncated_nonce", full[:len(full)-meowNonceLen-1]},
		{"no_start", full[:len(full)-meowNonceLen]},
		{"zero_disco_key", zeroDisco},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeKey, discoKey, _, ok := ParseMeowPing(tt.pkt)
			if ok {
				t.Fatalf("ParseMeowPing(%q) = ok; want !ok", tt.pkt)
			}
			var zeroNode key.NodePublic
			var zeroDisco key.DiscoPublic
			if nodeKey != zeroNode || discoKey != zeroDisco {
				t.Errorf("rejected packet returned non-zero keys: node=%v disco=%v", nodeKey, discoKey)
			}
		})
	}

	// Every strict prefix of a valid ping must be rejected.
	for n := range len(full) {
		if _, _, _, ok := ParseMeowPing(full[:n]); ok {
			t.Errorf("ParseMeowPing accepted a %d-byte prefix of a %d-byte ping", n, len(full))
		}
	}
}

// TestParseMeowPingTrailingBytes documents that extra trailing bytes are
// ignored: the keys are read from fixed offsets.
func TestParseMeowPingTrailingBytes(t *testing.T) {
	nodeKey := key.NewNode().Public()
	discoKey := key.NewDisco().Public()
	pkt := append(EncodeMeowPing(nodeKey, discoKey, testNonce, testStart), 'x', 'y', 'z')

	gotNode, gotDisco, _, ok := ParseMeowPing(pkt)
	if !ok {
		t.Fatal("ParseMeowPing rejected a ping with trailing bytes")
	}
	if gotNode != nodeKey || gotDisco != discoKey {
		t.Error("trailing bytes changed the parsed keys")
	}
}

// TestMeowMagicDistinct guards the comment in disco.go: the meow magic must
// not collide with WireGuard message types or disco's magic, since all three
// share the DERP packet path.
func TestMeowMagicDistinct(t *testing.T) {
	magic := EncodeMeowed(testNonce, testStart)[:4]
	for typ := byte(1); typ <= 4; typ++ {
		// WireGuard messages start with a type byte then three zero bytes.
		if bytes.Equal(magic, []byte{typ, 0, 0, 0}) {
			t.Errorf("meow magic collides with WireGuard message type %d", typ)
		}
	}
	if bytes.HasPrefix(magic, []byte("TS")) {
		t.Error("meow magic collides with disco magic")
	}
}

var testNonce = [meowNonceLen]byte{1, 2, 3, 4, 5, 6, 7, 8}

var testStart = [meowNonceLen]byte{9, 10, 11, 12, 13, 14, 15, 16}

// The handshake nonce goes out on the ping and comes back on the meowed, so the dialer can tell an answer to its own
// outstanding handshake from a duplicate or a stale one. A ping without the nonce, and a meowed without the nonce
// and the start id, are not handshake messages.
func TestMeowNonceRoundTrip(t *testing.T) {
	ping := EncodeMeowPing(key.NewNode().Public(), key.NewDisco().Public(), testNonce, testStart)
	if _, _, nonce, ok := ParseMeowPing(ping); !ok || nonce != testNonce {
		t.Fatalf("ping nonce %v ok=%v", nonce, ok)
	}
	answer := EncodeMeowed(testNonce, testStart)
	if nonce, start, ok := meowedNonce(answer); !ok || nonce != testNonce || start != testStart {
		t.Fatalf("meowed echo %v start %v ok=%v", nonce, start, ok)
	}
	if start := meowPingStart(ping); start != testStart {
		t.Fatalf("ping start %v", start)
	}
	if _, _, _, ok := ParseMeowPing(ping[:len(ping)-2*meowNonceLen]); ok {
		t.Fatal("a ping without a nonce was accepted")
	}
	if _, _, _, ok := ParseMeowPing(ping[:len(ping)-meowNonceLen]); ok {
		t.Fatal("a ping without a start id was accepted")
	}
	for _, n := range []int{5, 5 + meowNonceLen, 5 + 2*meowNonceLen - 1} {
		if _, _, ok := meowedNonce(answer[:n]); ok {
			t.Fatalf("a %d-byte meowed was taken as an answer", n)
		}
	}
}
