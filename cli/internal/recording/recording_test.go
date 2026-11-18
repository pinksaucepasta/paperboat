package recording

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
	"strings"
	"testing"
	"time"
)

func teamKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSegmentRoundTripAndTamper(t *testing.T) {
	team := teamKey(t)
	dataKey, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealDataKey(team.PublicKey().Bytes(), "rec_1", 3, dataKey)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := append(EventLine(1500*time.Millisecond, CodeOutput, "héllo <\x1b[0m>"), EventLine(2*time.Second, CodeResize, ResizeData(120, 40))...)
	file, err := EncodeSegment(SegmentHeader{Recording: "rec_1", Index: 2, KeyGeneration: 3, SealedKey: sealed, Final: true, EndReason: "stopped"}, dataKey, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(file, []byte("llo")) {
		t.Fatal("segment holds plaintext")
	}
	header, err := ParseSegmentHeader(file)
	if err != nil || header.Index != 2 || !header.Final {
		t.Fatalf("header %+v %v", header, err)
	}
	opened, err := OpenDataKey(team.Bytes(), "rec_1", 3, header.SealedKey)
	if err != nil || !bytes.Equal(opened, dataKey) {
		t.Fatal("data key did not open")
	}
	if _, err := OpenDataKey(team.Bytes(), "rec_2", 3, header.SealedKey); err == nil {
		t.Fatal("a sealed key opened for another recording")
	}
	if _, err := OpenDataKey(teamKey(t).Bytes(), "rec_1", 3, header.SealedKey); err == nil {
		t.Fatal("a sealed key opened with another team key")
	}
	_, got, err := OpenSegment(file, dataKey)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("open %q %v", got, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	event, err := ParseEvent([]byte(lines[0]))
	if err != nil || event.Seconds != 1.5 || event.Code != CodeOutput || event.Data != "héllo <\x1b[0m>" {
		t.Fatalf("event %+v %v", event, err)
	}
	// Flipping the final flag (header) or one ciphertext byte must fail authentication.
	for _, tamper := range []func([]byte) []byte{
		func(f []byte) []byte { return bytes.Replace(f, []byte(`"final":true`), []byte(`"final":fals`), 1) },
		func(f []byte) []byte { f[len(f)-1] ^= 1; return f },
	} {
		if _, _, err := OpenSegment(tamper(bytes.Clone(file)), dataKey); err == nil {
			t.Fatal("tampered segment opened")
		}
	}
}

func TestHeaderLineIsAsciicastV2(t *testing.T) {
	line, err := HeaderLine(Header{Width: 80, Height: 24, Timestamp: 1, Paperboat: HeaderInfo{Recording: "rec_1", Session: "s", Trigger: "manual"}})
	if err != nil || !bytes.HasPrefix(line, []byte(`{"version":2,"width":80,"height":24`)) || line[len(line)-1] != '\n' {
		t.Fatalf("%s %v", line, err)
	}
}

func newBundle(t *testing.T) (Bundle, []byte) {
	t.Helper()
	private := teamKey(t)
	_, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Bundle{Private: private.Bytes(), Signing: signing}, private.PublicKey().Bytes()
}

// A viewer holds generation 3's bundle; the chain opens generation 1, whose key sealed an old
// recording, and nothing opens for another team or generation.
func TestRecordingKeyOpensThroughTheChain(t *testing.T) {
	viewer := teamKey(t)
	first, firstPublic := newBundle(t)
	second, secondPublic := newBundle(t)
	third, thirdPublic := newBundle(t)
	_ = firstPublic
	copy3, err := SealBundle(viewer.PublicKey().Bytes(), "team_1", 3, third)
	if err != nil {
		t.Fatal(err)
	}
	link3, _ := SealBundle(thirdPublic, "team_1", 2, second)
	link2, _ := SealBundle(secondPublic, "team_1", 1, first)
	chain := []ChainLink{{Generation: 3, SealedPrevious: link3}, {Generation: 2, SealedPrevious: link2}}
	got, err := OpenRecordingKey(viewer.Bytes(), "team_1", 3, copy3, chain, 1)
	if err != nil || !bytes.Equal(got, first.Private) {
		t.Fatalf("generation 1 key %v", err)
	}
	if got, err := OpenRecordingKey(viewer.Bytes(), "team_1", 3, copy3, nil, 3); err != nil || !bytes.Equal(got, third.Private) {
		t.Fatalf("current key %v", err)
	}
	if _, err := OpenRecordingKey(viewer.Bytes(), "team_2", 3, copy3, chain, 3); err == nil {
		t.Fatal("a copy opened for another team")
	}
	if _, err := OpenRecordingKey(viewer.Bytes(), "team_1", 3, copy3, chain[:1], 1); err == nil {
		t.Fatal("a missing chain link was skipped")
	}
}

// The reader opens segments only in the order the machine wrote them: a dropped, repeated or foreign
// segment, or anything after the final one, is refused.
func TestReaderFollowsTheSegmentChain(t *testing.T) {
	team := teamKey(t)
	dataKey, _ := NewDataKey()
	sealed, _ := SealDataKey(team.PublicKey().Bytes(), "rec_1", 2, dataKey)
	var files [][]byte
	previous := ""
	for i := range 3 {
		file, err := EncodeSegment(SegmentHeader{Recording: "rec_1", Index: uint32(i), KeyGeneration: 2, SealedKey: sealed, PreviousSHA256: previous, Final: i == 2}, dataKey, EventLine(time.Duration(i)*time.Second, CodeOutput, "x"))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(file)
		previous = hex.EncodeToString(digest[:])
		files = append(files, file)
	}
	reader := NewReader(team.Bytes(), "rec_1", 2)
	for i, file := range files {
		if _, err := reader.Open(file); err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
	}
	if !reader.Final() {
		t.Fatal("final segment not seen")
	}
	if _, err := reader.Open(files[2]); err == nil {
		t.Fatal("a segment after the final one opened")
	}
	for name, order := range map[string][]int{"dropped": {0, 2}, "repeated": {0, 0}, "reordered": {1}} {
		reader := NewReader(team.Bytes(), "rec_1", 2)
		var err error
		for _, i := range order {
			if _, err = reader.Open(files[i]); err != nil {
				break
			}
		}
		if err == nil {
			t.Fatalf("%s segments opened", name)
		}
	}
	if _, err := NewReader(team.Bytes(), "rec_2", 2).Open(files[0]); err == nil {
		t.Fatal("another recording's segment opened")
	}
}

// A member holding generation 1 replaces the key: the new generation verifies as a signed successor (what a machine
// checks), the previous bundle is sealed to it so older recordings stay readable through the chain, a copy sealed
// to a member opens the new generation, and a signature made by anyone else is refused.
func TestRotateMakesASignedSuccessorThatKeepsOldRecordingsReadable(t *testing.T) {
	member := teamKey(t)
	first, firstPublic := newBundle(t)
	firstSigningPublic := ed25519.PrivateKey(first.Signing).Public().(ed25519.PublicKey)
	gen1 := wire.Key{Generation: 1, PublicKey: firstPublic, SigningPublicKey: firstSigningPublic}
	rotation, err := Rotate("team_1", 1, first)
	if err != nil {
		t.Fatal(err)
	}
	gen2 := wire.Key{Generation: 2, PublicKey: rotation.PublicKey, SigningPublicKey: rotation.SigningPublicKey, RotationSignature: rotation.RotationSignature}
	if !wire.VerifyRotation("team_1", gen1, gen2) {
		t.Fatal("the new generation is not a signed successor of generation 1")
	}
	if wire.VerifyRotation("team_2", gen1, gen2) {
		t.Fatal("the rotation verified for another team")
	}
	forged := gen2
	forged.RotationSignature = ed25519.Sign(rotation.Bundle.Signing, wire.RotationMessage("team_1", 2, gen2.PublicKey, gen2.SigningPublicKey))
	if wire.VerifyRotation("team_1", gen1, forged) {
		t.Fatal("a signature by the new key itself verified: only generation 1's signing key may replace it")
	}
	copy2, err := SealBundle(member.PublicKey().Bytes(), "team_1", 2, rotation.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	chain := []ChainLink{{Generation: 2, SealedPrevious: rotation.SealedPrevious}}
	if got, err := OpenRecordingKey(member.Bytes(), "team_1", 2, copy2, chain, 1); err != nil || !bytes.Equal(got, first.Private) {
		t.Fatalf("a recording made under generation 1 must stay readable through the chain: %v", err)
	}
	if got, err := OpenRecordingKey(member.Bytes(), "team_1", 2, copy2, nil, 2); err != nil || !bytes.Equal(got, rotation.Bundle.Private) {
		t.Fatalf("the new generation opens with the member's copy: %v", err)
	}
	if _, err := Rotate("", 1, first); err == nil {
		t.Fatal("a rotation without a team was made")
	}
	if _, err := Rotate("team_1", 1, Bundle{Private: first.Private}); err == nil {
		t.Fatal("a rotation without the signing key was made")
	}
}

// The CLI's whole client side of replacing the key: the caller's own copy of the current generation signs the next
// one, every viewer with a vault gets a copy of it (one without a vault is skipped), and a caller without a copy of
// the current generation cannot.
func TestReplaceKeyForACallerWhoHoldsTheCurrentKey(t *testing.T) {
	caller, other := teamKey(t), teamKey(t)
	first, firstPublic := newBundle(t)
	gen1 := wire.Key{Generation: 1, PublicKey: firstPublic, SigningPublicKey: ed25519.PrivateKey(first.Signing).Public().(ed25519.PublicKey)}
	copy1, err := SealBundle(caller.PublicKey().Bytes(), "team_1", 1, first)
	if err != nil {
		t.Fatal(err)
	}
	state := wire.KeyState{
		Key:   &gen1,
		MyKey: &wire.SealedKey{Generation: 1, Sealed: copy1},
		Viewers: []wire.Viewer{
			{AccountID: "caller", SharingPublicKey: caller.PublicKey().Bytes(), HasGrant: true},
			{AccountID: "other", SharingPublicKey: other.PublicKey().Bytes()},
			{AccountID: "novault"},
		},
	}
	request, err := ReplaceKey(caller.Bytes(), "team_1", state)
	if err != nil {
		t.Fatal(err)
	}
	if request.Key.Generation != 2 || !wire.VerifyRotation("team_1", gen1, request.Key) {
		t.Fatal("the replacement is not a signed successor of the current key")
	}
	if len(request.Grants) != 2 || request.Grants[0].AccountID != "caller" || request.Grants[1].AccountID != "other" {
		t.Fatalf("grants %+v", request.Grants)
	}
	chain := []ChainLink{{Generation: 2, SealedPrevious: request.SealedPrevious}}
	if got, err := OpenRecordingKey(other.Bytes(), "team_1", 2, request.Grants[1].Sealed, chain, 1); err != nil || !bytes.Equal(got, first.Private) {
		t.Fatalf("another member opens an old recording through the chain: %v", err)
	}
	// No copy, a copy of an older generation, or a copy sealed to another vault: nobody can sign the next key.
	for name, bad := range map[string]wire.KeyState{
		"no copy":    {Key: &gen1, Viewers: state.Viewers},
		"older copy": {Key: &wire.Key{Generation: 2, PublicKey: firstPublic, SigningPublicKey: gen1.SigningPublicKey, RotationSignature: make([]byte, 64)}, MyKey: state.MyKey, Viewers: state.Viewers},
		"no viewers": {Key: &gen1, MyKey: state.MyKey},
	} {
		if _, err := ReplaceKey(caller.Bytes(), "team_1", bad); err == nil {
			t.Fatalf("%s: a key was made", name)
		}
	}
	if _, err := ReplaceKey(other.Bytes(), "team_1", state); err == nil {
		t.Fatal("a copy sealed to another vault was opened")
	}
}
