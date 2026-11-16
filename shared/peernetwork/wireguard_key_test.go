package peernetwork

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"math/big"
	"testing"
)

// x25519Accepts is the definition ValidWireGuardKey stands for: X25519 with a clamped scalar (a multiple of 8) gives a
// non-zero shared secret, which fails exactly for points whose order divides 8.
func x25519Accepts(raw []byte) bool {
	var scalar [32]byte
	scalar[0] = 1
	private, _ := ecdh.X25519().NewPrivateKey(scalar[:])
	public, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return false
	}
	_, err = private.ECDH(public)
	return err == nil
}

// TestValidWireGuardKeyMatchesX25519 checks the blacklist against X25519 itself: every listed point and its bit-255
// twin is refused by both, every small and non-canonical u (where the low-order encodings live) and random keys get
// the same answer from both.
func TestValidWireGuardKeyMatchesX25519(t *testing.T) {
	check := func(raw []byte) {
		t.Helper()
		key := base64.RawURLEncoding.EncodeToString(raw)
		if got, want := ValidWireGuardKey(key), x25519Accepts(raw); got != want {
			t.Fatalf("ValidWireGuardKey(%x) = %v, X25519 accepts = %v", raw, got, want)
		}
	}
	littleEndian := func(n *big.Int) []byte {
		raw := make([]byte, 32)
		b := n.Bytes()
		for i := range b {
			raw[i] = b[len(b)-1-i]
		}
		return raw
	}
	for _, low := range lowOrderX25519 {
		raw := low
		if x25519Accepts(raw[:]) {
			t.Fatalf("X25519 accepts listed low-order point %x", raw)
		}
		check(raw[:])
		raw[31] |= 0x80
		check(raw[:])
	}
	p := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	for i := range int64(64) {
		check(littleEndian(big.NewInt(i)))
		check(littleEndian(new(big.Int).Add(p, big.NewInt(i-40)))) // p-40 .. p+23, past 2^255 masked by bit 255
	}
	for range 2000 {
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		check(raw)
	}
}
