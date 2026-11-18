package main

import (
	"context"
	"crypto/ecdh"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/recording"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
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
	out, err = alice.run(ctx, request{Action: "recording-key-grant", TeamID: "team-one", Generation: 1, Sealed: grants[0].Sealed, Viewers: []recordingViewer{{AccountID: b.Account, SharingPublicKey: sharingPublic(t, bob)}}})
	if err != nil {
		t.Fatal(err)
	}
	bobCopy := out.(map[string]any)["grants"].([]recordingGrant)[0].Sealed
	if _, err := carol.run(ctx, request{Action: "recording-key-grant", TeamID: "team-one", Generation: 1, Sealed: bobCopy, Viewers: []recordingViewer{{AccountID: c.Account, SharingPublicKey: sharingPublic(t, carol)}}}); err == nil {
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

	open := request{Action: "recording-open", TeamID: "team-one", RecordingID: id, KeyGeneration: 1, ViewerKey: &recordingSealed{Generation: 1, Sealed: bobCopy}}
	if _, err := carol.run(ctx, request{Action: open.Action, TeamID: open.TeamID, RecordingID: id, KeyGeneration: 1, ViewerKey: open.ViewerKey}); err == nil {
		t.Fatal("another viewer's copy opened the recording")
	}
	out, err = bob.run(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	handle := out.(map[string]string)["handle"]
	tampered := append([]byte(nil), file...)
	tampered[len(tampered)-1] ^= 1
	if _, err := bob.run(ctx, request{Action: "recording-segment", Handle: handle, File: encoded(tampered)}); err == nil {
		t.Fatal("a tampered segment opened")
	}
	out, err = bob.run(ctx, request{Action: "recording-segment", Handle: handle, File: encoded(file)})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(map[string]any); got["text"] != string(text) || got["final"] != true {
		t.Fatalf("segment %+v", got)
	}
	if _, err := bob.run(ctx, request{Action: "recording-close", Handle: handle}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.run(ctx, request{Action: "recording-segment", Handle: handle, File: encoded(file)}); err == nil {
		t.Fatal("a closed handle read a segment")
	}
	// Locking drops open recordings.
	out, _ = bob.run(ctx, open)
	handle = out.(map[string]string)["handle"]
	if _, err := bob.run(ctx, request{Action: "lock"}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.run(ctx, request{Action: "recording-segment", Handle: handle, File: encoded(file)}); err == nil {
		t.Fatal("a recording stayed open after lock")
	}
}

// A holder replaces the team key in the browser worker: the new generation is a successor signed by generation 1's
// signing key (what machines check), every viewer gets a copy of it, an old recording stays readable through the
// chain link, and a member without a copy cannot replace it.
func TestRecordingKeyRotateKeepsOldRecordingsReadable(t *testing.T) {
	ctx := context.Background()
	alice, a := initialized(t)
	defer alice.clear()
	bob, b := secondVault(t, "account-bob")
	defer bob.clear()
	carol, _ := secondVault(t, "account-carol")
	defer carol.clear()
	viewers := []recordingViewer{{AccountID: a.Account, SharingPublicKey: sharingPublic(t, alice)}, {AccountID: b.Account, SharingPublicKey: sharingPublic(t, bob)}}

	out, err := alice.run(ctx, request{Action: "recording-key-create", TeamID: "team-one", Viewers: viewers[:1]})
	if err != nil {
		t.Fatal(err)
	}
	created := out.(map[string]any)
	first := created["key"].(map[string]any)
	copy1 := created["grants"].([]recordingGrant)[0].Sealed

	// A recording made under generation 1.
	public, _ := decode(first["public_key"].(string))
	const id = "rec_00000000-0000-4000-8000-000000000002"
	dataKey, _ := recording.NewDataKey()
	sealedKey, err := recording.SealDataKey(public, id, 1, dataKey)
	if err != nil {
		t.Fatal(err)
	}
	text := recording.EventLine(time.Second, recording.CodeOutput, "before the replacement")
	file, err := recording.EncodeSegment(recording.SegmentHeader{Recording: id, KeyGeneration: 1, SealedKey: sealedKey, Final: true}, dataKey, text)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := carol.run(ctx, request{Action: "recording-key-rotate", TeamID: "team-one", Generation: 1, Sealed: copy1, Viewers: viewers}); err == nil {
		t.Fatal("a member without a copy of the key replaced it")
	}
	out, err = alice.run(ctx, request{Action: "recording-key-rotate", TeamID: "team-one", Generation: 1, Sealed: copy1, Viewers: viewers})
	if err != nil {
		t.Fatal(err)
	}
	rotated := out.(map[string]any)
	second := rotated["key"].(map[string]any)
	if second["generation"] != int64(2) {
		t.Fatalf("generation %v", second["generation"])
	}
	// The new generation is a successor signed by generation 1's signing key.
	decodeKey := func(key map[string]any, generation int64) wire.Key {
		out := wire.Key{Generation: generation}
		out.PublicKey, _ = decode(key["public_key"].(string))
		out.SigningPublicKey, _ = decode(key["signing_public_key"].(string))
		if signature, ok := key["rotation_signature"].(string); ok {
			out.RotationSignature, _ = decode(signature)
		}
		return out
	}
	if !wire.VerifyRotation("team-one", decodeKey(first, 1), decodeKey(second, 2)) {
		t.Fatal("the replacement is not a signed successor of generation 1")
	}
	grants := rotated["grants"].([]recordingGrant)
	if len(grants) != 2 {
		t.Fatalf("every viewer gets a copy of the new generation: %d", len(grants))
	}
	// Bob holds only the new copy and the chain link: the old recording opens, and so does the new generation.
	var bobCopy2 string
	for _, grant := range grants {
		if grant.AccountID == b.Account {
			bobCopy2 = grant.Sealed
		}
	}
	open := request{Action: "recording-open", TeamID: "team-one", RecordingID: id, KeyGeneration: 1, ViewerKey: &recordingSealed{Generation: 2, Sealed: bobCopy2}, Chain: []recordingLink{{Generation: 2, SealedPrevious: rotated["sealed_previous"].(string)}}}
	out, err = bob.run(ctx, open)
	if err != nil {
		t.Fatalf("a recording made before the replacement must stay readable: %v", err)
	}
	handle := out.(map[string]string)["handle"]
	read, err := bob.run(ctx, request{Action: "recording-segment", Handle: handle, File: encoded(file)})
	if err != nil || read.(map[string]any)["text"] != string(text) {
		t.Fatalf("segment %v %v", read, err)
	}
	// Without the chain link the old generation does not open.
	open.Chain = nil
	if _, err := bob.run(ctx, open); err == nil {
		t.Fatal("generation 1 opened without the chain link")
	}
}
