package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/recording"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

const testRecordingID = "rec_00000000-0000-4000-8000-000000000001"

// recordingStream builds what the daemon hands the CLI: the metadata line, then each segment file
// behind its 4-byte length.
func recordingStream(t *testing.T, team *ecdh.PrivateKey, final bool, texts ...[]byte) []byte {
	t.Helper()
	dataKey, _ := recording.NewDataKey()
	sealed, err := recording.SealDataKey(team.PublicKey().Bytes(), testRecordingID, 1, dataKey)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	var total int64
	previous := ""
	for i, text := range texts {
		file, err := recording.EncodeSegment(recording.SegmentHeader{Recording: testRecordingID, Index: uint32(i), KeyGeneration: 1, SealedKey: sealed, PreviousSHA256: previous, Final: final && i == len(texts)-1}, dataKey, text)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(file)
		previous = hex.EncodeToString(digest[:])
		body.Write(binary.BigEndian.AppendUint32(nil, uint32(len(file))))
		body.Write(file)
		total += int64(len(file))
	}
	line, _ := json.Marshal(tunnel.RecordingReplayMetadata{RecordingID: testRecordingID, Segments: uint32(len(texts)), Bytes: total, Final: final, StreamID: 1})
	return append(append(line, '\n'), body.Bytes()...)
}

func TestRecordingStreamDecodesAndPlaysInOrder(t *testing.T) {
	team, _ := ecdh.X25519().GenerateKey(rand.Reader)
	header, _ := recording.HeaderLine(recording.Header{Width: 80, Height: 24, Timestamp: 1})
	first := append(header, recording.EventLine(time.Second, recording.CodeOutput, "hello ")...)
	second := append(recording.EventLine(30*time.Second, recording.CodeResize, recording.ResizeData(100, 30)), recording.EventLine(31*time.Second, recording.CodeOutput, "world")...)
	stream := recordingStream(t, team, true, first, second)

	var cast bytes.Buffer
	if err := decodeRecordingStream(bufio.NewReader(bytes.NewReader(stream)), recording.NewReader(team.Bytes(), testRecordingID, 1), testRecordingID, func(b []byte) error { cast.Write(b); return nil }); err != nil {
		t.Fatal(err)
	}
	if want := string(first) + string(second); cast.String() != want {
		t.Fatalf("export %q", cast.String())
	}
	var screen bytes.Buffer
	var waits []time.Duration
	player := castPlayer{out: &screen, speed: 2, idle: 2, sleep: func(d time.Duration) { waits = append(waits, d) }}
	if err := player.write(cast.Bytes()); err != nil {
		t.Fatal(err)
	}
	// 1 s at 2x, then the 30 s pause capped at the 2 s idle limit, at 2x.
	if screen.String() != "hello world" || len(waits) != 2 || waits[0] != 500*time.Millisecond || waits[1] != time.Second {
		t.Fatalf("screen %q waits %v", screen.String(), waits)
	}

	// A finished recording whose final segment is missing, or a segment opened with another team's key, fails.
	truncated := recordingStream(t, team, true, first, second)
	var metadata tunnel.RecordingReplayMetadata
	end := bytes.IndexByte(truncated, '\n')
	_ = json.Unmarshal(truncated[:end], &metadata)
	firstSize := int64(binary.BigEndian.Uint32(truncated[end+1:]))
	metadata.Segments, metadata.Bytes = 1, firstSize
	line, _ := json.Marshal(metadata)
	cut := append(append(line, '\n'), truncated[end+1:end+1+4+int(firstSize)]...)
	err := decodeRecordingStream(bufio.NewReader(bytes.NewReader(cut)), recording.NewReader(team.Bytes(), testRecordingID, 1), testRecordingID, func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "final segment") {
		t.Fatalf("truncated recording: %v", err)
	}
	other, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if err := decodeRecordingStream(bufio.NewReader(bytes.NewReader(stream)), recording.NewReader(other.Bytes(), testRecordingID, 1), testRecordingID, func([]byte) error { return nil }); err == nil {
		t.Fatal("a recording opened with another team key")
	}
}

// An export is owner-only and never replaces an existing file.
func TestRecordingExportFileIsNewAndOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.cast")
	file, err := createExportFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("export file mode %v %v", info.Mode(), err)
	}
	if _, err := createExportFile(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("an export replaced an existing file: %v", err)
	}
}

// A replay that fails says why, instead of the generic "could not finish the command".
func TestReplayFailureIsNamedAndTypedAnswersKeepTheirWords(t *testing.T) {
	named := namedReplayFailure(errors.New("the recording's final segment is missing"))
	var failure *recordingReplayFailure
	if !errors.As(named, &failure) || !strings.Contains(commandFailureMessage(named, classifyCommandFailure(named)), "final segment is missing") {
		t.Fatalf("not named: %v / %q", named, commandFailureMessage(named, classifyCommandFailure(named)))
	}
	usage := localArgumentError("not a recording id")
	if namedReplayFailure(usage) != usage {
		t.Fatal("a usage error was rewrapped")
	}
	if namedReplayFailure(nil) != nil || namedReplayFailure(context.Canceled) != context.Canceled {
		t.Fatal("nil or cancellation was rewrapped")
	}
}
