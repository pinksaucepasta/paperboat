// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"crypto/hmac"
	"crypto/sha256"

	go4mem "go4.org/mem"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/types/key"
)

// discoPrivateForNode deterministically derives a path-discovery key from a
// node private key. Keeping the two public keys unlinkable is a security
// boundary: disco frames expose the disco public key on the local network,
// while Paperboat binds the node key to signed network authority.
func discoPrivateForNode(k key.NodePrivate) key.DiscoPrivate {
	raw := k.Raw32()
	mac := hmac.New(sha256.New, raw[:])
	mac.Write([]byte("github.com/tailscale/tailcat disco key v1"))
	discoRaw := mac.Sum(nil)
	// Canonicalize the Curve25519 scalar, as key.NewDisco does.
	discoRaw[0] &= 248
	discoRaw[31] &= 127
	discoRaw[31] |= 64
	return key.DiscoPrivateFromRaw32(go4mem.B(discoRaw))
}

// DiscoPrivateForNode returns the deterministic discovery key used by this
// engine. Callers use it only to host an authority-approved UDP relay service.
func DiscoPrivateForNode(k key.NodePrivate) key.DiscoPrivate { return discoPrivateForNode(k) }

// DiscoPublicForNode returns the path-discovery public key derived from a
// node private key. Code constructing a [ConnInfo] directly must include it
// as ConnInfo.ServerDiscoPublic.
func DiscoPublicForNode(k key.NodePrivate) DiscoPublic {
	return DiscoPublic{discoPrivateForNode(k).Public()}
}
