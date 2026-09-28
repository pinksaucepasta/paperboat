//go:build js && wasm

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall/js"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

const (
	websocketQueueDepth = 32
	websocketWriteLimit = 1 << 20
	websocketWriteWait  = 10 * time.Second
	terminalInputLimit  = protocol.MaxBinaryFrame - 13
	maximumColumns      = 500
	maximumRows         = 200
)

var browserRuntime = struct {
	sync.Mutex
	identities  map[string]*browserIdentity
	connections map[string]*browserTerminal
}{identities: make(map[string]*browserIdentity), connections: make(map[string]*browserTerminal)}

type terminalConfig struct {
	URL               string
	Subprotocols      []string
	OwnerAccountID    string
	MachineID         string
	RootKeyID         string
	RootPublicKey     string
	RootFingerprint   string
	TerminalSessionID string
	Role              string
	FromSequence      uint64
	AtLiveBoundary    bool
}

type browserTerminal struct {
	id       string
	identity *browserIdentity
	callback js.Value
	config   terminalConfig

	mu              sync.Mutex
	websocket       *browserWSConn
	tls             *tls.Conn
	streamID        uint32
	attachmentID    string
	inputSequence   uint64
	resizeSequence  uint64
	cursorRequestID string
	attached        bool
	stopping        atomic.Bool
	ended           atomic.Bool
	stopOnce        sync.Once
	keyEvents       chan browserbroadcast.Epoch
}

type websocketMessage struct {
	data   []byte
	binary bool
}

type browserWSConn struct {
	socket js.Value

	messages   chan websocketMessage
	broadcasts chan []byte
	opened     chan struct{}
	done       chan struct{}
	failure    chan error

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	readBuffer    []byte
	readOffset    int
	closeErr      error
	openOnce      sync.Once
	doneOnce      sync.Once
	writeMu       sync.Mutex
	closeOnce     sync.Once
	handlers      []js.Func
}

func main() {
	api := js.Global().Get("Object").New()
	api.Set("createIdentity", js.FuncOf(apiCreateIdentity))
	api.Set("inspectRoot", js.FuncOf(apiInspectRoot))
	api.Set("discardIdentity", js.FuncOf(apiDiscardIdentity))
	api.Set("connect", js.FuncOf(apiConnect))
	api.Set("sendInput", js.FuncOf(apiSendInput))
	api.Set("resize", js.FuncOf(apiResize))
	api.Set("ack", js.FuncOf(apiAck))
	api.Set("cursor", js.FuncOf(apiCursor))
	api.Set("leave", js.FuncOf(apiLeave))
	js.Global().Set("paperboatBrowserTerminal", api)
	select {}
}

func apiCreateIdentity(_ js.Value, _ []js.Value) any {
	identity, err := newBrowserIdentity(time.Now())
	if err != nil {
		return errorObject("identity_generation_failed", "A temporary browser key could not be created.")
	}
	id, err := randomID("browser_")
	if err != nil {
		zero(identity.PrivateKey)
		return errorObject("identity_generation_failed", "A temporary browser key could not be created.")
	}
	browserRuntime.Lock()
	browserRuntime.identities[id] = identity
	browserRuntime.Unlock()
	return js.ValueOf(map[string]any{"id": id, "public_key_sha256": identity.Fingerprint})
}

func apiInspectRoot(_ js.Value, args []js.Value) any {
	if len(args) != 1 || args[0].Type() != js.TypeString {
		return errorObject("invalid_root", "The trusted owner root key is invalid.")
	}
	encoded := args[0].String()
	publicKey, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(publicKey) != encoded {
		return errorObject("invalid_root", "The trusted owner root key must be canonical unpadded base64url.")
	}
	keyID := accountRootKeyID(ed25519.PublicKey(publicKey))
	return js.ValueOf(map[string]any{"root_key_id": keyID, "fingerprint": keyID[4:]})
}

func apiDiscardIdentity(_ js.Value, args []js.Value) any {
	if len(args) > 0 {
		browserRuntime.Lock()
		identity := browserRuntime.identities[args[0].String()]
		delete(browserRuntime.identities, args[0].String())
		browserRuntime.Unlock()
		if identity != nil {
			zero(identity.PrivateKey)
		}
	}
	return nil
}

func apiConnect(_ js.Value, args []js.Value) any {
	if len(args) != 3 || args[1].Type() != js.TypeObject || args[2].Type() != js.TypeFunction {
		return errorObject("invalid_request", "The browser terminal request is invalid.")
	}
	identityID := args[0].String()
	config, err := parseTerminalConfig(args[1])
	if err != nil {
		return errorObject("invalid_request", "The browser terminal request is invalid.")
	}
	browserRuntime.Lock()
	identity := browserRuntime.identities[identityID]
	delete(browserRuntime.identities, identityID)
	browserRuntime.Unlock()
	if identity == nil {
		return errorObject("identity_missing", "The temporary browser key expired. Start the connection again.")
	}
	id, err := randomID("browser_connection_")
	if err != nil {
		zero(identity.PrivateKey)
		return errorObject("connection_failed", "The encrypted connection could not be started.")
	}
	connection := &browserTerminal{id: id, identity: identity, callback: args[2], config: config}
	browserRuntime.Lock()
	browserRuntime.connections[id] = connection
	browserRuntime.Unlock()
	go connection.run()
	return js.ValueOf(map[string]any{"id": id})
}

func apiSendInput(_ js.Value, args []js.Value) any {
	if len(args) != 2 {
		return errorObject("invalid_request", "Terminal input is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	data, err := bytesFromJS(args[1])
	if err != nil || len(data) == 0 || len(data) > 1<<20 {
		return errorObject("input_limit", "Terminal input is too large.")
	}
	if err := connection.sendInput(data); err != nil {
		return errorObject("input_failed", "Terminal input could not be sent. Reconnect before retrying.")
	}
	return nil
}

func apiResize(_ js.Value, args []js.Value) any {
	if len(args) != 3 {
		return errorObject("invalid_request", "Terminal size is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	columns, rows := args[1].Int(), args[2].Int()
	if columns < 1 || columns > maximumColumns || rows < 1 || rows > maximumRows {
		return errorObject("invalid_size", "Terminal size is outside the supported range.")
	}
	if err := connection.sendResize(uint16(columns), uint16(rows)); err != nil {
		return errorObject("resize_failed", "Terminal size could not be sent.")
	}
	return nil
}

func apiAck(_ js.Value, args []js.Value) any {
	if len(args) != 2 {
		return errorObject("invalid_request", "Terminal acknowledgement is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	nextSequence, err := strconv.ParseUint(args[1].String(), 10, 64)
	if err != nil {
		return errorObject("invalid_request", "Terminal acknowledgement is invalid.")
	}
	if err := connection.sendAck(nextSequence); err != nil {
		return errorObject("ack_failed", "Terminal replay could not be acknowledged.")
	}
	return nil
}

func apiCursor(_ js.Value, args []js.Value) any {
	if len(args) != 1 {
		return errorObject("invalid_request", "Terminal cursor request is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	requestID, err := connection.sendCursor()
	if err != nil {
		return errorObject("cursor_failed", "The terminal position could not be requested.")
	}
	return js.ValueOf(map[string]any{"request_id": requestID})
}

func apiLeave(_ js.Value, args []js.Value) any {
	if len(args) == 0 {
		return nil
	}
	connection := getConnection(args[0].String())
	if connection != nil {
		connection.leave()
	}
	return nil
}

func parseTerminalConfig(value js.Value) (terminalConfig, error) {
	config := terminalConfig{
		URL:               stringProperty(value, "url"),
		OwnerAccountID:    stringProperty(value, "owner_account_id"),
		MachineID:         stringProperty(value, "machine_id"),
		RootKeyID:         stringProperty(value, "root_key_id"),
		RootPublicKey:     stringProperty(value, "root_public_key"),
		RootFingerprint:   stringProperty(value, "root_fingerprint"),
		TerminalSessionID: stringProperty(value, "terminal_session_id"),
		Role:              stringProperty(value, "role"),
		FromSequence:      0,
		AtLiveBoundary:    value.Get("at_live_boundary").Type() == js.TypeBoolean && value.Get("at_live_boundary").Bool(),
	}
	sequenceText := stringProperty(value, "from_sequence")
	if sequenceText != "" {
		parsed, err := strconv.ParseUint(sequenceText, 10, 64)
		if err != nil {
			return terminalConfig{}, err
		}
		config.FromSequence = parsed
	}
	protocolValues := value.Get("subprotocols")
	if protocolValues.Type() != js.TypeObject || protocolValues.Get("length").Int() < 1 || protocolValues.Get("length").Int() > 2 {
		return terminalConfig{}, errors.New("invalid terminal subprotocols")
	}
	for index := 0; index < protocolValues.Get("length").Int(); index++ {
		config.Subprotocols = append(config.Subprotocols, protocolValues.Index(index).String())
	}
	if config.Role != "owner" && config.Role != "viewer" && config.Role != "interactive" {
		return terminalConfig{}, errors.New("invalid browser terminal role")
	}
	if len(config.RootPublicKey) == 0 || len(config.RootFingerprint) != 64 || config.RootKeyID == "" {
		return terminalConfig{}, errors.New("invalid imported owner root")
	}
	rootPublic, err := base64.RawURLEncoding.Strict().DecodeString(config.RootPublicKey)
	if err != nil || len(rootPublic) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(rootPublic) != config.RootPublicKey || accountRootKeyID(ed25519.PublicKey(rootPublic)) != config.RootKeyID {
		return terminalConfig{}, errors.New("invalid imported owner root")
	}
	rootDigest := accountRootKeyID(ed25519.PublicKey(rootPublic))[4:]
	if !strings.EqualFold(rootDigest, config.RootFingerprint) || rootDigest != config.RootFingerprint {
		return terminalConfig{}, errors.New("imported owner root fingerprint does not match")
	}
	parsedURL, err := url.Parse(config.URL)
	if err != nil || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.Fragment != "" || parsedURL.RawQuery != "" || parsedURL.Path == "" || parsedURL.Scheme != "wss" {
		return terminalConfig{}, errors.New("invalid browser stream URL")
	}
	if len(config.Subprotocols) != 2 || config.Subprotocols[0] != websocketSubprotocol || !strings.HasPrefix(config.Subprotocols[1], "pb-ticket.") || len(config.Subprotocols[1]) > 128 {
		return terminalConfig{}, errors.New("invalid browser stream subprotocols")
	}
	ticket := strings.TrimPrefix(config.Subprotocols[1], "pb-ticket.")
	ticketBytes, err := base64.RawURLEncoding.Strict().DecodeString(ticket)
	if err != nil || len(ticketBytes) != 32 || base64.RawURLEncoding.EncodeToString(ticketBytes) != ticket {
		return terminalConfig{}, errors.New("invalid browser stream ticket")
	}
	if config.OwnerAccountID == "" || config.MachineID == "" || config.TerminalSessionID == "" {
		return terminalConfig{}, errors.New("missing terminal identity")
	}
	return config, nil
}

func (connection *browserTerminal) run() {
	defer connection.cleanup()
	connection.emit("state", map[string]any{"state": "connecting"})
	if err := connection.connectAndServe(); err != nil {
		if connection.stopping.Load() {
			return
		}
		var connectErr *terminalConnectError
		if errors.As(err, &connectErr) {
			values := map[string]any{"state": connectErr.state, "code": connectErr.code, "message": connectErr.message}
			for name, value := range connectErr.details {
				values[name] = value
			}
			connection.emit("state", values)
		} else {
			connection.emit("state", map[string]any{"state": "transport-error", "code": "connection_failed", "message": "The encrypted terminal connection failed. You can reconnect."})
		}
	}
}

type terminalConnectError struct {
	state   string
	code    string
	message string
	details map[string]any
}

func (e *terminalConnectError) Error() string { return e.message }

func (connection *browserTerminal) connectAndServe() error {
	ws, err := newBrowserWSConn(connection.config.URL, connection.config.Subprotocols)
	if err != nil {
		return &terminalConnectError{state: "transport-error", code: "connection_failed", message: "The encrypted terminal connection could not be opened."}
	}
	connection.mu.Lock()
	connection.websocket = ws
	connection.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ws.waitOpen(ctx); err != nil {
		return &terminalConnectError{state: "transport-error", code: "connection_failed", message: "The encrypted terminal connection could not be opened."}
	}
	identityMessage, err := ws.readInitialMessage(ctx)
	if err != nil || !identityMessage.binary {
		return &terminalConnectError{state: "transport-error", code: "machine_identity_unverified", message: "The machine did not provide a valid signed identity."}
	}
	rootBytes, _ := base64.RawURLEncoding.DecodeString(connection.config.RootPublicKey)
	peer, err := verifyIdentityEnvelope(identityMessage.data, ed25519.PublicKey(rootBytes), connection.config.RootKeyID, connection.config.OwnerAccountID, connection.config.MachineID, time.Now())
	if err != nil {
		return &terminalConnectError{state: "denied", code: "machine_identity_unverified", message: "The machine key did not match the trusted owner key and expected machine."}
	}
	if ws.socket.Get("protocol").String() != websocketSubprotocol {
		return &terminalConnectError{state: "transport-error", code: "subprotocol_mismatch", message: "The browser terminal transport did not select its encrypted protocol."}
	}
	clientTLS := tls.Client(ws, tlsClientConfig(connection.identity, peer, time.Now))
	if err := clientTLS.HandshakeContext(ctx); err != nil {
		return &terminalConnectError{state: "denied", code: "machine_identity_unverified", message: "The machine did not prove the key in its signed identity."}
	}
	connection.mu.Lock()
	connection.tls = clientTLS
	connection.mu.Unlock()
	connection.emit("identity-verified", map[string]any{
		"owner_account_id": connection.config.OwnerAccountID,
		"machine_id":       connection.config.MachineID,
		"root_key_id":      connection.config.RootKeyID,
	})
	if err := connection.sendHello(clientTLS); err != nil {
		return &terminalConnectError{state: "transport-error", code: "protocol_error", message: "The machine rejected the encrypted terminal handshake."}
	}
	welcome, err := readStructuredFrame(clientTLS)
	if err != nil || welcome.Type != "welcome" || welcome.Version != protocol.ProtocolVersion {
		return &terminalConnectError{state: "transport-error", code: "protocol_error", message: "The machine rejected the encrypted terminal handshake."}
	}
	requestID, err := randomID("req_browser_")
	if err != nil {
		return err
	}
	operationID, err := randomID("op_browser_")
	if err != nil {
		return err
	}
	attachPayload, err := json.Marshal(map[string]any{"action": "attach", "session_id": connection.config.TerminalSessionID, "from_sequence": connection.config.FromSequence, "at_live_boundary": connection.config.AtLiveBoundary})
	if err != nil {
		return err
	}
	attach := protocol.Frame{Type: "request", RequestID: requestID, Version: protocol.ProtocolVersion, OperationID: operationID, Capability: "terminal.v1", DeadlineMS: 10000, Payload: attachPayload}
	if err := writeStructuredFrame(clientTLS, attach); err != nil {
		return &terminalConnectError{state: "transport-error", code: "connection_failed", message: "The terminal attachment could not be requested."}
	}
	response, err := readStructuredFrame(clientTLS)
	if err != nil {
		return &terminalConnectError{state: "transport-error", code: "connection_failed", message: "The terminal attachment response could not be read."}
	}
	if response.Type == "error" {
		var serverError struct {
			Code    string `json:"code"`
			Details struct {
				EarliestSequence uint64 `json:"earliest_sequence"`
				LatestSequence   uint64 `json:"latest_sequence"`
			} `json:"details"`
		}
		_ = json.Unmarshal(response.Payload, &serverError)
		if serverError.Code == "credential_expired" {
			return &terminalConnectError{state: "reconnecting", code: serverError.Code, message: "The short-lived terminal ticket expired. Requesting a fresh ticket."}
		}
		if serverError.Code == "replay_gap" && serverError.Details.EarliestSequence > connection.config.FromSequence {
			return &terminalConnectError{state: "reconnecting", code: serverError.Code, message: "Some earlier terminal output is no longer available.", details: map[string]any{
				"earliest_sequence": strconv.FormatUint(serverError.Details.EarliestSequence, 10),
				"latest_sequence":   strconv.FormatUint(serverError.Details.LatestSequence, 10),
			}}
		}
		if serverError.Code == "not_found_or_forbidden" || serverError.Code == "stale_generation" {
			return &terminalConnectError{state: "denied", code: serverError.Code, message: "You do not have access to this terminal. Ask its owner to restore access."}
		}
		if serverError.Code == "terminal_closed" || serverError.Code == "closed" {
			return &terminalConnectError{state: "closed", code: serverError.Code, message: "This terminal has closed."}
		}
		return &terminalConnectError{state: "transport-error", code: serverError.Code, message: "The machine could not attach this terminal."}
	}
	if response.Type != "response" || response.RequestID != requestID {
		return &terminalConnectError{state: "transport-error", code: "invalid_attach_response", message: "The machine returned an invalid terminal attachment."}
	}
	attachState, err := parseAttachResponse(response.Payload)
	if err != nil {
		return &terminalConnectError{state: "transport-error", code: "invalid_attach_response", message: "The machine returned an invalid terminal attachment."}
	}
	connection.mu.Lock()
	connection.streamID = attachState.StreamID
	connection.attachmentID = attachState.AttachmentID
	connection.inputSequence = attachState.InputSequence
	connection.resizeSequence = 0
	connection.attached = true
	connection.mu.Unlock()
	checkpoint, checkpointSequence, err := readScreenCheckpoint(clientTLS, attachState.StreamID)
	if err != nil {
		return &terminalConnectError{state: "transport-error", code: "invalid_checkpoint", message: "The machine could not restore the terminal screen."}
	}
	connection.keyEvents = make(chan browserbroadcast.Epoch, 4)
	connection.emit("attached", map[string]any{
		"role":              connection.config.Role,
		"stream_id":         attachState.StreamID,
		"input_sequence":    strconv.FormatUint(attachState.InputSequence, 10),
		"from_sequence":     strconv.FormatUint(attachState.ReplayFrom, 10),
		"to_sequence":       strconv.FormatUint(attachState.ReplayTo, 10),
		"earliest_sequence": strconv.FormatUint(attachState.EarliestSequence, 10),
		"latest_sequence":   strconv.FormatUint(attachState.LatestSequence, 10),
		"columns":           attachState.Columns,
		"rows":              attachState.Rows,
		"first_attachment":  attachState.FirstAttachment,
		"terminal_state":    attachState.State,
		"screen":            jsByteArray(checkpoint),
		"screen_sequence":   strconv.FormatUint(checkpointSequence, 10),
	})
	go connection.readBroadcast(ws, peer.Certificate.Claims.QUICPublicKey, attachState.Generation, checkpointSequence)
	return connection.readTerminalStream(clientTLS)
}

func readScreenCheckpoint(stream io.Reader, streamID uint32) ([]byte, uint64, error) {
	checkpoint := make([]byte, 0, 64<<10)
	var sequence uint64
	for {
		kind, payload, err := readApplicationFrame(stream)
		if err != nil || kind != appKindBinary {
			return nil, 0, errors.New("missing terminal screen checkpoint")
		}
		frame, err := protocol.DecodeTerminalOutput(payload)
		if err != nil || frame.Channel != protocol.TerminalScreenCheckpoint || frame.StreamID != streamID || len(frame.Data) == 0 || frame.Data[0] > 1 {
			return nil, 0, errors.New("invalid terminal screen checkpoint")
		}
		if len(checkpoint) == 0 {
			sequence = frame.StartSequence
		} else if frame.StartSequence != sequence {
			return nil, 0, errors.New("terminal screen checkpoint boundary changed")
		}
		if len(frame.Data)-1 > protocol.MaxTerminalScreenCheckpointBytes-len(checkpoint) {
			return nil, 0, errors.New("terminal screen checkpoint is too large")
		}
		checkpoint = append(checkpoint, frame.Data[1:]...)
		if frame.Data[0] == 1 {
			return checkpoint, sequence, nil
		}
	}
}

func (connection *browserTerminal) sendHello(writer io.Writer) error {
	requestID, err := randomID("req_browser_")
	if err != nil {
		return err
	}
	payload := json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1"]}`)
	return writeStructuredFrame(writer, protocol.Frame{Type: "hello", RequestID: requestID, Version: protocol.ProtocolVersion, Payload: payload})
}

type terminalAttachState struct {
	StreamID         uint32
	AttachmentID     string
	InputSequence    uint64
	Generation       uint64
	Columns          int
	Rows             int
	FirstAttachment  bool
	State            string
	ReplayFrom       uint64
	ReplayTo         uint64
	EarliestSequence uint64
	LatestSequence   uint64
}

func parseAttachResponse(payload []byte) (terminalAttachState, error) {
	var envelope struct {
		Result struct {
			StreamID        uint32 `json:"stream_id"`
			AttachmentID    string `json:"attachment_id"`
			InputSequence   uint64 `json:"input_sequence"`
			FirstAttachment bool   `json:"first_attachment"`
			Session         struct {
				Snapshot struct {
					State      string `json:"state"`
					Generation uint64 `json:"generation"`
					Dimensions struct {
						Columns int `json:"columns"`
						Rows    int `json:"rows"`
					} `json:"dimensions"`
					EarliestSequence uint64 `json:"earliest_sequence"`
					LatestSequence   uint64 `json:"latest_sequence"`
				} `json:"snapshot"`
				Replay struct {
					FromSequence uint64 `json:"from_sequence"`
					ToSequence   uint64 `json:"to_sequence"`
				} `json:"replay"`
			} `json:"session"`
		} `json:"result"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return terminalAttachState{}, errors.New("invalid terminal attach response")
	}
	result := envelope.Result
	if result.StreamID == 0 || result.AttachmentID == "" || result.Session.Snapshot.Generation == 0 || result.Session.Snapshot.Dimensions.Columns < 1 || result.Session.Snapshot.Dimensions.Columns > maximumColumns || result.Session.Snapshot.Dimensions.Rows < 1 || result.Session.Snapshot.Dimensions.Rows > maximumRows {
		return terminalAttachState{}, errors.New("terminal attach response is missing its stream or geometry")
	}
	return terminalAttachState{
		StreamID: result.StreamID, AttachmentID: result.AttachmentID, InputSequence: result.InputSequence, Generation: result.Session.Snapshot.Generation,
		Columns: result.Session.Snapshot.Dimensions.Columns, Rows: result.Session.Snapshot.Dimensions.Rows,
		FirstAttachment: result.FirstAttachment,
		State:           result.Session.Snapshot.State, ReplayFrom: result.Session.Replay.FromSequence, ReplayTo: result.Session.Replay.ToSequence,
		EarliestSequence: result.Session.Snapshot.EarliestSequence, LatestSequence: result.Session.Snapshot.LatestSequence,
	}, nil
}

func (connection *browserTerminal) readTerminalStream(stream io.Reader) error {
	for {
		kind, payload, err := readApplicationFrame(stream)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return &terminalConnectError{state: "reconnecting", code: "connection_lost", message: "The terminal connection was interrupted. Reconnect to resume."}
			}
			return &terminalConnectError{state: "reconnecting", code: "connection_lost", message: "The terminal connection was interrupted. Reconnect to resume."}
		}
		if kind == appKindBinary {
			frame, decodeErr := protocol.DecodeTerminalOutput(payload)
			if decodeErr != nil {
				return &terminalConnectError{state: "transport-error", code: "invalid_output", message: "The machine sent invalid terminal output."}
			}
			connection.mu.Lock()
			streamID := connection.streamID
			connection.mu.Unlock()
			if frame.StreamID != streamID {
				return &terminalConnectError{state: "transport-error", code: "wrong_stream", message: "Terminal output did not match this attachment."}
			}
			if frame.Channel == protocol.TerminalBroadcastKey {
				if len(frame.Data) != 49 || frame.Data[0] != 1 {
					return &terminalConnectError{state: "transport-error", code: "invalid_output_key", message: "The device sent an invalid output key."}
				}
				var epoch browserbroadcast.Epoch
				copy(epoch.ID[:], frame.Data[1:17])
				copy(epoch.Key[:], frame.Data[17:])
				select {
				case connection.keyEvents <- epoch:
				default:
					return &terminalConnectError{state: "reconnecting", code: "output_key_lag", message: "The terminal output key changed too quickly. Reconnecting."}
				}
				continue
			}
			connection.emit("output", map[string]any{
				"stream_id":      frame.StreamID,
				"channel":        frame.Channel,
				"start_sequence": strconv.FormatUint(frame.StartSequence, 10),
				"next_sequence":  strconv.FormatUint(frame.StartSequence+uint64(len(frame.Data)), 10),
				"data":           jsByteArray(frame.Data),
			})
			continue
		}
		frame, decodeErr := decodeStructuredFrame(payload)
		if decodeErr != nil {
			return &terminalConnectError{state: "transport-error", code: "invalid_frame", message: "The machine sent an invalid terminal frame."}
		}
		connection.mu.Lock()
		cursorResponse := frame.RequestID != "" && frame.RequestID == connection.cursorRequestID
		if cursorResponse {
			connection.cursorRequestID = ""
		}
		connection.mu.Unlock()
		if cursorResponse {
			if frame.Type == "response" {
				var body struct {
					Result struct {
						LatestSequence uint64 `json:"latest_sequence"`
					} `json:"result"`
				}
				if json.Unmarshal(frame.Payload, &body) == nil {
					connection.emit("cursor", map[string]any{"request_id": frame.RequestID, "latest_sequence": strconv.FormatUint(body.Result.LatestSequence, 10)})
					continue
				}
			}
			connection.emit("cursor-error", map[string]any{"request_id": frame.RequestID})
			continue
		}
		if frame.Type == "error" {
			var serverError struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(frame.Payload, &serverError)
			if serverError.Code == "credential_expired" {
				return &terminalConnectError{state: "reconnecting", code: serverError.Code, message: "The short-lived terminal ticket expired. Requesting a fresh ticket."}
			}
			if serverError.Code == "not_found_or_forbidden" || serverError.Code == "stale_generation" {
				return &terminalConnectError{state: "denied", code: serverError.Code, message: "Terminal access was revoked. Ask its owner to restore access."}
			}
			return &terminalConnectError{state: "transport-error", code: serverError.Code, message: "The machine ended the terminal connection."}
		}
		if frame.Type == "event" {
			var event struct {
				Event string `json:"event"`
				State string `json:"state"`
			}
			_ = json.Unmarshal(frame.Payload, &event)
			if event.Event == "terminal_stream_end" {
				connection.ended.Store(true)
				connection.emit("state", map[string]any{"state": "closed", "message": "The remote terminal has closed."})
				return nil
			}
			connection.emit("event", map[string]any{"type": frame.Type, "capability": frame.Capability, "payload": json.RawMessage(frame.Payload)})
			continue
		}
		connection.emit("frame", map[string]any{"type": frame.Type, "capability": frame.Capability, "payload": json.RawMessage(frame.Payload)})
	}
}

// readBroadcast consumes one ordered edge fanout feed. Output is accepted only
// after the device sends its epoch key over the independently authenticated
// inner TLS channel; every record must also carry the device's Ed25519 proof.
func (connection *browserTerminal) readBroadcast(ws *browserWSConn, devicePublic ed25519.PublicKey, generation, checkpointSequence uint64) {
	keys := make(map[[16]byte]browserbroadcast.Epoch)
	indexes := make(map[[16]byte]uint64)
	keyOrder := make([][16]byte, 0, 4)
	pending := make([][]byte, 0, 16)
	cursor := checkpointSequence
	process := func() error {
		for len(pending) > 0 {
			hint, err := browserbroadcast.EpochHint(pending[0])
			if err != nil {
				return err
			}
			epoch, ok := keys[hint]
			if !ok {
				return nil
			}
			record, err := browserbroadcast.Open(pending[0], epoch, devicePublic)
			if err != nil || record.SessionID != connection.config.TerminalSessionID || record.Generation != generation || record.Channel != protocol.TerminalStdout && record.Channel != protocol.TerminalStderr {
				return browserbroadcast.ErrInvalidRecord
			}
			previous := indexes[hint]
			if previous != 0 && record.Index != previous+1 || record.Index == 0 {
				return browserbroadcast.ErrInvalidRecord
			}
			indexes[hint] = record.Index
			pending[0] = nil
			pending = pending[1:]
			end := record.StartSequence + uint64(len(record.Data))
			if end < record.StartSequence {
				return browserbroadcast.ErrInvalidRecord
			}
			if end <= cursor {
				continue
			}
			if record.StartSequence > cursor {
				return browserbroadcast.ErrInvalidRecord
			}
			data := record.Data
			if record.StartSequence < cursor {
				data = data[cursor-record.StartSequence:]
			}
			start := cursor
			cursor = end
			connection.emit("output", map[string]any{
				"stream_id":      connection.streamID,
				"channel":        record.Channel,
				"start_sequence": strconv.FormatUint(start, 10),
				"next_sequence":  strconv.FormatUint(cursor, 10),
				"data":           jsByteArray(data),
			})
		}
		return nil
	}
	for {
		select {
		case <-ws.done:
			return
		case epoch := <-connection.keyEvents:
			if _, exists := keys[epoch.ID]; !exists {
				keyOrder = append(keyOrder, epoch.ID)
			}
			keys[epoch.ID] = epoch
			if len(keyOrder) > 4 {
				delete(keys, keyOrder[0])
				delete(indexes, keyOrder[0])
				keyOrder = keyOrder[1:]
			}
		case record := <-ws.broadcasts:
			if len(pending) >= 256 {
				ws.fail(errors.New("terminal output is behind; reconnect for a fresh screen"))
				return
			}
			pending = append(pending, record)
		}
		if err := process(); err != nil {
			ws.fail(err)
			return
		}
	}
}

func writeStructuredFrame(writer io.Writer, frame protocol.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return writeApplicationFrame(writer, appKindStructured, encoded)
}

func readStructuredFrame(reader io.Reader) (protocol.Frame, error) {
	kind, payload, err := readApplicationFrame(reader)
	if err != nil {
		return protocol.Frame{}, err
	}
	if kind != appKindStructured {
		return protocol.Frame{}, errors.New("expected a structured terminal frame")
	}
	return decodeStructuredFrame(payload)
}

func decodeStructuredFrame(payload []byte) (protocol.Frame, error) {
	if len(payload) > protocol.MaxStructuredFrame {
		return protocol.Frame{}, errors.New("structured terminal frame is too large")
	}
	wire := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint32(wire, uint32(len(payload)))
	wire = append(wire, payload...)
	return protocol.ReadFrame(bytes.NewReader(wire))
}

func (connection *browserTerminal) sendInput(data []byte) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if !connection.attached || connection.tls == nil || connection.stopping.Load() {
		return net.ErrClosed
	}
	if connection.config.Role == "viewer" {
		return errors.New("viewer input is disabled")
	}
	for len(data) > 0 {
		count := min(len(data), terminalInputLimit)
		connection.inputSequence++
		encoded, err := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: connection.streamID, Sequence: connection.inputSequence, Data: data[:count]}, nil)
		if err != nil {
			connection.inputSequence--
			return err
		}
		if err := writeApplicationFrame(connection.tls, appKindBinary, encoded); err != nil {
			return err
		}
		data = data[count:]
	}
	return nil
}

func (connection *browserTerminal) sendResize(columns, rows uint16) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.config.Role != "owner" {
		return errors.New("only the owner may resize the terminal")
	}
	if !connection.attached || connection.tls == nil || connection.stopping.Load() {
		return net.ErrClosed
	}
	connection.resizeSequence++
	encoded, err := protocol.EncodeTerminalResize(protocol.TerminalResizeFrame{StreamID: connection.streamID, Columns: columns, Rows: rows, Sequence: connection.resizeSequence}, nil)
	if err != nil {
		connection.resizeSequence--
		return err
	}
	return writeApplicationFrame(connection.tls, appKindBinary, encoded)
}

func (connection *browserTerminal) sendAck(nextSequence uint64) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if !connection.attached || connection.tls == nil || connection.stopping.Load() {
		return net.ErrClosed
	}
	encoded, err := protocol.EncodeTerminalACK(protocol.TerminalACKFrame{StreamID: connection.streamID, NextSequence: nextSequence}, nil)
	if err != nil {
		return err
	}
	return writeApplicationFrame(connection.tls, appKindBinary, encoded)
}

func (connection *browserTerminal) sendCursor() (string, error) {
	requestID, err := randomID("req_cursor_")
	if err != nil {
		return "", err
	}
	operationID, err := randomID("op_cursor_")
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]string{"action": "cursor", "session_id": connection.config.TerminalSessionID})
	if err != nil {
		return "", err
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if !connection.attached || connection.tls == nil || connection.stopping.Load() || connection.cursorRequestID != "" {
		return "", net.ErrClosed
	}
	connection.cursorRequestID = requestID
	frame := protocol.Frame{Type: "request", RequestID: requestID, Version: protocol.ProtocolVersion, OperationID: operationID, Capability: "terminal.v1", DeadlineMS: 10000, Payload: payload}
	if err := writeStructuredFrame(connection.tls, frame); err != nil {
		connection.cursorRequestID = ""
		return "", err
	}
	return requestID, nil
}

func (connection *browserTerminal) sendDetach() {
	connection.mu.Lock()
	if !connection.attached || connection.tls == nil || connection.attachmentID == "" {
		connection.mu.Unlock()
		return
	}
	writer := connection.tls
	sessionID, attachmentID := connection.config.TerminalSessionID, connection.attachmentID
	connection.attached = false
	connection.mu.Unlock()
	payload, _ := json.Marshal(map[string]string{"session_id": sessionID, "attachment_id": attachmentID})
	requestID, err := randomID("req_browser_")
	if err != nil {
		return
	}
	_ = writeStructuredFrame(writer, protocol.Frame{Type: "detach", RequestID: requestID, Version: protocol.ProtocolVersion, Payload: payload})
}

func (connection *browserTerminal) leave() {
	connection.stopOnce.Do(func() {
		connection.stopping.Store(true)
		connection.sendDetach()
		connection.emit("state", map[string]any{"state": "left"})
		connection.closeTransport()
	})
}

func (connection *browserTerminal) cleanup() {
	connection.closeTransport()
	zero(connection.identity.PrivateKey)
	browserRuntime.Lock()
	delete(browserRuntime.connections, connection.id)
	browserRuntime.Unlock()
}

func (connection *browserTerminal) closeTransport() {
	connection.mu.Lock()
	clientTLS, websocket := connection.tls, connection.websocket
	connection.attached = false
	connection.mu.Unlock()
	if clientTLS != nil {
		_ = clientTLS.Close()
	} else if websocket != nil {
		_ = websocket.Close()
	}
}

func (connection *browserTerminal) emit(eventType string, values map[string]any) {
	if connection.callback.Type() != js.TypeFunction {
		return
	}
	values["type"] = eventType
	values["connection_id"] = connection.id
	defer func() { _ = recover() }()
	connection.callback.Invoke(jsObject(values))
}

func newBrowserWSConn(url string, protocols []string) (*browserWSConn, error) {
	constructor := js.Global().Get("WebSocket")
	if constructor.Type() != js.TypeFunction {
		return nil, errors.New("browser WebSocket is unavailable")
	}
	protocolArray := js.Global().Get("Array").New(len(protocols))
	for index, protocolName := range protocols {
		protocolArray.SetIndex(index, protocolName)
	}
	socket := constructor.New(url, protocolArray)
	socket.Set("binaryType", "arraybuffer")
	connection := &browserWSConn{socket: socket, messages: make(chan websocketMessage, websocketQueueDepth), broadcasts: make(chan []byte, 256), opened: make(chan struct{}), done: make(chan struct{}), failure: make(chan error, 1)}
	connection.handlers = append(connection.handlers,
		js.FuncOf(func(_ js.Value, _ []js.Value) any {
			connection.openOnce.Do(func() { close(connection.opened) })
			return nil
		}),
		js.FuncOf(func(_ js.Value, args []js.Value) any {
			if len(args) == 0 {
				connection.fail(errors.New("invalid WebSocket message"))
				return nil
			}
			eventData := args[0].Get("data")
			if eventData.Type() != js.TypeObject {
				connection.fail(errors.New("terminal transport must use binary WebSocket messages"))
				return nil
			}
			bytesValue := js.Global().Get("Uint8Array").New(eventData)
			length := bytesValue.Get("byteLength").Int()
			if length < 1 || length > websocketMessageMax {
				connection.fail(errors.New("WebSocket message size is invalid"))
				return nil
			}
			data := make([]byte, length)
			if js.CopyBytesToGo(data, bytesValue) != length {
				connection.fail(errors.New("WebSocket message could not be copied"))
				return nil
			}
			switch data[0] {
			case 0:
				if len(data) == 1 {
					connection.fail(errors.New("empty TLS envelope"))
					return nil
				}
				select {
				case connection.messages <- websocketMessage{data: data[1:], binary: true}:
				default:
					connection.fail(errors.New("WebSocket receive queue is full"))
				}
			case 1:
				if len(data) == 1 {
					connection.fail(errors.New("empty output envelope"))
					return nil
				}
				select {
				case connection.broadcasts <- data[1:]:
				default:
					connection.fail(errors.New("terminal output receive queue is full"))
				}
			default:
				connection.fail(errors.New("unknown terminal transport envelope"))
			}
			return nil
		}),
		js.FuncOf(func(_ js.Value, _ []js.Value) any {
			connection.fail(errors.New("WebSocket transport failed"))
			return nil
		}),
		js.FuncOf(func(_ js.Value, _ []js.Value) any { connection.finish(io.EOF); return nil }),
	)
	socket.Set("onopen", connection.handlers[0])
	socket.Set("onmessage", connection.handlers[1])
	socket.Set("onerror", connection.handlers[2])
	socket.Set("onclose", connection.handlers[3])
	return connection, nil
}

func (connection *browserWSConn) waitOpen(ctx context.Context) error {
	select {
	case <-connection.opened:
		if connection.socket.Get("protocol").String() != websocketSubprotocol {
			return errors.New("encrypted browser terminal subprotocol was not selected")
		}
		return nil
	case <-connection.done:
		return connection.terminalError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (connection *browserWSConn) readInitialMessage(ctx context.Context) (websocketMessage, error) {
	select {
	case message := <-connection.messages:
		return message, nil
	case <-connection.done:
		return websocketMessage{}, connection.terminalError()
	case <-ctx.Done():
		return websocketMessage{}, ctx.Err()
	}
}

func (connection *browserWSConn) Read(target []byte) (int, error) {
	if len(target) == 0 {
		return 0, nil
	}
	for {
		connection.mu.Lock()
		if connection.readOffset < len(connection.readBuffer) {
			count := copy(target, connection.readBuffer[connection.readOffset:])
			connection.readOffset += count
			if connection.readOffset == len(connection.readBuffer) {
				connection.readBuffer = nil
				connection.readOffset = 0
			}
			connection.mu.Unlock()
			return count, nil
		}
		deadline := connection.readDeadline
		connection.mu.Unlock()

		// Drain already received data before returning a close error.
		select {
		case message := <-connection.messages:
			if !message.binary {
				return 0, errors.New("terminal transport must use binary WebSocket messages")
			}
			connection.mu.Lock()
			connection.readBuffer = message.data
			connection.readOffset = 0
			connection.mu.Unlock()
			continue
		default:
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, timeoutError{}
		}
		var timer <-chan time.Time
		var stop *time.Timer
		if !deadline.IsZero() {
			stop = time.NewTimer(time.Until(deadline))
			timer = stop.C
		}
		select {
		case message := <-connection.messages:
			if stop != nil {
				stop.Stop()
			}
			if !message.binary {
				return 0, errors.New("terminal transport must use binary WebSocket messages")
			}
			connection.mu.Lock()
			connection.readBuffer = message.data
			connection.readOffset = 0
			connection.mu.Unlock()
		case <-connection.done:
			if stop != nil {
				stop.Stop()
			}
			return 0, connection.terminalError()
		case err := <-connection.failure:
			if stop != nil {
				stop.Stop()
			}
			return 0, err
		case <-timer:
			return 0, timeoutError{}
		}
	}
}

func (connection *browserWSConn) Write(data []byte) (written int, err error) {
	defer func() {
		if recover() != nil {
			// WebSocket.send may throw if the peer closes between readyState and send.
			written = 0
			err = errors.New("WebSocket send failed")
		}
	}()
	if len(data) == 0 {
		return 0, nil
	}
	if len(data) > websocketMessageMax {
		return 0, errors.New("TLS record exceeds the WebSocket bound")
	}
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	deadline := connection.currentWriteDeadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(websocketWriteWait)
	}
	for connection.socket.Get("readyState").Int() == 1 && connection.socket.Get("bufferedAmount").Int() > websocketWriteLimit {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, timeoutError{}
		}
		delay := 5 * time.Millisecond
		if !deadline.IsZero() && time.Until(deadline) < delay {
			delay = time.Until(deadline)
		}
		time.Sleep(delay)
	}
	if connection.socket.Get("readyState").Int() != 1 {
		return 0, connection.terminalError()
	}
	bytesValue := js.Global().Get("Uint8Array").New(len(data) + 1)
	wrapped := make([]byte, len(data)+1)
	copy(wrapped[1:], data)
	if js.CopyBytesToJS(bytesValue, wrapped) != len(wrapped) {
		return 0, errors.New("TLS record could not be copied to WebSocket")
	}
	connection.socket.Call("send", bytesValue.Get("buffer"))
	return len(data), nil
}

func (connection *browserWSConn) Close() error {
	connection.closeOnce.Do(func() {
		if connection.socket.Get("readyState").Int() < 2 {
			connection.socket.Call("close", 1000, "closed")
		}
		connection.detachHandlers()
		connection.finish(net.ErrClosed)
	})
	return nil
}

func (connection *browserWSConn) LocalAddr() net.Addr  { return browserAddr("browser-local") }
func (connection *browserWSConn) RemoteAddr() net.Addr { return browserAddr("browser-websocket") }

func (connection *browserWSConn) SetDeadline(deadline time.Time) error {
	connection.mu.Lock()
	connection.readDeadline, connection.writeDeadline = deadline, deadline
	connection.mu.Unlock()
	return nil
}

func (connection *browserWSConn) SetReadDeadline(deadline time.Time) error {
	connection.mu.Lock()
	connection.readDeadline = deadline
	connection.mu.Unlock()
	return nil
}

func (connection *browserWSConn) SetWriteDeadline(deadline time.Time) error {
	connection.mu.Lock()
	connection.writeDeadline = deadline
	connection.mu.Unlock()
	return nil
}

func (connection *browserWSConn) fail(err error) {
	select {
	case connection.failure <- err:
	default:
	}
	connection.finish(err)
	if connection.socket.Get("readyState").Int() < 2 {
		connection.socket.Call("close", 1002, "invalid terminal transport")
	}
}

func (connection *browserWSConn) finish(err error) {
	connection.doneOnce.Do(func() {
		connection.mu.Lock()
		connection.closeErr = err
		connection.mu.Unlock()
		close(connection.done)
	})
}

func (connection *browserWSConn) terminalError() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.closeErr == nil {
		return io.EOF
	}
	return connection.closeErr
}

func (connection *browserWSConn) currentWriteDeadline() time.Time {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.writeDeadline
}

func (connection *browserWSConn) detachHandlers() {
	for _, name := range []string{"onopen", "onmessage", "onerror", "onclose"} {
		connection.socket.Set(name, js.Null())
	}
	for _, handler := range connection.handlers {
		handler.Release()
	}
	connection.handlers = nil
}

type browserAddr string

func (addr browserAddr) Network() string { return "websocket" }
func (addr browserAddr) String() string  { return string(addr) }

type timeoutError struct{}

func (timeoutError) Error() string   { return "browser websocket deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func getConnection(id string) *browserTerminal {
	browserRuntime.Lock()
	defer browserRuntime.Unlock()
	return browserRuntime.connections[id]
}

func randomID(prefix string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func stringProperty(value js.Value, name string) string {
	property := value.Get(name)
	if property.Type() != js.TypeString {
		return ""
	}
	return property.String()
}

func bytesFromJS(value js.Value) ([]byte, error) {
	if value.Type() == js.TypeString {
		return []byte(value.String()), nil
	}
	if value.Type() != js.TypeObject || value.IsNull() {
		return nil, errors.New("expected byte array or string")
	}
	array := js.Global().Get("Uint8Array").New(value)
	length := array.Get("byteLength").Int()
	if length < 0 || length > 1<<20 {
		return nil, errors.New("byte array is too large")
	}
	data := make([]byte, length)
	if js.CopyBytesToGo(data, array) != length {
		return nil, errors.New("byte array could not be copied")
	}
	return data, nil
}

func jsByteArray(data []byte) js.Value {
	array := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(array, data)
	return array
}

func jsObject(values map[string]any) js.Value {
	result := js.Global().Get("Object").New()
	for name, value := range values {
		switch typed := value.(type) {
		case js.Value:
			result.Set(name, typed)
		case json.RawMessage:
			parsed := js.Global().Get("JSON").Call("parse", string(typed))
			result.Set(name, parsed)
		default:
			result.Set(name, js.ValueOf(typed))
		}
	}
	return result
}

func errorObject(code, message string) js.Value {
	return js.ValueOf(map[string]any{"error": code, "message": message})
}

var _ net.Conn = (*browserWSConn)(nil)
