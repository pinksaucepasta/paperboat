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

// meowNonceLen is the length of the handshake nonce every ping carries and its meowed echoes: the dialer takes only
// an answer to its own outstanding handshake (not a duplicate or a stale one). A ping or meowed without the nonce
// and the start id is not a handshake message.
const meowNonceLen = 8

// EncodeMeowPing encodes a meow ping packet containing the sender's node public key, disco public key, handshake
// nonce and start id (as on the meowed).
func EncodeMeowPing(nodeKey key.NodePublic, discoKey key.DiscoPublic, nonce, start [meowNonceLen]byte) []byte {
	b := make([]byte, 0, 4+1+key.NodePublicRawLen+key.DiscoPublicRawLen+2*meowNonceLen)
	b = append(b, meowMagic[:]...)
	b = append(b, meowTypePing)
	b = nodeKey.AppendTo(b)
	b = discoKey.AppendTo(b)
	b = append(b, nonce[:]...)
	return append(b, start[:]...)
}

// meowPingStart is the sender's start id a parsed meow ping carries.
func meowPingStart(pkt []byte) (start [meowNonceLen]byte) {
	const at = 4 + 1 + key.NodePublicRawLen + key.DiscoPublicRawLen + meowNonceLen
	copy(start[:], pkt[at:])
	return start
}

// EncodeMeowed encodes a meowed (acknowledgment) packet echoing the ping's nonce, then the answering engine's start
// id (random per engine start, so a dialer learns the peer restarted with the same keys).
func EncodeMeowed(nonce, start [meowNonceLen]byte) []byte {
	b := make([]byte, 0, 4+1+2*meowNonceLen)
	b = append(b, meowMagic[:]...)
	b = append(b, meowTypePong)
	b = append(b, nonce[:]...)
	return append(b, start[:]...)
}

// ParseMeowPing parses a meow ping packet, returning the sender's
// node public key, disco public key and nonce; a ping without the nonce and start id is not accepted. The pkt must have already
// been verified with IsMeowPacket.
func ParseMeowPing(pkt []byte) (nodeKey key.NodePublic, discoKey key.DiscoPublic, nonce [meowNonceLen]byte, ok bool) {
	const keys = 4 + 1 + key.NodePublicRawLen + key.DiscoPublicRawLen
	if len(pkt) < keys+2*meowNonceLen {
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
	copy(nonce[:], pkt[keys:])
	return nodeKey, discoKey, nonce, true
}

// meowedNonce is the nonce a meowed packet echoes and the answering engine's start id; false when it lacks either
// (not an answer to a handshake).
func meowedNonce(pkt []byte) (nonce, start [meowNonceLen]byte, ok bool) {
	if len(pkt) < 5+2*meowNonceLen {
		return nonce, start, false
	}
	copy(nonce[:], pkt[5:])
	copy(start[:], pkt[5+meowNonceLen:])
	return nonce, start, true
}

// IsMeowedPacket reports whether pkt is a meowed (acknowledgment) packet.
func IsMeowedPacket(pkt []byte) bool {
	return len(pkt) >= 5 && [4]byte(pkt[:4]) == meowMagic && pkt[4] == meowTypePong
}
