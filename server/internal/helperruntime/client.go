package helperruntime

import (
	"bytes"
	"context"
	"github.com/google/uuid"

	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	protocolVersion = "1.0"
	subprotocol     = "paperboat.terminal.v1"
	maxFrameBytes   = 64 << 10
)

type Client struct {
	HTTPClient *http.Client
}

type Snapshot struct {
	Title                                 string        `json:"title"`
	CurrentDirectory                      string        `json:"current_directory,omitempty"`
	ForegroundProcess                     string        `json:"foreground_process,omitempty"`
	ID                                    string        `json:"id"`
	Name                                  string        `json:"name"`
	CWD                                   string        `json:"cwd"`
	Dimensions                            Dimensions    `json:"dimensions"`
	State                                 string        `json:"state"`
	Generation                            uint64        `json:"generation"`
	EarliestSequence                      uint64        `json:"earliest_sequence"`
	LatestSequence                        uint64        `json:"latest_sequence"`
	Exit                                  *ExitResult   `json:"exit,omitempty"`
	EligibleTransferDestinationMachineIDs []string      `json:"eligible_transfer_destination_machine_ids,omitempty"`
	Participants                          []Participant `json:"participants,omitempty"`
	TerminalModes                         TerminalModes `json:"terminal_modes"`
	LastActiveAt                          *time.Time    `json:"last_active_at,omitempty"`
}

type TerminalModes struct {
	AlternateScreen bool `json:"alternate_screen"`
	MouseClick      bool `json:"mouse_click"`
	MouseDrag       bool `json:"mouse_drag"`
	MouseMotion     bool `json:"mouse_motion"`
	MouseURXVT      bool `json:"mouse_urxvt"`
	MouseSGR        bool `json:"mouse_sgr"`
	FocusEvents     bool `json:"focus_events"`
	BracketedPaste  bool `json:"bracketed_paste"`
}

type Participant struct {
	AttachmentID string    `json:"attachment_id"`
	AccountID    string    `json:"account_id"`
	ClientID     string    `json:"client_id"`
	Role         string    `json:"role"`
	ConnectedAt  time.Time `json:"connected_at"`
}

type Dimensions struct {
	Columns uint16 `json:"columns"`
	Rows    uint16 `json:"rows"`
}

type ExitResult struct {
	Code     int       `json:"code"`
	Signal   string    `json:"signal,omitempty"`
	ExitedAt time.Time `json:"exited_at"`
}

type frame struct {
	Type        string          `json:"type"`
	RequestID   string          `json:"request_id"`
	Version     string          `json:"version"`
	OperationID string          `json:"operation_id,omitempty"`
	Capability  string          `json:"capability,omitempty"`
	DeadlineMS  uint32          `json:"deadline_ms,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

type remoteError struct {
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	RequestID  string          `json:"requestId"`
	Retryable  bool            `json:"retryable"`
	RetryAfter uint64          `json:"retryAfterMs,omitempty"`
	Recovery   string          `json:"recovery,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
}

// DiagnosticErrno retains remote platform metadata without turning it into a
// local syscall error. Remote type names and messages are never diagnostic labels.
func (e *remoteError) DiagnosticErrno() int {
	if e == nil || e.Code != "terminal_create_failed" {
		return 0
	}
	var details struct {
		ErrorType string `json:"error_type"`
		Errno     int    `json:"errno"`
	}
	if json.Unmarshal(e.Details, &details) != nil || len(details.ErrorType) > 100 || details.Errno < 1 || details.Errno > 65535 {
		return 0
	}
	return details.Errno
}

func (e *remoteError) Error() string {
	if e.Code == "terminal_create_failed" {
		return "terminal creation failed on host"
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func (c Client) Terminal(ctx context.Context, route, credential, action, sessionID, operationID string) (Snapshot, error) {
	return c.terminal(ctx, route, credential, action, sessionID, operationID, map[string]any{"action": action, "session_id": sessionID})
}

// TerminalViaEdge pins a browser snapshot to the edge selected for the
// terminal ticket. The runtime hostname remains in the path for edge route
// authorization; DNS for the public runtime alias cannot redirect this read.
func (c Client) TerminalViaEdge(ctx context.Context, edgeHost, runtimeHost, credential, action, sessionID, operationID string) (Snapshot, error) {
	if edgeHost == "" || runtimeHost == "" || strings.ContainsAny(edgeHost, "/:@?#") || strings.ContainsAny(runtimeHost, "/:@?#") {
		return Snapshot{}, protocolError("edge_route_invalid", "invalid helper runtime edge route", nil)
	}
	endpoint := (&url.URL{Scheme: "wss", Host: edgeHost, Path: "/v1/runtime/" + runtimeHost}).String()
	return c.terminalEndpoint(ctx, endpoint, credential, action, sessionID, operationID, map[string]any{"action": action, "session_id": sessionID})
}

// CreateTerminal starts the exact server-owned session on its enrolled host.
// The host's create-or-get response makes worker retries safe after a lost ACK.
func (c Client) CreateTerminal(ctx context.Context, route, credential, sessionID, operationID, name, cwd string) (Snapshot, error) {
	return c.terminal(ctx, route, credential, "create", sessionID, operationID, map[string]any{"action": "create", "session_id": sessionID, "name": name, "cwd": cwd, "columns": 80, "rows": 24, "environment": map[string]string{"COLORTERM": "truecolor"}, "existing_snapshot": true})
}

func (c Client) terminal(ctx context.Context, route, credential, action, sessionID, operationID string, requestPayload map[string]any) (Snapshot, error) {
	endpoint, err := runtimeEndpoint(route)
	if err != nil || credential == "" || sessionID == "" || action == "" || operationID == "" {
		return Snapshot{}, protocolError("operation_invalid", "invalid helper runtime operation", nil)
	}
	return c.terminalEndpoint(ctx, endpoint, credential, action, sessionID, operationID, requestPayload)
}

// websocketDialFailure preserves bounded handshake metadata independently of
// provider error text, which may contain private response content.
type websocketDialFailure struct {
	status int
	cause  error
}

func (e websocketDialFailure) Error() string { return "helper runtime websocket dial failed" }
func (e websocketDialFailure) Unwrap() error { return e.cause }
func (e websocketDialFailure) DiagnosticStatus() int {
	if e.status >= 100 && e.status <= 599 {
		return e.status
	}
	return 0
}

func (c Client) terminalEndpoint(ctx context.Context, endpoint, credential, action, sessionID, operationID string, requestPayload map[string]any) (Snapshot, error) {
	if endpoint == "" || credential == "" || sessionID == "" || action == "" || operationID == "" {
		return Snapshot{}, protocolError("operation_invalid", "invalid helper runtime operation", nil)
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+credential)
	connection, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient:   c.HTTPClient,
		HTTPHeader:   headers,
		Subprotocols: []string{subprotocol},
	})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
			if response.Body != nil {
				_ = response.Body.Close()
			}
		}
		return Snapshot{}, websocketDialFailure{status: status, cause: err}
	}
	defer connection.Close(websocket.StatusNormalClosure, "complete")
	connection.SetReadLimit(maxFrameBytes + 4)
	if connection.Subprotocol() != subprotocol {
		return Snapshot{}, protocolError("subprotocol_mismatch", "helper runtime did not negotiate required subprotocol", nil)
	}

	helloID, err := randomID("request")
	if err != nil {
		return Snapshot{}, err
	}
	helloPayload, _ := json.Marshal(map[string]any{
		"min_version": protocolVersion, "max_version": protocolVersion,
		"capabilities": []string{"terminal.v1", "health.v1"},
	})
	if err := writeFrame(ctx, connection, frame{Type: "hello", RequestID: helloID, Version: protocolVersion, Payload: helloPayload}); err != nil {
		return Snapshot{}, err
	}
	welcome, err := readFrame(ctx, connection)
	if err != nil {
		return Snapshot{}, err
	}
	if welcome.Type != "welcome" || welcome.RequestID != helloID {
		return Snapshot{}, protocolError("welcome_invalid", "helper runtime returned an invalid welcome", nil)
	}
	var negotiated struct {
		Version      string   `json:"version"`
		Capabilities []string `json:"capabilities"`
	}
	if err := decodeStrict(welcome.Payload, &negotiated); err != nil {
		return Snapshot{}, protocolError("capabilities_mismatch", "helper runtime did not negotiate required capabilities", err)
	}
	if negotiated.Version != protocolVersion || !contains(negotiated.Capabilities, "terminal.v1") || !contains(negotiated.Capabilities, "health.v1") {
		return Snapshot{}, protocolError("capabilities_mismatch", "helper runtime did not negotiate required capabilities", nil)
	}

	requestID, err := randomID("request")
	if err != nil {
		return Snapshot{}, err
	}
	payload, _ := json.Marshal(requestPayload)
	deadline := uint32(30000)
	if remaining, ok := ctx.Deadline(); ok {
		milliseconds := time.Until(remaining).Milliseconds()
		if milliseconds < 1 {
			return Snapshot{}, context.DeadlineExceeded
		}
		if milliseconds < int64(deadline) {
			deadline = uint32(milliseconds)
		}
	}
	request := frame{Type: "request", RequestID: requestID, Version: protocolVersion, OperationID: operationID, Capability: "terminal.v1", DeadlineMS: deadline, Payload: payload}
	if err := writeFrame(ctx, connection, request); err != nil {
		return Snapshot{}, err
	}
	for {
		response, err := readFrame(ctx, connection)
		if err != nil {
			return Snapshot{}, err
		}
		if response.Type == "heartbeat" {
			if err := writeFrame(ctx, connection, response); err != nil {
				return Snapshot{}, err
			}
			continue
		}
		if response.RequestID != requestID {
			return Snapshot{}, protocolError("response_mismatch", "helper runtime response did not match request", nil)
		}
		if response.Type == "error" {
			var remote remoteError
			if err := decodeStrict(response.Payload, &remote); err != nil {
				return Snapshot{}, protocolError("error_invalid", "helper runtime returned an invalid error", err)
			}
			if remote.Code == "" {
				return Snapshot{}, protocolError("error_invalid", "helper runtime returned an invalid error", nil)
			}
			return Snapshot{}, &remote
		}
		if response.Type != "response" {
			return Snapshot{}, protocolError("response_invalid", "helper runtime returned an invalid response", nil)
		}
		var envelope struct {
			Result json.RawMessage `json:"result"`
			Replay bool            `json:"replay"`
		}
		if err := decodeStrict(response.Payload, &envelope); err != nil {
			return Snapshot{}, protocolError("snapshot_invalid", "helper runtime returned an invalid terminal snapshot", err)
		}
		if action == "delete" {
			var empty struct{}
			if err := decodeStrict(envelope.Result, &empty); err != nil {
				return Snapshot{}, protocolError("delete_ack_invalid", "helper runtime returned an invalid delete acknowledgement", err)
			}
			return Snapshot{ID: sessionID, State: "deleted"}, nil
		}
		if action == "create" {
			var created struct {
				Snapshot
				Existing bool `json:"existing"`
			}
			if err := decodeStrict(envelope.Result, &created); err != nil {
				return Snapshot{}, protocolError("create_ack_invalid", "helper runtime returned an invalid terminal creation", err)
			}
			if created.ID != sessionID || !validParticipants(created.Participants) {
				return Snapshot{}, protocolError("create_ack_invalid", "helper runtime returned an invalid terminal creation", nil)
			}
			return created.Snapshot, nil
		}
		var snapshot Snapshot
		if err := decodeStrict(envelope.Result, &snapshot); err != nil {
			return Snapshot{}, protocolError("snapshot_encoding_invalid", "helper runtime returned an invalid terminal snapshot encoding", err)
		}
		if snapshot.ID != sessionID {
			return Snapshot{}, protocolError("snapshot_id_mismatch", "helper runtime returned the wrong terminal snapshot", nil)
		}
		if !validParticipants(snapshot.Participants) {
			return Snapshot{}, protocolError("participants_invalid", "helper runtime returned invalid terminal participants", nil)
		}
		return snapshot, nil
	}
}

func validParticipants(participants []Participant) bool {
	if len(participants) > 128 {
		return false
	}
	seen := make(map[string]struct{}, len(participants))
	for _, participant := range participants {
		if participant.AttachmentID == "" || participant.AccountID == "" || participant.ClientID == "" || participant.ConnectedAt.IsZero() || (participant.Role != "owner" && participant.Role != "viewer" && participant.Role != "interactive") {
			return false
		}
		if _, ok := seen[participant.AttachmentID]; ok {
			return false
		}
		seen[participant.AttachmentID] = struct{}{}
	}
	return true
}

func runtimeEndpoint(route string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(route))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
		return "", protocolError("route_invalid", "invalid helper runtime route", nil)
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/runtime"
	return u.String(), nil
}

func writeFrame(ctx context.Context, connection *websocket.Conn, value frame) error {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxFrameBytes {
		return protocolError("frame_invalid", "invalid helper runtime frame", nil)
	}
	return connection.Write(ctx, websocket.MessageText, encoded)
}

func readFrame(ctx context.Context, connection *websocket.Conn) (frame, error) {
	typ, data, err := connection.Read(ctx)
	if err != nil {
		return frame{}, err
	}
	if typ != websocket.MessageText || len(data) == 0 || len(data) > maxFrameBytes {
		return frame{}, protocolError("frame_invalid", "invalid helper runtime frame", nil)
	}
	var value frame
	if err := decodeStrict(data, &value); err != nil {
		return frame{}, protocolError("frame_invalid", "invalid helper runtime frame", err)
	}
	if value.Version != protocolVersion || value.Type == "" || value.RequestID == "" {
		return frame{}, protocolError("frame_invalid", "invalid helper runtime frame", nil)
	}
	return value, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return protocolError("json_trailing", "trailing JSON data", err)
		}
		return protocolError("json_trailing", "trailing JSON data", nil)
	}
	return nil
}

type protocolFailure struct {
	code    string
	message string
	cause   error
}

func (e *protocolFailure) Error() string { return e.message }

func (e *protocolFailure) Unwrap() error { return e.cause }

// DiagnosticProtocolCode reports a local finite stage without wire content.
func (e *protocolFailure) DiagnosticProtocolCode() string { return e.code }

func protocolError(code, message string, cause error) error {
	return &protocolFailure{code: code, message: message, cause: cause}
}

func randomID(prefix string) (string, error) {
	value, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return prefix + "_" + value.String(), nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
