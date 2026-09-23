// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/go-cmp/cmp"
	"go4.org/mem"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

func TestAddr(t *testing.T) {
	akey := func(a [32]byte) NodePublic {
		return NodePublic{key.NodePublicFromRaw32(mem.B(a[:]))}
	}
	tests := []struct {
		name string
		ci   ConnInfo
		want Addr      // if non-empty, check exact encoding
		back *ConnInfo // if non-nil, round-tripped form we want
	}{
		{
			name: "just_key",
			ci: ConnInfo{
				ServerPublic: akey([32]byte{1: 1, 2: 2, 31: 31}),
			},
			want: "tcoWFwWCAAAQIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAHw",
		},
		{
			name: "key_with_full_custom_region",
			ci: ConnInfo{
				ServerPublic: akey([32]byte{1: 1, 2: 2, 31: 31}),
				Region: []*tailcfg.DERPRegion{
					{
						Nodes: []*tailcfg.DERPNode{
							{
								Name:     "1a",
								IPv4:     "400.400.400.400",
								HostName: "my-derp.custom.example",
							},
							{
								Name:     "1b",
								IPv4:     "400.400.400.400",
								HostName: "my-derp2.custom.example",
							},
						},
					},
				},
			},
			back: &ConnInfo{
				ServerPublic: akey([32]byte{1: 1, 2: 2, 31: 31}),
				Region: []*tailcfg.DERPRegion{
					{
						RegionID:   1,
						RegionCode: "1",
						Nodes: []*tailcfg.DERPNode{
							{
								RegionID: 1,
								Name:     "my-derp.custom.example",
								IPv4:     "400.400.400.400",
								HostName: "my-derp.custom.example",
							},
							{
								RegionID: 1,
								Name:     "my-derp2.custom.example",
								IPv4:     "400.400.400.400",
								HostName: "my-derp2.custom.example",
							},
						},
					},
				},
			},
		},

		{
			name: "remove_implicit_fields_on_marshal",
			ci: ConnInfo{
				ServerPublic: akey([32]byte{1: 1, 2: 2, 31: 31}),
				Region: []*tailcfg.DERPRegion{
					{
						RegionID:   123,
						RegionName: "Seattle",
						Nodes: []*tailcfg.DERPNode{
							{
								RegionID: 123,
								Name:     "1a",
								HostName: "tc1a.ipn.dev",
							},
							{
								RegionID: 123,
								Name:     "1b",
								HostName: "derp1b.tailscale.com",
							},
						},
					},
				},
			},
			back: &ConnInfo{
				ServerPublic: akey([32]byte{1: 1, 2: 2, 31: 31}),
				Region: []*tailcfg.DERPRegion{
					{
						RegionID:   1,
						RegionCode: "1",
						Nodes: []*tailcfg.DERPNode{
							{
								RegionID: 1,
								Name:     "tc1a.ipn.dev",
								HostName: "tc1a.ipn.dev",
							},
							{
								RegionID: 1,
								Name:     "derp1b.tailscale.com",
								HostName: "derp1b.tailscale.com",
							},
						},
					},
				},
			},
		},

		{
			name: "region_id",
			ci: ConnInfo{
				ServerPublic: akey([32]byte{1: 1, 2: 2, 31: 31}),
				RegionID:     10,
			},
			want: "tcomFwWCAAAQIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAH2FpCg",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ci.Addr()
			t.Logf("length: %v (%v)", len(got), got)
			if tt.want != "" && got != tt.want {
				t.Fatalf("ConnInfo.Addr marshal wrong.\n got: %s\nwant: %s\n", got, tt.want)
			}

			gotCI, err := ParseAddr(got)
			if err != nil {
				t.Fatalf("ParseAddr: %v", err)
			}
			want := tt.ci
			if tt.back != nil {
				want = *tt.back
			}
			if diff := cmp.Diff(want, gotCI); diff != "" {
				t.Errorf("ParseAddr result back diff:\n%s", diff)
			}
		})
	}
}

func TestAddrSeparateDiscoKey(t *testing.T) {
	priv := key.NewNode()
	discoPub := DiscoPublicForNode(priv)
	ci := ConnInfo{
		ServerPublic:      NodePublic{priv.Public()},
		ServerDiscoPublic: discoPub,
		RegionID:          10,
	}
	got, err := ParseAddr(ci.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if !got.ServerDiscoPublic.Equal(discoPub) {
		t.Fatalf("disco public key changed in round trip: got %v, want %v", got.ServerDiscoPublic, discoPub)
	}
	if got.ServerDiscoPublic.Raw32() == got.ServerPublic.Raw32() {
		t.Fatal("server disco public key exposes the server node public key")
	}
	if again := DiscoPublicForNode(priv); !again.Equal(discoPub) {
		t.Fatal("disco key derivation is not stable")
	}
	// Reproduce the report's reconstruction strategy: treating the public
	// key visible in a direct-path disco frame as the server node key must
	// not recover the tailcat address.
	reconstructed := (&ConnInfo{
		ServerPublic: NodePublic{key.NodePublicFromRaw32(mem.B(discoPub.AppendTo(nil)))},
		RegionID:     ci.RegionID,
	}).Addr()
	if reconstructed == ci.Addr() {
		t.Fatal("disco public key can reconstruct the tailcat address")
	}
}

func TestAddrPresharedKey(t *testing.T) {
	psk := PresharedKey{1, 2, 3}
	if psk.IsZero() {
		t.Fatal("preshared-key fixture is zero")
	}
	priv := struct{ Public ConnInfo }{Public: ConnInfo{ServerPublic: NodePublic{key.NewNode().Public()}}}
	priv.Public.PresharedKey = psk
	priv.Public.RegionID = 10

	got, err := ParseAddr(priv.Public.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if !got.PresharedKey.Equal(psk) {
		t.Fatalf("pre-shared key changed in address round trip: got %x, want %x", got.PresharedKey, psk)
	}

	j, err := json.Marshal(priv)
	if err != nil {
		t.Fatal(err)
	}
	var back struct{ Public ConnInfo }
	if err := json.Unmarshal(j, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Public.PresharedKey.Equal(psk) {
		t.Fatalf("pre-shared key changed in JSON round trip: got %x, want %x", back.Public.PresharedKey, psk)
	}
	if !strings.Contains(string(j), `"PresharedKey":"psk:`) {
		t.Fatalf("JSON pre-shared key is not in typed text form: %s", j)
	}

	withoutPSK := priv.Public
	withoutPSK.PresharedKey = PresharedKey{}
	if got, want := len(withoutPSK.Addr()), len(priv.Public.Addr()); got >= want {
		t.Errorf("address without PSK length = %d; want less than %d", got, want)
	}
}

func TestParseAddrMalformedPublicKey(t *testing.T) {
	for name, keyBytes := range map[string][]byte{
		"short": make([]byte, key.NodePublicRawLen-1),
		"long":  make([]byte, key.NodePublicRawLen+1),
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := cbor.Marshal(map[string][]byte{"p": keyBytes})
			if err != nil {
				t.Fatal(err)
			}
			addr := Addr("tc" + base64.RawURLEncoding.EncodeToString(raw))
			assertParseError := func(name string, parse func() error) {
				t.Helper()
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s panicked: %v", name, r)
					}
				}()
				if err := parse(); err == nil {
					t.Errorf("%s unexpectedly accepted malformed public key", name)
				}
			}
			assertParseError("ParseAddr", func() error {
				_, err := ParseAddr(addr)
				return err
			})
			assertParseError("parseWire", func() error {
				_, err := parseWire(addr)
				return err
			})
		})
	}
}

func TestParseAddrMalformedDiscoPublicKey(t *testing.T) {
	for name, keyBytes := range map[string][]byte{
		"short": make([]byte, key.DiscoPublicRawLen-1),
		"long":  make([]byte, key.DiscoPublicRawLen+1),
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := cbor.Marshal(map[string][]byte{
				"p": key.NewNode().Public().AppendTo(nil),
				"k": keyBytes,
			})
			if err != nil {
				t.Fatal(err)
			}
			addr := Addr("tc" + base64.RawURLEncoding.EncodeToString(raw))
			if _, err := ParseAddr(addr); err == nil {
				t.Fatal("ParseAddr unexpectedly accepted malformed disco public key")
			}
		})
	}
}

func TestParseAddrMalformedPresharedKey(t *testing.T) {
	for name, keyBytes := range map[string][]byte{
		"short": make([]byte, presharedKeyLen-1),
		"long":  make([]byte, presharedKeyLen+1),
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := cbor.Marshal(map[string][]byte{
				"p": key.NewNode().Public().AppendTo(nil),
				"q": keyBytes,
			})
			if err != nil {
				t.Fatal(err)
			}
			addr := Addr("tc" + base64.RawURLEncoding.EncodeToString(raw))
			if _, err := ParseAddr(addr); err == nil {
				t.Fatal("ParseAddr unexpectedly accepted malformed pre-shared key")
			}
		})
	}
}

// TestParseAddrNullInArrays checks that ParseAddr rejects addresses whose
// region or node arrays contain a CBOR null. Those decode to nil pointers, and
// before this was checked they panicked when dereferenced. Addrs come from
// untrusted places, so a panic here takes down the process.
func TestParseAddrNullInArrays(t *testing.T) {
	addr := func(t *testing.T, m map[string]any) Addr {
		t.Helper()
		b, err := cbor.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return Addr("tc" + base64.RawURLEncoding.EncodeToString(b))
	}
	pub := key.NewNode().Public().AppendTo(nil)

	tests := []struct {
		name string
		addr Addr
	}{
		{
			name: "null_region",
			addr: addr(t, map[string]any{"p": pub, "r": []any{nil}}),
		},
		{
			name: "null_node",
			addr: addr(t, map[string]any{"p": pub, "r": []any{
				map[string]any{"i": 1, "N": []any{nil}},
			}}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseAddr(tt.addr); err == nil {
				t.Fatal("ParseAddr succeeded; want an error")
			}
		})
	}
}
