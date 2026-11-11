package main

import (
	"context"
	"crypto/ecdh"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/recording"
)

func sharingPublic(t *testing.T, e *engine) string {
	t.Helper()
	private, err := ecdh.X25519().NewPrivateKey(e.keys.SharingPrivate)
	if err != nil {
		t.Fatal(err)
	}
	return encoded(private.PublicKey().Bytes())
}

func secondVault(t *testing.T, account string) (*engine, vaultState) {
	t.Helper()
	e := &engine{}
	if _, err := e.run(context.Background(), request{Action: "initialize", Vault: vaultState{Issuer: "https://control.example", Account: account}, Password: account + " password"}); err != nil {
		t.Fatal(err)
	}
	v := stateFrom(t, e.pendingRaw)
	if _, err := e.run(context.Background(), request{Action: "commit", Vault: v}); err != nil {
		t.Fatal(err)
	}
	return e, v
}

// The team recording key is made for its first viewer, shared to the next one by a holder, and opens a
// recording's segments only for a viewer who holds a copy; a tampered segment is refused.
func TestRecordingKeyCreateGrantOpenAndRead(t *testing.T) {
	ctx := context.Background()
	alice, a := initialized(t)
	defer alice.clear()
	bob, b := secondVault(t, "account-bob")
	defer bob.clear()
	carol, c := secondVault(t, "account-carol")
	defer carol.clear()

	out, err := alice.run(ctx, request{Action: "recording-key-create", TeamID: "team-one", Viewers: []recordingViewer{{AccountID: a.Account, SharingPublicKey: sharingPublic(t, alice)}}})
	if err != nil {
		t.Fatal(err)
	}
	created := out.(map[string]any)
	key := created["key"].(map[string]any)
	grants := created["grants"].([]recordingGrant)
	if key["generation"] != 1 || len(grants) != 1 || grants[0].AccountID != a.Account {
		t.Fatalf("create %+v", created)
	}
	out, err = alice.run(ctx, request{Action: "recording-key-grant", Vault: a, TeamID: "team-one", Generation: 1, Sealed: grants[0].Sealed, Viewers: []recordingViewer{{AccountID: b.Account, SharingPublicKey: sharingPublic(t, bob)}}})
	if err != nil {
		t.Fatal(err)
	}
	bobCopy := out.(map[string]any)["grants"].([]recordingGrant)[0].Sealed
	if _, err := carol.run(ctx, request{Action: "recording-key-grant", Vault: c, TeamID: "team-one", Generation: 1, Sealed: bobCopy, Viewers: []recordingViewer{{AccountID: c.Account, SharingPublicKey: sharingPublic(t, carol)}}}); err == nil {
		t.Fatal("a viewer without a copy shared the key")
	}

	// A machine records with the public key.
	public, _ := decode(key["public_key"].(string))
	const id = "rec_00000000-0000-4000-8000-000000000001"
	dataKey, _ := recording.NewDataKey()
	sealedKey, err := recording.SealDataKey(public, id, 1, dataKey)
	if err != nil {
		t.Fatal(err)
	}
	text := recording.EventLine(time.Second, recording.CodeOutput, "hello")
	file, err := recording.EncodeSegment(recording.SegmentHeader{Recording: id, KeyGeneration: 1, SealedKey: sealedKey, Final: true}, dataKey, text)
	if err != nil {
		t.Fatal(err)
	}

	open := request{Action: "recording-open", Vault: b, TeamID: "team-one", RecordingID: id, KeyGeneration: 1, ViewerKey: &recordingSealed{Generation: 1, Sealed: bobCopy}}
	if _, err := carol.run(ctx, request{Action: open.Action, Vault: c, TeamID: open.TeamID, RecordingID: id, KeyGeneration: 1, ViewerKey: open.ViewerKey}); err == nil {
		t.Fatal("another viewer's copy opened the recording")
	}
	out, err = bob.run(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	handle := out.(map[string]string)["handle"]
	tampered := append([]byte(nil), file...)
	tampered[len(tampered)-1] ^= 1
	if _, err := bob.run(ctx, request{Action: "recording-segment", Handle: handle, file: tampered}); err == nil {
		t.Fatal("a tampered segment opened")
	}
	out, err = bob.run(ctx, request{Action: "recording-segment", Handle: handle, file: file})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(map[string]any); got["text"] != string(text) || got["final"] != true {
		t.Fatalf("segment %+v", got)
	}
	if _, err := bob.run(ctx, request{Action: "recording-close", Handle: handle}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.run(ctx, request{Action: "recording-segment", Handle: handle, file: file}); err == nil {
		t.Fatal("a closed handle read a segment")
	}
	// Locking drops open recordings.
	out, _ = bob.run(ctx, open)
	handle = out.(map[string]string)["handle"]
	if _, err := bob.run(ctx, request{Action: "lock"}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.run(ctx, request{Action: "recording-segment", Handle: handle, file: file}); err == nil {
		t.Fatal("a recording stayed open after lock")
	}
}
