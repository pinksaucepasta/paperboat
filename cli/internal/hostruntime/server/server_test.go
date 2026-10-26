package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	hosttelemetry "github.com/pinksaucepasta/paperboat/internal/hostruntime/telemetry"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	hostauth "github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
)

type authorizerFunc func(context.Context, protocol.Frame) (Authorization, error)

func (f authorizerFunc) Authorize(ctx context.Context, frame protocol.Frame) (Authorization, error) {
	return f(ctx, frame)
}

func TestExecControlUsesDistinctInternalJournalOperation(t *testing.T) {
	start := protocol.Frame{RequestID: "req_start", OperationID: "exec_operation_1", Capability: "exec.v1", Payload: json.RawMessage(`{"action":"start"}`)}
	attach := protocol.Frame{RequestID: "req_attach", OperationID: start.OperationID, Capability: "exec.v1", Payload: json.RawMessage(`{"action":"attach","operation_id":"exec_operation_1","from_sequence":2}`)}
	cancel := protocol.Frame{RequestID: "req_cancel", OperationID: start.OperationID, Capability: "exec.v1", Payload: json.RawMessage(`{"action":"cancel","operation_id":"exec_operation_1"}`)}
	if got := requestJournalOperationID(start); got != start.OperationID {
		t.Fatalf("start journal operation = %q", got)
	}
	if got := requestJournalOperationID(attach); got == "" || got == start.OperationID {
		t.Fatalf("attach journal operation = %q", got)
	}
	first := requestJournalOperationID(cancel)
	if first == "" || first == start.OperationID {
		t.Fatalf("control journal operation = %q", first)
	}
	if again := requestJournalOperationID(cancel); again != first {
		t.Fatalf("control journal operation changed: %q != %q", again, first)
	}
	cancel.RequestID = "req_cancel_retry"
	if retry := requestJournalOperationID(cancel); retry == first {
		t.Fatalf("distinct control request reused journal operation %q", retry)
	}
}

func TestTerminalBindingSurvivesCredentialExpiry(t *testing.T) {
	attach := protocol.Frame{Type: "request", RequestID: "req_attach", Version: "1.0", OperationID: "op_attach_0001", Capability: "terminal.v1", DeadlineMS: 1000, Payload: json.RawMessage(`{"action":"attach","session_id":"ses_1"}`)}
	outcome := operation.Outcome{Result: json.RawMessage(`{"attachment_id":"att_1","session":{"snapshot":{"generation":1}}}`)}

	state := newTerminalConnectionState()
	if _, err := state.bind(Authorization{ClientID: "cli_1", ExpiresAt: time.Now().Add(20 * time.Millisecond)}, attach, outcome); err != nil {
		t.Fatal(err)
	}
	select {
	case <-state.expired:
		t.Fatal("credential expiry closed an established terminal binding")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestSharedAndBrowserTerminalOutliveCredentialAndRevokeImmediately(t *testing.T) {
	attach := protocol.Frame{Type: "request", RequestID: "req_attach", Version: "1.0", OperationID: "op_attach_0001", Capability: "terminal.v1", DeadlineMS: 1000, Payload: json.RawMessage(`{"action":"attach","session_id":"ses_1"}`)}
	outcome := operation.Outcome{Result: json.RawMessage(`{"attachment_id":"att_1","session":{"snapshot":{"generation":1}}}`)}
	for _, role := range []TerminalRole{TerminalRoleOwner, TerminalRoleViewer, TerminalRoleInteractive} {
		for _, browser := range []bool{false, true} {
			state := newTerminalConnectionState()
			done := make(chan struct{})
			state.done = done
			revoked := make(chan struct{})
			if _, err := state.bind(Authorization{ClientID: "cli_1", TerminalRole: role, BrowserTerminal: browser, ExpiresAt: time.Now().Add(20 * time.Millisecond), RevokedSignal: revoked}, attach, outcome); err != nil {
				t.Fatal(err)
			}
			select {
			case <-state.expired:
				t.Fatalf("%s browser=%t expired admitted terminal", role, browser)
			case <-time.After(60 * time.Millisecond):
			}
			close(revoked)
			select {
			case <-state.expired:
			case <-time.After(time.Second):
				t.Fatalf("%s browser=%t ignored revocation", role, browser)
			}
			close(done)
		}
	}
}

func TestTerminalBindingRevocationClosesConnectionImmediately(t *testing.T) {
	attach := protocol.Frame{Type: "request", RequestID: "req_attach", Version: "1.0", OperationID: "op_attach_0001", Capability: "terminal.v1", DeadlineMS: 1000, Payload: json.RawMessage(`{"action":"attach","session_id":"ses_1"}`)}
	outcome := operation.Outcome{Result: json.RawMessage(`{"attachment_id":"att_1","session":{"snapshot":{"generation":1}}}`)}
	revoked := make(chan struct{})
	state := newTerminalConnectionState()
	state.done = make(chan struct{})
	if _, err := state.bind(Authorization{ClientID: "cli_1", RevokedSignal: revoked}, attach, outcome); err != nil {
		t.Fatal(err)
	}
	close(revoked)
	select {
	case <-state.expired:
	case <-time.After(time.Second):
		t.Fatal("revoked terminal binding did not close the connection")
	}
}

type handlerFunc func(context.Context, Authorization, string, json.RawMessage) operation.Outcome

func (f handlerFunc) Handle(ctx context.Context, authorization Authorization, capability string, payload json.RawMessage) operation.Outcome {
	return f(ctx, authorization, capability, payload)
}

func testServer(t *testing.T, authorize authorizerFunc, handle handlerFunc, maxConcurrent int) *Server {
	t.Helper()
	journal, err := operation.NewJournal(32)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Negotiator: protocol.Negotiator{Available: map[string]bool{"terminal.v1": true, "health.v1": true}},
		Journal:    journal, Authorizer: authorize, Handler: handle, MaxConcurrent: maxConcurrent,
		HeartbeatInterval: time.Hour, MutationDeadline: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return server
}

func hello(t *testing.T, conn net.Conn) protocol.Frame {
	t.Helper()
	payload := json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1"]}`)
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: "hello", RequestID: "req_hello", Version: "1.0", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != "welcome" {
		t.Fatalf("welcome=%#v", frame)
	}
	return frame
}

func TestServeDoesNotApplyApplicationIdleTimeout(t *testing.T) {
	journal, err := operation.NewJournal(8)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Negotiator: protocol.Negotiator{Available: map[string]bool{"terminal.v1": true, "health.v1": true}},
		Journal:    journal,
		Authorizer: authorizerFunc(func(context.Context, protocol.Frame) (Authorization, error) {
			return Authorization{JournalBinding: "idle-test"}, nil
		}),
		Handler: handlerFunc(func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
			return operation.Outcome{}
		}),
		MaxConcurrent:     1,
		HeartbeatInterval: time.Hour,
		MutationDeadline:  time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, peer := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.Serve(peer) }()
	hello(t, client)
	select {
	case err := <-done:
		t.Fatalf("idle peer was closed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	_ = client.Close()
	<-done
}

func request(id, operationID string, payload json.RawMessage) protocol.Frame {
	return protocol.Frame{Type: "request", RequestID: id, Version: "1.0", OperationID: operationID, Capability: "terminal.v1", DeadlineMS: 30_000, Payload: payload}
}

func TestServeRequiresHelloBeforeMutation(t *testing.T) {
	var authorized, handled atomic.Int32
	server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
		authorized.Add(1)
		return Authorization{JournalBinding: "user:1"}, nil
	}, func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
		handled.Add(1)
		return operation.Outcome{}
	}, 1)
	client, peer := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.Serve(peer) }()
	if err := protocol.WriteFrame(client, request("req_1", "op_00000001", json.RawMessage(`{"action":"list"}`))); err != nil {
		t.Fatal(err)
	}
	response, err := protocol.ReadFrame(client)
	if err != nil || response.Type != "error" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	_ = client.Close()
	if err := <-done; !errors.Is(err, ErrHandshakeRequired) {
		t.Fatalf("serve err=%v", err)
	}
	if authorized.Load() != 0 || handled.Load() != 0 {
		t.Fatalf("authorized=%d handled=%d", authorized.Load(), handled.Load())
	}
}

func TestServeReportsUnexpectedAuthorizationFailureWithoutLeakingCause(t *testing.T) {
	const privateText = "private credential database detail"
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()
	var handled atomic.Int32
	server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{}, &hostauth.Error{Code: hostauth.KeyUnknown, Cause: errors.New(privateText)}
	}, func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
		handled.Add(1)
		return operation.Outcome{}
	}, 1)
	client, peer := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.Serve(peer) }()
	hello(t, client)
	if err := protocol.WriteFrame(client, request("req_auth_failure", "op_auth_failure", json.RawMessage(`{}`))); err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.ReadFrame(client)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != "error" || !strings.Contains(string(frame.Payload), "unavailable") || strings.Contains(string(frame.Payload), privateText) {
		t.Fatalf("authorization error response = %#v", frame)
	}
	_ = client.Close()
	<-done
	if handled.Load() != 0 || len(faults) != 1 || faults[0].Stage != "peer_authority" || faults[0].Code != "peer_authority_failed" || strings.Contains(strings.Join(faults[0].ErrorChain, ","), privateText) {
		t.Fatalf("authorization failure capture = handled:%d faults:%#v", handled.Load(), faults)
	}
}

func TestErrorEnvelopeUsesContractFieldNames(t *testing.T) {
	frame := errorFrame("req_1", "invalid_request", "invalid request", false)
	var payload map[string]any
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"code", "message", "requestId", "retryable"} {
		if _, ok := payload[field]; !ok {
			t.Fatalf("missing %s in %s", field, frame.Payload)
		}
	}
	if _, leaked := payload["RequestID"]; leaked {
		t.Fatalf("Go field name leaked in %s", frame.Payload)
	}
}

func TestReplayGapErrorCarriesOnlyBoundedSequenceDetails(t *testing.T) {
	outcome := domainResult(nil, &history.GapError{RequestedSequence: 100, EarliestSequence: 1024, LatestSequence: 2048})
	frame := errorFrameWithDetails("req_1", outcome.ErrorCode, "operation failed", false, outcome.Result)
	var envelope struct {
		Code    string            `json:"code"`
		Details map[string]uint64 `json:"details"`
	}
	if err := json.Unmarshal(frame.Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != "replay_gap" || envelope.Details["requested_sequence"] != 100 || envelope.Details["earliest_sequence"] != 1024 || envelope.Details["latest_sequence"] != 2048 || len(envelope.Details) != 3 {
		t.Fatalf("envelope=%s", frame.Payload)
	}
}

func TestStaleGenerationErrorCarriesCurrentGeneration(t *testing.T) {
	outcome := domainResult(nil, &session.StaleGenerationError{CurrentGeneration: 7})
	frame := errorFrameWithDetails("req_1", outcome.ErrorCode, "operation failed", false, outcome.Result)
	var envelope struct {
		Details map[string]uint64 `json:"details"`
	}
	if err := json.Unmarshal(frame.Payload, &envelope); err != nil || envelope.Details["current_generation"] != 7 || len(envelope.Details) != 1 {
		t.Fatalf("payload=%s err=%v", frame.Payload, err)
	}
}

type streamTestConnection struct {
	mu         sync.Mutex
	structured []protocol.Frame
	closeCode  int
}

func (*streamTestConnection) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *streamTestConnection) Write(data []byte) (int, error) { return len(data), nil }
func (*streamTestConnection) Close() error                     { return nil }
func (c *streamTestConnection) WriteStructured(frame protocol.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.structured = append(c.structured, frame)
	return nil
}
func (*streamTestConnection) WriteBinary(protocol.BinaryFrame) error { return nil }
func (c *streamTestConnection) CloseProtocol(code int, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeCode = code
	return nil
}

type errorOutputStream struct{ err error }

func (s errorOutputStream) Next(context.Context) (protocol.BinaryFrame, error) {
	return protocol.BinaryFrame{}, s.err
}
func (errorOutputStream) Close() error { return nil }

type releasingOutputStream struct {
	released *atomic.Int64
	sent     bool
}

func (s *releasingOutputStream) Next(context.Context) (protocol.BinaryFrame, error) {
	if s.sent {
		return protocol.BinaryFrame{}, io.EOF
	}
	s.sent = true
	return protocol.BinaryFrame{Channel: protocol.Stdout, Data: []byte("x"), Release: func() { s.released.Add(1) }}, nil
}
func (*releasingOutputStream) Close() error { return nil }

func TestTerminalStreamReleasesOutputAfterWrite(t *testing.T) {
	connection := &streamTestConnection{}
	server := &Server{}
	var released atomic.Int64
	server.wg.Add(1)
	server.stream(context.Background(), &lockedWriter{writer: connection, connection: connection}, connection, &releasingOutputStream{released: &released}, newTerminalConnectionState(), 1, "terminal.v1")
	if got := released.Load(); got != 1 {
		t.Fatalf("release calls = %d", got)
	}
}

func TestSlowConsumerStreamSendsBoundedErrorBefore4408Close(t *testing.T) {
	details, _ := json.Marshal(map[string]any{"queued_bytes": float64(1 << 20), "secret": "discard"})
	connection := &streamTestConnection{}
	server := &Server{}
	server.wg.Add(1)
	server.stream(context.Background(), &lockedWriter{writer: connection, connection: connection}, connection, errorOutputStream{&StreamError{Code: "slow_consumer", Details: details, CloseCode: protocol.CloseSlowConsumer}}, newTerminalConnectionState(), 1, "terminal.v1")
	if connection.closeCode != protocol.CloseSlowConsumer || len(connection.structured) != 1 {
		t.Fatalf("code=%d frames=%#v", connection.closeCode, connection.structured)
	}
	var envelope struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(connection.structured[0].Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != "slow_consumer" || envelope.Details["queued_bytes"] != float64(1<<20) || len(envelope.Details) != 1 {
		t.Fatalf("payload=%s", connection.structured[0].Payload)
	}
}

func TestTerminalStreamEndEmitsStructuredEvent(t *testing.T) {
	payload := json.RawMessage(`{"event":"terminal_stream_end","session_id":"ses_1","state":"exited","final_sequence":12,"exit":{"code":7}}`)
	connection := &streamTestConnection{}
	server := &Server{}
	server.wg.Add(1)
	server.stream(context.Background(), &lockedWriter{writer: connection, connection: connection}, connection, errorOutputStream{&StreamEnd{Payload: payload}}, newTerminalConnectionState(), 1, "terminal.v1")
	if connection.closeCode != 0 || len(connection.structured) != 1 {
		t.Fatalf("code=%d frames=%#v", connection.closeCode, connection.structured)
	}
	frame := connection.structured[0]
	if frame.Type != "event" || frame.Capability != "terminal.v1" || frame.RequestID != "stream" || string(frame.Payload) != string(payload) {
		t.Fatalf("frame=%#v", frame)
	}
}

func TestHelloRejectsDuplicateCapabilities(t *testing.T) {
	server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{JournalBinding: "user:1"}, nil
	}, func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
		return operation.Outcome{}
	}, 1)
	client, peer := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.Serve(peer) }()
	payload := json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","terminal.v1","health.v1"]}`)
	if err := protocol.WriteFrame(client, protocol.Frame{Type: "hello", RequestID: "req_hello", Version: "1.0", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	response, err := protocol.ReadFrame(client)
	if err != nil || response.Type != "error" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	_ = client.Close()
	if err := <-done; !errors.Is(err, ErrHandshakeRequired) {
		t.Fatalf("serve err=%v", err)
	}
}

func TestServeAuthorizesBeforeHandlerAndReplaysResult(t *testing.T) {
	var mu sync.Mutex
	var order []string
	var calls atomic.Int32
	server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
		mu.Lock()
		order = append(order, "authorize")
		mu.Unlock()
		return Authorization{JournalBinding: "user:1"}, nil
	}, func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
		mu.Lock()
		order = append(order, "handle")
		mu.Unlock()
		calls.Add(1)
		return operation.Outcome{Result: json.RawMessage(`{"sessions":[]}`)}
	}, 2)
	client, peer := net.Pipe()
	go server.Serve(peer)
	hello(t, client)
	for _, requestID := range []string{"req_1", "req_2"} {
		if err := protocol.WriteFrame(client, request(requestID, "op_00000001", json.RawMessage(`{"action":"list"}`))); err != nil {
			t.Fatal(err)
		}
		response, err := protocol.ReadFrame(client)
		if err != nil || response.Type != "response" {
			t.Fatalf("response=%#v err=%v", response, err)
		}
		var payload struct {
			Replay bool `json:"replay"`
		}
		if err := json.Unmarshal(response.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Replay != (requestID == "req_2") {
			t.Fatalf("request=%s replay=%v", requestID, payload.Replay)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls=%d", calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != "authorize" || order[1] != "handle" || order[2] != "authorize" {
		t.Fatalf("order=%v", order)
	}
}

func TestServerMetricsUseOnlyBoundedOperationLabels(t *testing.T) {
	metrics, err := hosttelemetry.NewRegistry(hosttelemetry.DefaultDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{JournalBinding: "principal"}, nil
	}, func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
		return operation.Outcome{Result: json.RawMessage(`{}`)}
	}, 2)
	server.config.Metrics = metrics
	client, peer := net.Pipe()
	go server.Serve(peer)
	hello(t, client)
	for _, requestID := range []string{"req_1", "req_2"} {
		if response := sendRequest(t, client, request(requestID, "op_00000001", json.RawMessage(`{"action":"list"}`))); response.Type != "response" {
			t.Fatalf("response=%#v", response)
		}
	}
	series := metrics.Snapshot()
	operationSeries := 0
	for _, value := range series {
		if value.Name != "paperboat_runtime_operations_total" {
			continue
		}
		operationSeries++
		if value.Labels["component"] != "session" || value.Labels["result"] != "ok" && value.Labels["result"] != "replayed" || value.Value != 1 {
			t.Fatalf("series=%#v", series)
		}
	}
	if operationSeries != 2 {
		t.Fatalf("operation series=%d all=%#v", operationSeries, series)
	}
}

func TestExplicitCancellationStopsOperation(t *testing.T) {
	started := make(chan struct{})
	server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{JournalBinding: "user:1"}, nil
	}, func(ctx context.Context, _ Authorization, _ string, _ json.RawMessage) operation.Outcome {
		close(started)
		<-ctx.Done()
		return operation.Outcome{ErrorCode: "operation_canceled"}
	}, 1)
	client, peer := net.Pipe()
	go server.Serve(peer)
	hello(t, client)
	if err := protocol.WriteFrame(client, request("req_1", "op_00000001", json.RawMessage(`{"action":"close"}`))); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := protocol.WriteFrame(client, protocol.Frame{Type: "cancel", RequestID: "req_cancel", Version: "1.0", OperationID: "op_00000001"}); err != nil {
		t.Fatal(err)
	}
	seenCancel, seenResult := false, false
	for !seenCancel || !seenResult {
		frame, err := protocol.ReadFrame(client)
		if err != nil {
			t.Fatal(err)
		}
		seenCancel = seenCancel || frame.RequestID == "req_cancel" && frame.Type == "response"
		seenResult = seenResult || frame.RequestID == "req_1" && frame.Type == "error"
	}
}

func TestCancellationCannotCrossAuthorizationBinding(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{JournalBinding: "principal-a"}, nil
	}, func(ctx context.Context, _ Authorization, _ string, _ json.RawMessage) operation.Outcome {
		close(started)
		<-ctx.Done()
		close(canceled)
		return operation.Outcome{ErrorCode: "operation_canceled"}
	}, 2)
	clientA, peerA := net.Pipe()
	go server.ServeAuthenticated(peerA, authorizerFunc(func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{JournalBinding: "principal-a"}, nil
	}))
	hello(t, clientA)
	if err := protocol.WriteFrame(clientA, request("req_a", "op_00000001", json.RawMessage(`{"action":"close"}`))); err != nil {
		t.Fatal(err)
	}
	<-started

	clientB, peerB := net.Pipe()
	go server.ServeAuthenticated(peerB, authorizerFunc(func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{JournalBinding: "principal-b"}, nil
	}))
	hello(t, clientB)
	if response := sendRequest(t, clientB, protocol.Frame{Type: "cancel", RequestID: "req_cancel_b", Version: "1.0", OperationID: "op_00000001"}); response.Type != "response" {
		t.Fatalf("cancel b=%#v", response)
	}
	select {
	case <-canceled:
		t.Fatal("different principal canceled operation")
	case <-time.After(20 * time.Millisecond):
	}
	if err := protocol.WriteFrame(clientA, protocol.Frame{Type: "cancel", RequestID: "req_cancel_a", Version: "1.0", OperationID: "op_00000001"}); err != nil {
		t.Fatal(err)
	}
	for {
		response, err := protocol.ReadFrame(clientA)
		if err != nil {
			t.Fatal(err)
		}
		if response.RequestID == "req_cancel_a" {
			if response.Type != "response" {
				t.Fatalf("cancel a=%#v", response)
			}
			break
		}
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("own principal could not cancel operation")
	}
}

func TestDisconnectDoesNotCancelDurableOperation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{JournalBinding: "user:1"}, nil
	}, func(ctx context.Context, _ Authorization, _ string, _ json.RawMessage) operation.Outcome {
		close(started)
		select {
		case <-ctx.Done():
			t.Errorf("operation canceled on disconnect: %v", ctx.Err())
		case <-release:
		}
		close(completed)
		return operation.Outcome{Result: json.RawMessage(`{"closed":true}`)}
	}, 1)
	client, peer := net.Pipe()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(peer) }()
	hello(t, client)
	if err := protocol.WriteFrame(client, request("req_1", "op_00000001", json.RawMessage(`{"action":"close"}`))); err != nil {
		t.Fatal(err)
	}
	<-started
	_ = client.Close()
	<-serveDone
	close(release)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("operation did not complete after disconnect")
	}
}

func TestConcurrencyLimitRejectsBeforeSecondHandler(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{JournalBinding: "user:1"}, nil
	}, func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
		calls.Add(1)
		close(started)
		<-release
		return operation.Outcome{}
	}, 1)
	client, peer := net.Pipe()
	go server.Serve(peer)
	hello(t, client)
	if err := protocol.WriteFrame(client, request("req_1", "op_00000001", json.RawMessage(`{"action":"one"}`))); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := protocol.WriteFrame(client, request("req_2", "op_00000002", json.RawMessage(`{"action":"two"}`))); err != nil {
		t.Fatal(err)
	}
	response, err := protocol.ReadFrame(client)
	if err != nil || response.RequestID != "req_2" || response.Type != "error" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	close(release)
	if calls.Load() != 1 {
		t.Fatalf("handler calls=%d", calls.Load())
	}
}

func TestTerminalCreationErrorEnvelopeRetainsBoundedPlatformErrno(t *testing.T) {
	for _, number := range []int{5, 10060, 65535, -1, 65536} {
		details, _ := json.Marshal(map[string]any{"error_type": "syscall.Errno", "errno": number})
		response := errorFrameWithDetails("req_1", "terminal_create_failed", "terminal creation failed", false, details)
		var envelope struct {
			Details map[string]any `json:"details"`
		}
		if err := json.Unmarshal(response.Payload, &envelope); err != nil {
			t.Fatal(err)
		}
		if number < 0 || number > 65535 {
			if len(envelope.Details) != 0 {
				t.Fatal("invalid platform metadata reached wire")
			}
		} else if envelope.Details["errno"] != float64(number) {
			t.Fatalf("platform errno %d lost in wire envelope", number)
		}
	}
}

type gatedWriter struct {
	mu      sync.Mutex
	writes  [][]byte
	entered chan struct{}
	gate    chan struct{}
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	first := len(w.writes) == 0
	w.writes = append(w.writes, append([]byte(nil), p...))
	w.mu.Unlock()
	if first {
		close(w.entered)
		<-w.gate
	}
	return len(p), nil
}

// A flood must never delay the answer to a keystroke, heartbeat or probe: control frames go before
// terminal output already waiting for the same connection, and output keeps its own order.
func TestControlFramesGoBeforeQueuedTerminalOutput(t *testing.T) {
	out := &gatedWriter{entered: make(chan struct{}), gate: make(chan struct{})}
	writer := &lockedWriter{writer: out}
	defer writer.close()
	errs := make(chan error, 3)
	go func() {
		errs <- writer.writeTerminalOutput(1, protocol.BinaryFrame{Channel: 1, Data: []byte("first-output")})
	}()
	<-out.entered // the first output write is in progress and holds the connection
	go func() {
		errs <- writer.writeTerminalOutput(1, protocol.BinaryFrame{Channel: 1, Data: []byte("second-output")})
	}()
	time.Sleep(50 * time.Millisecond) // the second output is queued
	go func() {
		errs <- writer.write(protocol.Frame{Type: "heartbeat", RequestID: "probe_1", Version: protocol.ProtocolVersion})
	}()
	time.Sleep(50 * time.Millisecond)
	close(out.gate)
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	out.mu.Lock()
	defer out.mu.Unlock()
	var order []string
	for _, record := range out.writes {
		switch {
		case bytes.Contains(record, []byte("first-output")):
			order = append(order, "first")
		case bytes.Contains(record, []byte("probe_1")):
			order = append(order, "probe")
		case bytes.Contains(record, []byte("second-output")):
			order = append(order, "second")
		}
	}
	if strings.Join(order, ",") != "first,probe,second" {
		t.Fatalf("write order %v: the probe answer must not wait behind queued output", order)
	}
}

// A client resuming (tab shown, laptop awake) checks its connection with a probe and its own short
// deadline; the host answers a probe at once, and leaves ordinary heartbeats unanswered.
func TestServeAnswersAProbeHeartbeat(t *testing.T) {
	journal, err := operation.NewJournal(8)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Negotiator: protocol.Negotiator{Available: map[string]bool{"terminal.v1": true, "health.v1": true}},
		Journal:    journal,
		Authorizer: authorizerFunc(func(context.Context, protocol.Frame) (Authorization, error) {
			return Authorization{JournalBinding: "probe-test"}, nil
		}),
		Handler: handlerFunc(func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
			return operation.Outcome{}
		}),
		MaxConcurrent:     1,
		HeartbeatInterval: time.Hour,
		MutationDeadline:  time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, peer := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.Serve(peer) }()
	defer func() { _ = client.Close(); <-done }()
	hello(t, client)
	for _, id := range []string{"heartbeat", "probe_7"} {
		if err := protocol.WriteFrame(client, protocol.Frame{Type: "heartbeat", RequestID: id, Version: protocol.ProtocolVersion}); err != nil {
			t.Fatal(err)
		}
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := protocol.ReadFrame(client)
	if err != nil {
		t.Fatalf("no answer to the probe: %v", err)
	}
	if frame.Type != "heartbeat" || frame.RequestID != "probe_7" {
		t.Fatalf("first frame after the probe = %s %q, want the probe's heartbeat (and none for the plain heartbeat)", frame.Type, frame.RequestID)
	}
}
