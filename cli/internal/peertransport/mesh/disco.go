// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"go4.org/mem"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/types/key"
)

// Meow messages are sent as raw DERP packets (not disco-framed).
// They are identified by a 4-byte magic prefix, followed by a 1-byte
// message type and the message payload.

// meowMagic is the 4-byte prefix for all meow DERP packets.
// It's distinct from WireGuard message types (1-4) and
// disco's "TS💬" magic.
var meowMagic = [4]byte{'m', 'e', 'o', 'w'}

const (
	meowTypePing = 0x01 // client → server
	meowTypePong = 0x02 // server → client ("meowed")
)

// IsMeowPacket reports whether pkt starts with the meow magic prefix.
func IsMeowPacket(pkt []byte) bool {
	return len(pkt) >= 4 && [4]byte(pkt[:4]) == meowMagic
}

// meowNonceLen is the length of the handshake nonce a ping carries and its meowed echoes, so the dialer records
// the region of an answer to its own outstanding handshake only (not a duplicate or a stale one). Receivers read
// keys at fixed offsets and ignore trailing bytes, so a ping or meowed without one still parses.
const meowNonceLen = 8

// EncodeMeowPing encodes a meow ping packet containing the sender's
// node public key, disco public key and handshake nonce.
func EncodeMeowPing(nodeKey key.NodePublic, discoKey key.DiscoPublic, nonce [meowNonceLen]byte) []byte {
	b := make([]byte, 0, 4+1+key.NodePublicRawLen+key.DiscoPublicRawLen+meowNonceLen)
	b = append(b, meowMagic[:]...)
	b = append(b, meowTypePing)
	b = nodeKey.AppendTo(b)
	b = discoKey.AppendTo(b)
	return append(b, nonce[:]...)
}

// EncodeMeowed encodes a meowed (acknowledgment) packet echoing the ping's nonce; a ping without one (zero nonce)
// gets a bare meowed.
func EncodeMeowed(nonce [meowNonceLen]byte) []byte {
	b := make([]byte, 0, 4+1+meowNonceLen)
	b = append(b, meowMagic[:]...)
	b = append(b, meowTypePong)
	if nonce != ([meowNonceLen]byte{}) {
		b = append(b, nonce[:]...)
	}
	return b
}

// ParseMeowPing parses a meow ping packet, returning the sender's
// node public key, disco public key and nonce (zero when the ping has none). The pkt must have already
// been verified with IsMeowPacket.
func ParseMeowPing(pkt []byte) (nodeKey key.NodePublic, discoKey key.DiscoPublic, nonce [meowNonceLen]byte, ok bool) {
	const keys = 4 + 1 + key.NodePublicRawLen + key.DiscoPublicRawLen
	if len(pkt) < keys {
		return nodeKey, discoKey, nonce, false
	}
	if pkt[4] != meowTypePing {
		return nodeKey, discoKey, nonce, false
	}
	discoKey = key.DiscoPublicFromRaw32(mem.B(pkt[5+key.NodePublicRawLen : keys]))
	if discoKey.IsZero() {
		return key.NodePublic{}, key.DiscoPublic{}, nonce, false
	}
	nodeKey = key.NodePublicFromRaw32(mem.B(pkt[5 : 5+key.NodePublicRawLen]))
	if len(pkt) >= keys+meowNonceLen {
		copy(nonce[:], pkt[keys:])
	}
	return nodeKey, discoKey, nonce, true
}

// meowedNonce is the nonce a meowed packet echoes, if it carries one.
func meowedNonce(pkt []byte) ([meowNonceLen]byte, bool) {
	var nonce [meowNonceLen]byte
	if len(pkt) < 5+meowNonceLen {
		return nonce, false
	}
	copy(nonce[:], pkt[5:])
	return nonce, true
}

// IsMeowedPacket reports whether pkt is a meowed (acknowledgment) packet.
func IsMeowedPacket(pkt []byte) bool {
	return len(pkt) >= 5 && [4]byte(pkt[:4]) == meowMagic && pkt[4] == meowTypePong
}
