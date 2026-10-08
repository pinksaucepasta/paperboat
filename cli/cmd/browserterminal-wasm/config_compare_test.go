package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
)

// The actual host negotiator, request validator, response envelope, stream IDs
// and completion events own the producer side of this browser-reader regression.
type comparisonPeer struct{ net.Conn }

func (p comparisonPeer) ReadApplication() (protocol.Frame, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(p, header[:]); err != nil {
		return protocol.Frame{}, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if header[0] != 1 || length > protocol.MaxStructuredFrame {
		return protocol.Frame{}, nil, io.ErrUnexpectedEOF
	}
	wire := make([]byte, 4+length)
	binary.BigEndian.PutUint32(wire, length)
	if _, err := io.ReadFull(p, wire[4:]); err != nil {
		return protocol.Frame{}, nil, err
	}
	frame, err := protocol.ReadFrame(bytes.NewReader(wire))
	return frame, nil, err
}
func (p comparisonPeer) WriteStructured(frame protocol.Frame) error {
	return writeStructuredFrame(p, frame)
}
func (p comparisonPeer) WriteBinary(frame protocol.BinaryFrame) error {
	return p.WriteTerminalOutput(1, frame)
}
func (p comparisonPeer) WriteTerminalOutput(id uint32, frame protocol.BinaryFrame) error {
	payload, err := protocol.EncodeTerminalOutputAdaptive(protocol.TerminalOutputFrame{Channel: frame.Channel, StreamID: id, StartSequence: frame.StartSequence, Data: frame.Data}, nil)
	if err != nil {
		return err
	}
	return writeApplicationFrame(p, appKindBinary, payload)
}

type comparisonFixture struct {
	request compareRequest
	content []byte
	corrupt string
	offset  int
}

func (f *comparisonFixture) Authorize(_ context.Context, frame protocol.Frame) (server.Authorization, error) {
	if frame.Type == "request" && (frame.OperationID != "operation_bound" || frame.DeadlineMS != 120000) {
		return server.Authorization{}, server.ErrInvalidConfiguration
	}
	return server.Authorization{JournalBinding: "exact-comparison", ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (f *comparisonFixture) Handle(_ context.Context, _ server.Authorization, _ string, payload json.RawMessage) operation.Outcome {
	var request compareRequest
	if json.Unmarshal(payload, &request) != nil || request != f.request {
		return operation.Outcome{ErrorCode: "not_found_or_forbidden"}
	}
	digest := sha256.Sum256(f.content)
	hash := hex.EncodeToString(digest[:])
	if f.corrupt == "digest" {
		hash = strings.Repeat("0", 64)
	}
	if f.corrupt == "binding" {
		request.Path = "other.txt"
	}
	metadata := compareMetadata{compareRequest: request, Local: compareSide{Present: true, Kind: "regular", SHA256: hash, Bytes: int64(len(f.content))}, Managed: compareSide{Kind: "deleted"}}
	raw, _ := json.Marshal(metadata)
	return operation.Outcome{Result: raw}
}
func (f *comparisonFixture) OpenStream(context.Context, server.Authorization, string, json.RawMessage, operation.Outcome, bool) (server.OutputStream, bool, error) {
	return f, true, nil
}
func (f *comparisonFixture) Next(context.Context) (protocol.BinaryFrame, error) {
	if f.offset == len(f.content) {
		return protocol.BinaryFrame{}, &server.StreamEnd{Payload: json.RawMessage(`{"type":"comparison_complete"}`)}
	}
	start := f.offset
	end := min(start+64*1024, len(f.content))
	f.offset = end
	seq := uint64(start)
	if f.corrupt == "sequence" {
		seq++
	}
	return protocol.BinaryFrame{Channel: protocol.Stdout, StartSequence: seq, Data: f.content[start:end]}, nil
}
func (f *comparisonFixture) Close() error { return nil }
func TestBrowserComparisonReadsActualHostProtocolAndRejectsCorruption(t *testing.T) {
	for _, corrupt := range []string{"", "digest", "sequence", "binding"} {
		t.Run(corrupt, func(t *testing.T) {
			request := compareRequest{Action: "compare", AssignmentID: "assignment_exact", AssignmentVersion: 3, Path: "settings.txt", ConflictRevision: strings.Repeat("a", 64), ExpectedRemoteRevision: strings.Repeat("b", 40)}
			fixture := &comparisonFixture{request: request, content: bytes.Repeat([]byte{0, 1, 2, 255}, 40000), corrupt: corrupt}
			journal, err := operation.NewJournal(4)
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := server.New(server.Config{Negotiator: protocol.Negotiator{Available: map[string]bool{"terminal.v1": true, "health.v1": true, "config.compare.v1": true}}, Journal: journal, Authorizer: fixture, Handler: fixture, MaxConcurrent: 1, HeartbeatInterval: time.Hour, MutationDeadline: 2 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			browser, host := net.Pipe()
			browser.SetDeadline(time.Now().Add(time.Second))
			done := make(chan error, 1)
			go func() { done <- runtime.Serve(comparisonPeer{host}) }()
			metadata, local, managed, err := readConfigComparisonContent(browser, request, "operation_bound")
			browser.Close()
			host.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			runtime.Shutdown(ctx)
			<-done
			defer clear(local)
			defer clear(managed)
			if corrupt != "" {
				if err == nil || local != nil || managed != nil {
					t.Fatal("corrupt comparison released content")
				}
				return
			}
			if err != nil {
				t.Fatalf("actual host comparison failed: %v", err)
			}
			if metadata.StreamID == 0 || metadata.Local.Kind != "regular" || !bytes.Equal(local, fixture.content) || len(managed) != 0 {
				t.Fatal("verified comparison was incomplete")
			}
		})
	}
}
