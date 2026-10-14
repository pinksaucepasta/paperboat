package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func TestConfigComparisonStreamsCompleteLargeSidesAndClears(t *testing.T) {
	request := configsync.ConflictComparisonRequest{AssignmentID: "assignment_1", AssignmentVersion: 2, Path: "settings.txt", ConflictRevision: "conflict_1", ExpectedRemoteRevision: "remote_1"}
	claims := auth.Claims{AssignmentID: request.AssignmentID, AssignmentVersion: request.AssignmentVersion, ConfigPath: request.Path, ConflictRevision: request.ConflictRevision, ExpectedRemoteRevision: request.ExpectedRemoteRevision}
	authorization := Authorization{ResourceID: request.AssignmentID, Value: claims, ExpiresAt: time.Now().Add(time.Minute)}
	content := []byte(nil)
	d := &Dispatcher{compareGate: make(chan struct{}, 1), config: DispatcherConfig{ConfigCompare: func(context.Context, configsync.ConflictComparisonRequest) (configsync.ConflictComparison, error) {
		content = bytes.Repeat([]byte("private content\n"), 20000)
		digest := sha256.Sum256(content)
		return configsync.ConflictComparison{ConflictComparisonRequest: request, Local: configsync.ConflictComparisonSide{Present: true, Kind: "regular", SHA256: hex.EncodeToString(digest[:]), Content: content}, Managed: configsync.ConflictComparisonSide{Kind: "deleted"}}, nil
	}}}
	payload, _ := json.Marshal(struct {
		Action string `json:"action"`
		configsync.ConflictComparisonRequest
	}{"compare", request})
	outcome := d.configCompare(context.Background(), authorization, payload)
	if outcome.ErrorCode != "" || bytes.Contains(outcome.Result, []byte("content")) || len(outcome.Result) > protocol.MaxStructuredFrame {
		t.Fatalf("invalid metadata %s", outcome.Result)
	}
	if !bytes.Equal(content, make([]byte, len(content))) {
		t.Fatal("metadata retained private content")
	}
	stream, ok, err := d.openConfigComparison(context.Background(), authorization, payload, outcome)
	if err != nil || !ok {
		t.Fatal(err)
	}
	var all []byte
	for {
		frame, err := stream.Next(context.Background())
		var end *StreamEnd
		if errors.As(err, &end) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Channel != protocol.Stdout || frame.StartSequence != uint64(len(all)) || len(frame.Data) > 64<<10 {
			t.Fatal("invalid bounded byte framing")
		}
		var wire bytes.Buffer
		if err := protocol.WriteBinaryFrame(&wire, frame); err != nil {
			t.Fatal(err)
		}
		decoded, err := protocol.ReadBinaryFrame(&wire)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, decoded.Data...)
	}
	if !bytes.Equal(all, content) || len(all) <= protocol.MaxStructuredFrame {
		t.Fatal("content truncated")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if len(d.compareGate) != 0 || !bytes.Equal(content, make([]byte, len(content))) {
		t.Fatal("stream did not release content/gate")
	}
	changed := request
	changed.Path = "other"
	payload, _ = json.Marshal(struct {
		Action string `json:"action"`
		configsync.ConflictComparisonRequest
	}{"compare", changed})
	if got := d.configCompare(context.Background(), authorization, payload); got.ErrorCode != "not_found_or_forbidden" {
		t.Fatal("foreign path accessed")
	}
}
func TestConfigComparisonStopsOnRevocationAndExpiry(t *testing.T) {
	var revoked atomic.Bool
	revoked.Store(true)
	for _, authorization := range []Authorization{{Revoked: &revoked}, {ExpiresAt: time.Now().Add(-time.Second)}} {
		stream := &configComparisonStream{authorization: authorization}
		if _, err := stream.Next(context.Background()); err == nil {
			t.Fatal("stale grant read content")
		}
	}
}

func TestConfigComparisonDoesNotJournalReadResults(t *testing.T) {
	calls := 0
	runtime := testServer(t, authorizerFunc(func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{JournalBinding: "comparison-binding"}, nil
	}), handlerFunc(func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
		calls++
		return result(map[string]int{"read": calls})
	}), 2)
	runtime.config.Negotiator.Available["config.compare.v1"] = true
	client, peer := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(peer) }()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	payload := json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1","config.compare.v1"]}`)
	if err := protocol.WriteFrame(client, protocol.Frame{Type: "hello", RequestID: "req_hello", Version: "1.0", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if frame, err := protocol.ReadFrame(client); err != nil || frame.Type != "welcome" {
		t.Fatalf("welcome %v %v", frame, err)
	}
	for want := 1; want <= 2; want++ {
		request := protocol.Frame{Type: "request", RequestID: fmt.Sprintf("req_compare_%d", want), OperationID: "op_compare_1", Version: "1.0", Capability: "config.compare.v1", DeadlineMS: 1000, Payload: json.RawMessage(`{"action":"compare"}`)}
		if err := protocol.WriteFrame(client, request); err != nil {
			t.Fatal(err)
		}
		frame, err := protocol.ReadFrame(client)
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Result struct {
				Read int `json:"read"`
			} `json:"result"`
			Replay bool `json:"replay"`
		}
		if json.Unmarshal(frame.Payload, &response) != nil || frame.Type != "response" || response.Result.Read != want || response.Replay {
			t.Fatalf("read replayed/journaled: %s", frame.Payload)
		}
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("comparison connection leaked")
	}
}
