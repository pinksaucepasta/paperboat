package recording

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
