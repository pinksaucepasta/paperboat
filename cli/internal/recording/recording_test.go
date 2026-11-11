package recording

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
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
