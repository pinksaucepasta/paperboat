package derpquic

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func fragment(id uint64, k key.NodePublic, data []byte, index int) []byte {
	off := index * fragmentPayload
	end := min(off+fragmentPayload, len(data))
	b := make([]byte, fragmentHeader+end-off)
	binary.BigEndian.PutUint64(b, id)
	raw := k.Raw32()
	copy(b[8:40], raw[:])
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
	var b budget
	now := time.Now()
	for range 256 {
		if !b.allow(now) {
			t.Fatal("burst ended early")
		}
	}
	if b.allow(now) {
		t.Fatal("burst exceeded bound")
	}
	if !b.allow(now.Add(time.Second)) {
		t.Fatal("budget did not recover")
	}
}
