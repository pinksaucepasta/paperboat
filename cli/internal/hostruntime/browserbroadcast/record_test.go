package browserbroadcast

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
)

func TestCOSERecordAuthenticatedAndBound(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := NewEpoch()
	if err != nil {
		t.Fatal(err)
	}
	want := Record{SessionID: "umts_test", Generation: 7, EpochID: epoch.ID, Index: 1, Channel: 1, StartSequence: 43, Data: []byte("\x1b[38;2;128;0;255mhello")}
	raw, err := Seal(want, epoch, private)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(raw, epoch, public)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != want.SessionID || got.Generation != want.Generation || got.EpochID != want.EpochID || got.Index != want.Index || got.Channel != want.Channel || got.StartSequence != want.StartSequence || !bytes.Equal(got.Data, want.Data) {
		t.Fatalf("record mismatch: %+v", got)
	}
	modified := bytes.Clone(raw)
	modified[len(modified)-1] ^= 1
	if _, err := Open(modified, epoch, public); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("tamper accepted: %v", err)
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(raw, epoch, otherPublic); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("wrong device accepted: %v", err)
	}
	otherEpoch, err := NewEpoch()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(raw, otherEpoch, public); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("wrong epoch accepted: %v", err)
	}
}

func TestCOSERecordRejectsInvalidAndOversized(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := NewEpoch()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []Record{
		{SessionID: "umts_test", Generation: 1, EpochID: epoch.ID, Index: 0, Channel: 1, Data: []byte("x")},
		{SessionID: "umts_test", Generation: 1, EpochID: epoch.ID, Index: 1, Channel: 1, Data: make([]byte, MaxDataBytes+1)},
		{SessionID: "", Generation: 1, EpochID: epoch.ID, Index: 1, Channel: 1, Data: []byte("x")},
	} {
		if _, err := Seal(value, epoch, private); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("invalid record accepted: %v", err)
		}
	}
}

func TestSharedOutputCompressesBeforeEncryption(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := NewEpoch()
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("\x1b[38;2;128;0;255mterminal frame\x1b[0m\n"), 2048)
	wire, err := Seal(Record{SessionID: "umts_color", Generation: 1, EpochID: epoch.ID, Index: 1, Channel: 1, Data: data}, epoch, private)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) >= len(data)/4 {
		t.Fatalf("repetitive output was not compressed: wire=%d plain=%d", len(wire), len(data))
	}
	decoded, err := Open(wire, epoch, public)
	if err != nil || !bytes.Equal(decoded.Data, data) {
		t.Fatalf("compressed output changed: %v", err)
	}
}
