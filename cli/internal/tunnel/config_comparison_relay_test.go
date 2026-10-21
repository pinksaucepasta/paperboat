package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

type scriptedHelperMessages struct {
	messages []scriptedHelperMessage
}
type scriptedHelperMessage struct {
	kind helperMessageType
	data []byte
}

func (s *scriptedHelperMessages) ReadMessage(context.Context) (helperMessageType, []byte, error) {
	if len(s.messages) == 0 {
		return 0, nil, io.EOF
	}
	next := s.messages[0]
	s.messages = s.messages[1:]
	return next.kind, next.data, nil
}
func (*scriptedHelperMessages) WriteMessage(context.Context, helperMessageType, []byte) error {
	return nil
}
func (*scriptedHelperMessages) Close() error { return nil }

func comparisonContentMessage(t *testing.T, channel byte, offset uint64, data string) scriptedHelperMessage {
	t.Helper()
	encoded, err := protocol.EncodeTerminalOutput(protocol.TerminalOutputFrame{Channel: channel, Encoding: protocol.TerminalOutputRaw, StreamID: 1, StartSequence: offset, Data: []byte(data)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return scriptedHelperMessage{helperBinaryMessage, encoded}
}

func comparisonEndMessage(t *testing.T) scriptedHelperMessage {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"type": "comparison_complete", "stream_id": 1})
	data, err := json.Marshal(helperFrame{Type: "event", RequestID: "stream", Version: helperProtocolVersion, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return scriptedHelperMessage{helperStructuredMessage, data}
}

func relayedComparison(t *testing.T, messages ...scriptedHelperMessage) (string, error) {
	t.Helper()
	reader, writer := io.Pipe()
	result := ComparisonMetadata{StreamID: 1, ConflictComparisonRequest: configsync.ConflictComparisonRequest{Path: "ecfg"}, Local: ComparisonSideMetadata{Present: true, Kind: "file", Bytes: 12}, Managed: ComparisonSideMetadata{Present: true, Kind: "file", Bytes: 9}}
	go relayComparisonContent(context.Background(), &scriptedHelperMessages{messages}, writer, result)
	all, err := io.ReadAll(reader)
	return string(all), err
}

// The end event and the content frames use different channels of the peer connection, so the end
// event may arrive first. That used to fail the whole comparison on most attempts.
func TestComparisonSurvivesTheEndEventOvertakingItsContent(t *testing.T) {
	ordered, err := relayedComparison(t, comparisonContentMessage(t, protocol.TerminalStdout, 0, "machine side"), comparisonContentMessage(t, protocol.TerminalStderr, 0, "repo side"), comparisonEndMessage(t))
	if err != nil || !bytes.Contains([]byte(ordered), []byte("machine side")) {
		t.Fatalf("in-order stream = %q, %v", ordered, err)
	}
	overtaken, err := relayedComparison(t, comparisonEndMessage(t), comparisonContentMessage(t, protocol.TerminalStdout, 0, "machine side"), comparisonContentMessage(t, protocol.TerminalStderr, 0, "repo side"))
	if err != nil || !bytes.Contains([]byte(overtaken), []byte("machine side")) || !bytes.Contains([]byte(overtaken), []byte("repo side")) {
		t.Fatalf("overtaken stream = %q, %v; want both sides delivered", overtaken, err)
	}
	if _, err := relayedComparison(t, comparisonEndMessage(t), comparisonContentMessage(t, protocol.TerminalStdout, 0, "machine side")); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("a stream that ends short must fail, got %v", err)
	}
}
