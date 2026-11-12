package tunnel

import (
	"context"
	"io"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func recordingBytesMessage(t *testing.T, offset uint64, data string) scriptedHelperMessage {
	t.Helper()
	encoded, err := protocol.EncodeTerminalOutput(protocol.TerminalOutputFrame{Channel: protocol.TerminalStdout, Encoding: protocol.TerminalOutputRaw, StreamID: 1, StartSequence: offset, Data: []byte(data)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return scriptedHelperMessage{helperBinaryMessage, encoded}
}

// The host sends a heartbeat every 5 s, so a long replay on a slow path always meets one. It used to end the replay
// with "the recording stream ended early" (about one in five exports of a 6 MB recording over the internet).
func TestRecordingReplaySurvivesAHeartbeatMidStream(t *testing.T) {
	metadata := RecordingReplayMetadata{RecordingID: "rec_00000000-0000-4000-8000-000000000001", Segments: 1, Bytes: 8, StreamID: 1}
	messages := &scriptedHelperMessages{[]scriptedHelperMessage{
		recordingBytesMessage(t, 0, "abcd"), heartbeatMessage(t), recordingBytesMessage(t, 4, "efgh"), heartbeatMessage(t), recordingBytesMessage(t, 8, "ijkl"),
	}}
	reader, writer := io.Pipe()
	go relayRecording(context.Background(), messages, writer, metadata)
	all, err := io.ReadAll(reader)
	if err != nil || string(all[len(all)-12:]) != "abcdefghijkl" {
		t.Fatalf("replay with heartbeats = %q, %v", all, err)
	}
}
