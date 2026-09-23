// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/fxamacker/cbor/v2"
	"github.com/tailscale/wireguard-go/device"
	"go4.org/mem"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/types/key"
)

// Addr is a compact, URL-safe tailcat address that a server gives to clients
// so they can connect. It is the "tc"-prefixed base64url encoding of a
// CBOR-encoded [ConnInfo]. A typical Addr looks like "tcomFwWC…".
type Addr string

// ConnInfo describes how to reach a server: its WireGuard and path-discovery
// public keys, WireGuard pre-shared key, and which DERP relay region to use. It is serialized into an
// [Addr] for exchange,
// via the wire types in wire.go.
type ConnInfo struct {
	ServerPublic NodePublic // a key.NodePublic
	// ServerDiscoPublic is the server's public key for path discovery.
	// It is deliberately independent from ServerPublic: disco packets carry
	// this key in cleartext on direct UDP paths, while ServerPublic is the
	// unguessable part of the server's tailcat address.
	ServerDiscoPublic DiscoPublic // a key.DiscoPublic

	// PresharedKey preserves the pinned descriptor wire field. Paperboat-generated
	// descriptors leave it zero; network authority and operation grants authorize
	// access independently of this descriptor.
	PresharedKey PresharedKey

	// Region embeds explicit bootstrap relay metadata. Empty supports direct-only
	// authority; parsing a descriptor never fetches an inventory.
	Region []*tailcfg.DERPRegion `json:",omitempty"`

	// RegionID preserves the pinned wire representation. It does not trigger
	// public relay discovery.
	RegionID tailcfg.DERPRegionID `json:",omitempty"`
}

// NodePublic is a wrapper around key.NodePublic just so we can have a slightly
// smaller CBOR representation without the "np" prefix.
type NodePublic struct {
	key.NodePublic
}

// DiscoPublic is a wrapper around key.DiscoPublic that uses its raw 32-byte
// representation in tailcat addresses.
type DiscoPublic struct {
	key.DiscoPublic
}

const presharedKeyLen = device.NoisePresharedKeySize

// PresharedKey retains the 256-bit field in the pinned descriptor codec.
// The Paperboat engine does not generate or consume descriptor preshared keys.
type PresharedKey device.NoisePresharedKey

// IsZero reports whether p is the zero value.
func (p PresharedKey) IsZero() bool {
	var zero PresharedKey
	return subtle.ConstantTimeCompare(p[:], zero[:]) == 1
}

// Equal reports whether p and q contain the same key.
func (p PresharedKey) Equal(q PresharedKey) bool {
	return subtle.ConstantTimeCompare(p[:], q[:]) == 1
}

// MarshalBinary implements encoding.BinaryMarshaler for CBOR serialization.
func (p PresharedKey) MarshalBinary() ([]byte, error) {
	return append([]byte(nil), p[:]...), nil
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler for CBOR serialization.
func (p *PresharedKey) UnmarshalBinary(x []byte) error {
	if len(x) != presharedKeyLen {
		return fmt.Errorf("invalid WireGuard pre-shared key length %d, want %d", len(x), presharedKeyLen)
	}
	copy(p[:], x)
	return nil
}

// MarshalText implements encoding.TextMarshaler for JSON serialization.
func (p PresharedKey) MarshalText() ([]byte, error) {
	ret := make([]byte, len("psk:")+hex.EncodedLen(len(p)))
	copy(ret, "psk:")
	hex.Encode(ret[len("psk:"):], p[:])
	return ret, nil
}

// UnmarshalText implements encoding.TextUnmarshaler for JSON serialization.
func (p *PresharedKey) UnmarshalText(x []byte) error {
	const prefix = "psk:"
	if len(x) != len(prefix)+hex.EncodedLen(presharedKeyLen) || string(x[:len(prefix)]) != prefix {
		return errors.New("invalid WireGuard pre-shared key encoding")
	}
	var ret PresharedKey
	if _, err := hex.Decode(ret[:], x[len(prefix):]); err != nil {
		return fmt.Errorf("invalid WireGuard pre-shared key: %w", err)
	}
	*p = ret
	return nil
}

// Equal reports whether a and b represent the same disco public key.
func (a DiscoPublic) Equal(b DiscoPublic) bool {
	return a.DiscoPublic == b.DiscoPublic
}

// MarshalBinary implements encoding.BinaryMarshaler.
func (p DiscoPublic) MarshalBinary() ([]byte, error) {
	return p.DiscoPublic.AppendTo(nil), nil
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler.
func (p *DiscoPublic) UnmarshalBinary(x []byte) error {
	if len(x) != key.DiscoPublicRawLen {
		return fmt.Errorf("invalid disco public key length %d, want %d", len(x), key.DiscoPublicRawLen)
	}
	p.DiscoPublic = key.DiscoPublicFromRaw32(mem.B(x))
	return nil
}

// MarshalBinary implements encoding.BinaryMarshaler for CBOR serialization,
// encoding the raw 32-byte key without the "nodekey:" text prefix.
func (p NodePublic) MarshalBinary() ([]byte, error) {
	return p.NodePublic.AppendTo(nil), nil
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler for CBOR deserialization.
func (p *NodePublic) UnmarshalBinary(x []byte) error {
	if len(x) != key.NodePublicRawLen {
		return fmt.Errorf("invalid node public key length %d, want %d", len(x), key.NodePublicRawLen)
	}
	p.NodePublic = key.NodePublicFromRaw32(mem.B(x))
	return nil
}

// Equal reports whether a and b represent the same public key.
func (a NodePublic) Equal(b NodePublic) bool {
	return a == b
}

func (lb *locoBackend) tailcatAddr() Addr {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if lb.dm == nil || len(lb.dm.Regions) == 0 {
		return "" // Live authority removal has no usable bootstrap descriptor.
	}
	var ci ConnInfo
	ci.ServerPublic = NodePublic{lb.pub}
	ci.ServerDiscoPublic = DiscoPublic{lb.discoPublic()}
	for _, r := range lb.dm.Regions {
		ci.Region = append(ci.Region, r)
	}
	return ci.Addr()
}

// Addr serializes the ConnInfo into a compact [Addr] string.
// It is encoded via the wire types (see wire.go), which drop the
// DERP region fields tailcat doesn't use. Some other fields
// (RegionID, RegionCode, RegionName, node names that are redundant
// next to an explicit HostName) are zeroed before encoding to reduce
// size; [ParseAddr] restores them.
func (ci *ConnInfo) Addr() Addr {
	w := &wireConnInfo{
		ServerPublic: ci.ServerPublic,
		RegionID:     ci.RegionID.Int64(),
	}
	if !ci.ServerDiscoPublic.IsZero() {
		w.ServerDiscoPublic = &ci.ServerDiscoPublic
	}
	if !ci.PresharedKey.IsZero() {
		w.PresharedKey = &ci.PresharedKey
	}
	for _, r := range ci.Region {
		wr := wireRegionOf(r)

		// Remove some fields before encoding to save space. The same
		// transforms are undone on the way back.
		wr.RegionID = 0
		wr.RegionCode = ""
		wr.RegionName = ""
		for _, n := range wr.Nodes {
			n.RegionID = 0
			if n.HostName != "" {
				n.Name = ""
			}
		}
		w.Region = append(w.Region, wr)
	}

	x, err := cbor.Marshal(w)
	if err != nil {
		panic(err)
	}
	return "tc" + Addr(base64.RawURLEncoding.EncodeToString(x))
}

// parseWire decodes an address into its wire form, without restoring the
// fields that [ConnInfo.Addr] elides.
func parseWire(addr Addr) (*wireConnInfo, error) {
	rest, ok := strings.CutPrefix(string(addr), "tc")
	if !ok {
		return nil, errors.New("tailcat address doesn't start with \"tc\"")
	}
	x, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	w := new(wireConnInfo)
	if err := cbor.Unmarshal(x, w); err != nil {
		return nil, fmt.Errorf("CBOR unmarshal: %v", err)
	}
	return w, nil
}

// ParseAddr decodes an [Addr] back into a [ConnInfo], restoring
// fields that were stripped during encoding (RegionID, RegionCode,
// node names).
func ParseAddr(addr Addr) (ConnInfo, error) {
	var zero ConnInfo
	w, err := parseWire(addr)
	if err != nil {
		return zero, err
	}
	ci := ConnInfo{
		ServerPublic: w.ServerPublic,
		RegionID:     tailcfg.DERPRegionID(w.RegionID),
	}
	if w.ServerDiscoPublic != nil {
		ci.ServerDiscoPublic = *w.ServerDiscoPublic
	}
	if w.PresharedKey != nil {
		ci.PresharedKey = *w.PresharedKey
	}
	for i, wr := range w.Region {
		// CBOR nulls decode to nil pointers, and addresses come from
		// untrusted places (a pasted address, a "tailcat=" TXT record),
		// so reject them rather than dereferencing them below.
		if wr == nil {
			return zero, fmt.Errorf("invalid tailcat address: region %d is null", i)
		}
		for j, n := range wr.Nodes {
			if n == nil {
				return zero, fmt.Errorf("invalid tailcat address: region %d node %d is null", i, j)
			}
		}
		ci.Region = append(ci.Region, wr.derpRegion())
	}
	for ri, r := range ci.Region {
		if r.RegionID == 0 {
			r.RegionID = tailcfg.DERPRegionID(ri + 1)
		}
		if r.RegionCode == "" {
			r.RegionCode = fmt.Sprint(r.RegionID)
		}
		for _, n := range r.Nodes {
			if n.Name == "" {
				// Netcheck identifies nodes by Name, so give each a
				// unique one.
				n.Name = n.HostName
			}
			if n.RegionID == 0 {
				n.RegionID = r.RegionID
			}
		}
	}
	return ci, nil
}
