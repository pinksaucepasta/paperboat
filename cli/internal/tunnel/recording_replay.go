package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/recording"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

// MaxRecordingReplayBytes bounds what one replay downloads from a machine.
const MaxRecordingReplayBytes = 4 << 30

// RecordingReplayMetadata is the first line of a replay stream; after it come Segments segment files
// (Bytes in all), each preceded by its length as 4 bytes big endian.
type RecordingReplayMetadata struct {
	RecordingID string `json:"recording_id"`
	Segments    uint32 `json:"segments"`
	Bytes       int64  `json:"bytes"`
	Final       bool   `json:"final"`
	StreamID    uint32 `json:"stream_id"`
}

// StreamBytes is how many bytes follow the metadata line.
func (m RecordingReplayMetadata) StreamBytes() int64 { return m.Bytes + 4*int64(m.Segments) }

func (m RecordingReplayMetadata) valid(recordingID string) bool {
	return m.RecordingID == recordingID && m.StreamID != 0 && m.Bytes >= 0 && m.StreamBytes() <= MaxRecordingReplayBytes &&
		int64(m.Segments) <= m.Bytes/int64(len(recording.Magic)+4)+1
}

// DialRecording reads one recording's encrypted segments from the machine with a replay ticket. The
// bytes stay ciphertext here; the CLI decrypts them.
func (t *PeerTerminalTunnel) DialRecording(ctx context.Context, info resolver.ConnectInfo, operationID, recordingID string) (Conn, error) {
	return t.dial(ctx, info, "recording", peerApplication{operationID: operationID, helper: func(ctx context.Context, message helperMessageConnection, target *resolver.TerminalTarget) (Conn, error) {
		payload, err := json.Marshal(struct {
			Action      string `json:"action"`
			RecordingID string `json:"recording_id"`
		}{wire.ReadAction, recordingID})
		if err != nil {
			return nil, err
		}
		frame, err := helperRequestSyncOperation(ctx, message, wire.Capability, operationID, payload, nil)
		if err != nil {
			return nil, err
		}
		var response struct {
			Result RecordingReplayMetadata `json:"result"`
		}
		if json.Unmarshal(frame.Payload, &response) != nil || !response.Result.valid(recordingID) {
			return nil, errors.New("invalid recording replay response")
		}
		reader, writer := io.Pipe()
		go relayRecording(ctx, message, writer, response.Result)
		return &comparisonPipe{reader: reader, message: message}, nil
	}})
}

// relayRecording writes the metadata line and then the declared stream bytes in order. Like a
// comparison, the end event may overtake the last bytes, so only the declared length ends it.
func relayRecording(ctx context.Context, message helperMessageConnection, writer *io.PipeWriter, metadata RecordingReplayMetadata) {
	defer writer.Close()
	defer message.Close()
	raw, _ := json.Marshal(metadata)
	if _, err := writer.Write(append(raw, '\n')); err != nil {
		return
	}
	var offset int64
	for offset < metadata.StreamBytes() {
		kind, data, err := message.ReadMessage(ctx)
		if err != nil {
			writer.CloseWithError(err)
			return
		}
		if kind != helperBinaryMessage {
			frame, err := decodeHelperFrame(data)
			if err == nil && (frame.Type == "event" || frame.Type == "heartbeat") {
				// The end event (the bytes it follows may still be on their way) or the host's 5 s heartbeat, which a replay
				// on a slow path always outlives: neither ends the stream.
				continue
			}
			writer.CloseWithError(errors.New("the recording stream ended early"))
			return
		}
		output, err := protocol.DecodeTerminalOutput(data)
		if err != nil || output.StreamID != metadata.StreamID || output.StartSequence != uint64(offset) || offset+int64(len(output.Data)) > metadata.StreamBytes() {
			writer.CloseWithError(errors.New("invalid recording stream"))
			return
		}
		if _, err := writer.Write(output.Data); err != nil {
			return
		}
		offset += int64(len(output.Data))
	}
}
