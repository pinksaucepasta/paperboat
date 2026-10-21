package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

const MaxComparisonSideBytes = 100 << 20

type ComparisonSideMetadata struct {
	Present bool   `json:"present"`
	Kind    string `json:"kind"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
}
type ComparisonMetadata struct {
	configsync.ConflictComparisonRequest
	StreamID uint32                 `json:"stream_id"`
	Local    ComparisonSideMetadata `json:"local"`
	Managed  ComparisonSideMetadata `json:"managed"`
}

func (t *PeerTerminalTunnel) DialConfigComparison(ctx context.Context, info resolver.ConnectInfo, operationID string, request configsync.ConflictComparisonRequest) (Conn, error) {
	return t.dial(ctx, info, "config_compare", peerApplication{operationID: operationID, helper: func(ctx context.Context, message helperMessageConnection, target *resolver.TerminalTarget) (Conn, error) {
		payload, err := json.Marshal(struct {
			Action string `json:"action"`
			configsync.ConflictComparisonRequest
		}{"compare", request})
		if err != nil {
			return nil, err
		}
		frame, err := helperRequestSyncOperation(ctx, message, "config.compare.v1", operationID, payload, nil)
		if err != nil {
			return nil, err
		}
		var response struct {
			Result ComparisonMetadata `json:"result"`
		}
		if json.Unmarshal(frame.Payload, &response) != nil || !validComparisonMetadata(response.Result, request) {
			return nil, errors.New("invalid conflict comparison response")
		}
		reader, writer := io.Pipe()
		connection := &comparisonPipe{reader: reader, message: message}
		go relayComparisonContent(ctx, message, writer, response.Result)
		return connection, nil
	}})
}

// relayComparisonContent forwards the comparison bytes to the reader. The end event and the content
// frames travel on different channels of the peer connection and the end event can overtake the last
// content, so a complete end event received before every declared byte is not an error: the relay
// keeps reading until the declared bytes have arrived, and only a closed connection first fails it.
func relayComparisonContent(ctx context.Context, message helperMessageConnection, writer *io.PipeWriter, result ComparisonMetadata) {
	defer writer.Close()
	defer message.Close()
	raw, _ := json.Marshal(result)
	if _, err := writer.Write(append(raw, '\n')); err != nil {
		return
	}
	var offsets [2]int64
	complete := func() bool { return offsets[0] == result.Local.Bytes && offsets[1] == result.Managed.Bytes }
	ended := false
	for !ended || !complete() {
		kind, data, err := message.ReadMessage(ctx)
		if err != nil {
			if ended {
				err = errors.New("comparison did not complete")
			}
			writer.CloseWithError(err)
			return
		}
		if kind == helperBinaryMessage {
			output, err := protocol.DecodeTerminalOutput(data)
			index := int(output.Channel) - 1
			if err != nil || output.StreamID != result.StreamID || index < 0 || index > 1 || output.StartSequence != uint64(offsets[index]) {
				writer.CloseWithError(errors.New("invalid comparison content stream"))
				return
			}
			expected := result.Local.Bytes
			if index == 1 {
				expected = result.Managed.Bytes
			}
			offsets[index] += int64(len(output.Data))
			if offsets[index] > expected {
				writer.CloseWithError(errors.New("comparison exceeds declared bytes"))
				return
			}
			if err := protocol.WriteBinaryFrame(writer, protocol.BinaryFrame{Channel: output.Channel, StartSequence: output.StartSequence, Data: output.Data}); err != nil {
				return
			}
			continue
		}
		frame, err := decodeHelperFrame(data)
		var end struct {
			Type     string `json:"type"`
			StreamID uint32 `json:"stream_id"`
		}
		if err != nil || frame.Type != "event" || json.Unmarshal(frame.Payload, &end) != nil || end.Type != "comparison_complete" || end.StreamID != result.StreamID {
			writer.CloseWithError(errors.New("comparison did not complete"))
			return
		}
		ended = true
	}
}

func validComparisonMetadata(m ComparisonMetadata, request configsync.ConflictComparisonRequest) bool {
	return m.ConflictComparisonRequest == request && m.StreamID != 0 && m.Local.Bytes >= 0 && m.Managed.Bytes >= 0 && m.Local.Bytes <= MaxComparisonSideBytes && m.Managed.Bytes <= MaxComparisonSideBytes
}

type comparisonPipe struct {
	reader  *io.PipeReader
	message helperMessageConnection
}

func (c *comparisonPipe) Read(b []byte) (int, error) { return c.reader.Read(b) }
func (*comparisonPipe) Write([]byte) (int, error)    { return 0, errors.New("comparison is read-only") }
func (*comparisonPipe) Resize(uint16, uint16) error  { return errors.New("comparison has no terminal") }
func (*comparisonPipe) Wait() (int, error)           { return 0, nil }
func (c *comparisonPipe) Close() error               { return errors.Join(c.reader.Close(), c.message.Close()) }
func VerifyComparisonSide(side ComparisonSideMetadata, content []byte) bool {
	if int64(len(content)) != side.Bytes {
		return false
	}
	if !side.Present {
		return len(content) == 0 && side.Kind == "deleted" && side.SHA256 == ""
	}
	if side.Kind != "file" && side.Kind != "symlink" {
		return false
	}
	if side.Kind == "symlink" {
		hasher := sha256.New()
		hasher.Write([]byte("symlink:"))
		hasher.Write(content)
		return hex.EncodeToString(hasher.Sum(nil)) == side.SHA256
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]) == side.SHA256
}
