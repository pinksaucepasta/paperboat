package helperruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"strings"
	"syscall"
	"testing"

	"github.com/coder/websocket"
)

func TestSnapshotStrictlyDecodesTerminalModesAndParticipants(t *testing.T) {
	var snapshot Snapshot
	raw := []byte(`{"id":"session_1","title":"vim main.go","current_directory":"/workspace/api","foreground_process":"vim","cwd":"/workspace","terminal_modes":{"alternate_screen":true,"mouse_click":true,"mouse_drag":true,"mouse_motion":true,"mouse_urxvt":true,"mouse_sgr":true,"focus_events":true,"bracketed_paste":true},"participants":[{"attachment_id":"attach_1","account_id":"account_1","client_id":"client_1","role":"viewer","connected_at":"2026-09-12T12:00:00Z"}]}`)
	if err := decodeStrict(raw, &snapshot); err != nil || snapshot.Title != "vim main.go" || snapshot.CurrentDirectory != "/workspace/api" || snapshot.ForegroundProcess != "vim" || snapshot.CWD != "/workspace" || !snapshot.TerminalModes.BracketedPaste || !snapshot.TerminalModes.AlternateScreen || !snapshot.TerminalModes.MouseClick || !snapshot.TerminalModes.MouseDrag || !snapshot.TerminalModes.MouseMotion || !snapshot.TerminalModes.MouseURXVT || !snapshot.TerminalModes.MouseSGR || !snapshot.TerminalModes.FocusEvents || !validParticipants(snapshot.Participants) {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestTerminalCloseRequiresMatchingCanonicalHelperAcknowledgement(t *testing.T) {
	requests := make(chan frame, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/runtime" || request.Header.Get("Authorization") != "Bearer signed-operation" {
			http.Error(writer, "invalid request", http.StatusUnauthorized)
			return
		}
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{subprotocol}})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "complete")
		hello, err := readFrame(request.Context(), connection)
		if err != nil {
			return
		}
		welcome, _ := json.Marshal(map[string]any{"version": protocolVersion, "capabilities": []string{"terminal.v1", "health.v1"}})
		if writeFrame(request.Context(), connection, frame{Type: "welcome", RequestID: hello.RequestID, Version: protocolVersion, Payload: welcome}) != nil {
			return
		}
		operation, err := readFrame(request.Context(), connection)
		if err != nil {
			return
		}
		requests <- operation
		payload := json.RawMessage(`{"result":{"id":"pts_1","name":"agent","cwd":"/workspace","dimensions":{"columns":80,"rows":24},"state":"closed","generation":1,"earliest_sequence":0,"latest_sequence":12,"exit":{"code":0,"signal":"SIGTERM","exited_at":"2026-07-22T16:00:00Z"}},"replay":false}`)
		_ = writeFrame(request.Context(), connection, frame{Type: "response", RequestID: operation.RequestID, Version: protocolVersion, Payload: payload})
	}))
	defer server.Close()

	observed, err := (Client{HTTPClient: server.Client()}).Terminal(context.Background(), server.URL, "signed-operation", "close", "pts_1", "tso_00000001")
	if err != nil {
		t.Fatal(err)
	}
	if observed.ID != "pts_1" || observed.State != "closed" || observed.CWD != "/workspace" || observed.LatestSequence != 12 || observed.Dimensions.Columns != 80 || observed.Exit == nil || observed.Exit.Signal != "SIGTERM" {
		t.Fatalf("observed = %#v", observed)
	}
	operation := <-requests
	if operation.Type != "request" || operation.Capability != "terminal.v1" || operation.OperationID != "tso_00000001" {
		t.Fatalf("operation = %#v", operation)
	}
	var payload struct {
		Action    string `json:"action"`
		SessionID string `json:"session_id"`
	}
	if decodeStrict(operation.Payload, &payload) != nil || payload.Action != "close" || payload.SessionID != "pts_1" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestTerminalRejectsMismatchedAcknowledgement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{subprotocol}})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "complete")
		hello, _ := readFrame(request.Context(), connection)
		welcome, _ := json.Marshal(map[string]any{"version": protocolVersion, "capabilities": []string{"terminal.v1", "health.v1"}})
		_ = writeFrame(request.Context(), connection, frame{Type: "welcome", RequestID: hello.RequestID, Version: protocolVersion, Payload: welcome})
		operation, _ := readFrame(request.Context(), connection)
		payload := json.RawMessage(`{"result":{"id":"pts_other","name":"agent","cwd":"/workspace","dimensions":{"columns":80,"rows":24},"state":"closed","generation":1,"earliest_sequence":0,"latest_sequence":0,"exit":{"code":0,"exited_at":"2026-07-22T16:00:00Z"}},"replay":false}`)
		_ = writeFrame(request.Context(), connection, frame{Type: "response", RequestID: operation.RequestID, Version: protocolVersion, Payload: payload})
	}))
	defer server.Close()

	if _, err := (Client{HTTPClient: server.Client()}).Terminal(context.Background(), server.URL, "signed-operation", "close", "pts_1", "tso_00000001"); err == nil {
		t.Fatal("mismatched session acknowledgement accepted")
	}
}

func TestTerminalMalformedWelcomeRetainsJSONCauseWithoutExposingFrame(t *testing.T) {
	const private = "HELPER_FRAME_CANARY"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{subprotocol}})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "complete")
		hello, err := readFrame(request.Context(), connection)
		if err != nil {
			return
		}
		malformed := []byte(`{"type":"welcome","request_id":"` + hello.RequestID + `","version":"1.0","payload":{"private":"` + private + `",}}`)
		_ = connection.Write(request.Context(), websocket.MessageText, malformed)
	}))
	defer server.Close()

	_, err := (Client{HTTPClient: server.Client()}).Terminal(context.Background(), server.URL, "signed-operation", "close", "pts_1", "tso_00000001")
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("malformed helper frame lost JSON cause: %T %v", err, err)
	}
	if err.Error() != "invalid helper runtime frame" || strings.Contains(err.Error(), private) {
		t.Fatalf("malformed helper frame exposed private payload: %v", err)
	}
	var coded interface{ DiagnosticProtocolCode() string }
	if !errors.As(err, &coded) || coded.DiagnosticProtocolCode() != "frame_invalid" {
		t.Fatal("malformed helper frame lost its safe protocol failure code")
	}
}

func TestCreateTerminalUsesExactSessionAndAcceptsReplay(t *testing.T) {
	requests := make(chan frame, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{subprotocol}})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "complete")
		hello, err := readFrame(request.Context(), connection)
		if err != nil {
			return
		}
		welcome, _ := json.Marshal(map[string]any{"version": protocolVersion, "capabilities": []string{"terminal.v1", "health.v1"}})
		if writeFrame(request.Context(), connection, frame{Type: "welcome", RequestID: hello.RequestID, Version: protocolVersion, Payload: welcome}) != nil {
			return
		}
		operation, err := readFrame(request.Context(), connection)
		if err != nil {
			return
		}
		requests <- operation
		payload := json.RawMessage(`{"result":{"id":"umts_1","name":"work","cwd":"/home/user","dimensions":{"columns":80,"rows":24},"state":"running","generation":2,"earliest_sequence":0,"latest_sequence":0,"existing":true},"replay":false}`)
		_ = writeFrame(request.Context(), connection, frame{Type: "response", RequestID: operation.RequestID, Version: protocolVersion, Payload: payload})
	}))
	defer server.Close()
	observed, err := (Client{HTTPClient: server.Client()}).CreateTerminal(context.Background(), server.URL, "signed-operation", "umts_1", "umtso_1", "work", "/home/user")
	if err != nil || observed.ID != "umts_1" || observed.Generation != 2 {
		t.Fatalf("created=%#v err=%v", observed, err)
	}
	operation := <-requests
	if operation.OperationID != "umtso_1" {
		t.Fatalf("operation=%#v", operation)
	}
	var input struct {
		Action           string            `json:"action"`
		SessionID        string            `json:"session_id"`
		Name             string            `json:"name"`
		CWD              string            `json:"cwd"`
		Columns          int               `json:"columns"`
		Rows             int               `json:"rows"`
		Environment      map[string]string `json:"environment"`
		ExistingSnapshot bool              `json:"existing_snapshot"`
	}
	if decodeStrict(operation.Payload, &input) != nil || input.Action != "create" || input.SessionID != "umts_1" || input.Name != "work" || input.CWD != "/home/user" || input.Columns != 80 || input.Rows != 24 || input.Environment["COLORTERM"] != "truecolor" || !input.ExistingSnapshot {
		t.Fatalf("create request=%#v", input)
	}
}

func TestHelperErrorAcceptsCanonicalRuntimeEnvelope(t *testing.T) {
	var reported remoteError
	if err := decodeStrict([]byte(`{"code":"not_found_or_forbidden","message":"Session unavailable","requestId":"req_1","retryable":false,"details":{"reason":"missing"}}`), &reported); err != nil || reported.Code != "not_found_or_forbidden" {
		t.Fatalf("runtime error=%#v decode=%v", reported, err)
	}
}

// RoundTripper closure exercises the actual dial boundary without changing
// production address or transport selection.
type helperDialTransport func(*http.Request) (*http.Response, error)

func (f helperDialTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTerminalViaEdgeDeniedHandshakePreservesSafeStatus(t *testing.T) {
	const private = "PRIVATE_HANDSHAKE_RESPONSE_CANARY"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime/runtime.example.test" || r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Error("edge request lost its exact runtime path or credential")
		}
		http.Error(w, private, http.StatusForbidden)
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := server.Client().Transport
	client := &http.Client{Transport: helperDialTransport(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(copy)
	})}
	_, err = (Client{HTTPClient: client}).TerminalViaEdge(context.Background(), "edge.example.test", "runtime.example.test", "test-credential", "snapshot", "session_1", "operation_1")
	var failure websocketDialFailure
	if !errors.As(err, &failure) || failure.DiagnosticStatus() != http.StatusForbidden || errors.Unwrap(failure) == nil {
		t.Fatal("denied handshake lost typed status or original cause")
	}
	fault := supportreport.ProjectFault(context.Background(), "paperboat-server", "http_terminal", "service_error", "terminal_host_unavailable", err)
	projected, marshalErr := json.Marshal(fault)
	if marshalErr != nil || fault.HTTPStatus != http.StatusForbidden || strings.Contains(string(projected), private) || strings.Contains(err.Error(), private) || err.Error() != "helper runtime websocket dial failed" {
		t.Fatal("handshake diagnostics lost status or exposed private response text")
	}
}

func TestTerminalViaEdgeTransportFailurePreservesCauseWithoutStatus(t *testing.T) {
	sentinel := errors.New("private transport cause")
	client := &http.Client{Transport: helperDialTransport(func(*http.Request) (*http.Response, error) { return nil, sentinel })}
	_, err := (Client{HTTPClient: client}).TerminalViaEdge(context.Background(), "edge.example.test", "runtime.example.test", "test-credential", "snapshot", "session_1", "operation_1")
	var failure websocketDialFailure
	if !errors.As(err, &failure) || !errors.Is(err, sentinel) || failure.DiagnosticStatus() != 0 || err.Error() != "helper runtime websocket dial failed" {
		t.Fatal("transport failure lost original cause, invented status, or exposed error text")
	}
}

func TestCreateTerminalPreservesRemoteErrnoWithoutLocalInterpretation(t *testing.T) {
	for _, number := range []int{5, 10060, -1, 65536} {
		t.Run(fmt.Sprint(number), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{subprotocol}})
				if err != nil {
					return
				}
				defer connection.Close(websocket.StatusNormalClosure, "complete")
				hello, err := readFrame(r.Context(), connection)
				if err != nil {
					return
				}
				welcome, _ := json.Marshal(map[string]any{"version": protocolVersion, "capabilities": []string{"terminal.v1", "health.v1"}})
				if writeFrame(r.Context(), connection, frame{Type: "welcome", RequestID: hello.RequestID, Version: protocolVersion, Payload: welcome}) != nil {
					return
				}
				operation, err := readFrame(r.Context(), connection)
				if err != nil {
					return
				}
				payload, _ := json.Marshal(map[string]any{"code": "terminal_create_failed", "message": "PRIVATE_REMOTE_ERROR", "requestId": operation.RequestID, "retryable": false, "details": map[string]any{"error_type": "PRIVATE_REMOTE_TYPE", "errno": number}})
				_ = writeFrame(r.Context(), connection, frame{Type: "error", RequestID: operation.RequestID, Version: protocolVersion, Payload: payload})
			}))
			defer server.Close()
			_, err := (Client{HTTPClient: server.Client()}).CreateTerminal(context.Background(), server.URL, "signed-operation", "umts_1", "umtso_1", "work", "/home/user")
			if err == nil {
				t.Fatal("remote creation failure returned success")
			}
			fault := supportreport.ProjectFault(context.Background(), "paperboat-server", "terminal_create", "service_error", "terminal_host_unavailable", err)
			expected := number
			if number < 1 || number > 65535 {
				expected = 0
			}
			if fault.Errno != expected {
				t.Fatalf("remote errno=%d, want %d", fault.Errno, expected)
			}
			if expected != 0 && fault.Cause != "system_error" {
				t.Fatalf("remote platform errno interpreted locally: %s", fault.Cause)
			}
			if errors.Is(err, syscall.EIO) {
				t.Fatal("remote Windows errno became local EIO")
			}
			encoded, _ := json.Marshal(fault)
			if strings.Contains(string(encoded), "PRIVATE_REMOTE") || strings.Contains(err.Error(), "PRIVATE_REMOTE") {
				t.Fatal("remote private metadata exposed")
			}
			if expected != 0 {
				deadlineFault := supportreport.ProjectFault(context.Background(), "paperboat-server", "terminal_create", "service_error", "terminal_host_unavailable", errors.Join(context.DeadlineExceeded, err))
				if deadlineFault.Cause != "deadline_exceeded" || deadlineFault.Errno != expected {
					t.Fatalf("joined deadline lost independent remote metadata: %#v", deadlineFault)
				}
			}
		})
	}
}

// A host whose managed ENV delivery is pending still launches the terminal and says so in
// launch_warnings; that ack is a successful creation, not create_ack_invalid.
func TestCreateTerminalAcceptsLaunchWarnings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{subprotocol}})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "complete")
		hello, err := readFrame(request.Context(), connection)
		if err != nil {
			return
		}
		welcome, _ := json.Marshal(map[string]any{"version": protocolVersion, "capabilities": []string{"terminal.v1", "health.v1"}})
		if writeFrame(request.Context(), connection, frame{Type: "welcome", RequestID: hello.RequestID, Version: protocolVersion, Payload: welcome}) != nil {
			return
		}
		operation, err := readFrame(request.Context(), connection)
		if err != nil {
			return
		}
		payload := json.RawMessage(`{"result":{"id":"umts_1","name":"work","cwd":"/home/user","dimensions":{"columns":80,"rows":24},"state":"running","generation":1,"earliest_sequence":0,"latest_sequence":0,"terminal_modes":{},"existing":false,"launch_warnings":["environment_pending"]},"replay":false}`)
		_ = writeFrame(request.Context(), connection, frame{Type: "response", RequestID: operation.RequestID, Version: protocolVersion, Payload: payload})
	}))
	defer server.Close()
	created, err := (Client{HTTPClient: server.Client()}).CreateTerminal(context.Background(), server.URL, "signed-operation", "umts_1", "umtso_1", "work", "/home/user")
	if err != nil || created.ID != "umts_1" || created.State != "running" {
		t.Fatalf("created=%#v err=%v", created, err)
	}
}
