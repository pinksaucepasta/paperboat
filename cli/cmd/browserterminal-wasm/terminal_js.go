//go:build js && wasm

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"syscall/js"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func parseTerminalConfig(value js.Value) (terminalConfig, error) {
	config := terminalConfig{
		Mode:              stringProperty(value, "mode"),
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
	if config.Mode == "" {
		config.Mode = connectionModeTerminal
	}
	if config.Mode != connectionModeTerminal && config.Mode != connectionModeInspector {
		return terminalConfig{}, errors.New("invalid connection mode")
	}
	if config.Mode == connectionModeInspector {
		config.Role, config.TerminalSessionID = "", ""
	} else if config.Role != "owner" && config.Role != "viewer" && config.Role != "interactive" {
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
	if config.OwnerAccountID == "" || config.MachineID == "" || config.Mode == connectionModeTerminal && config.TerminalSessionID == "" {
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
			connectErr = innermostConnectError(connectErr)
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

func (connection *browserTerminal) connectAndServe() error {
	ws, err := newBrowserWSConn(connection.config.URL, connection.config.Subprotocols)
	if err != nil {
		return &terminalConnectError{state: "transport-error", code: "connection_failed", message: "The encrypted terminal connection could not be opened.", cause: err}
	}
	connection.mu.Lock()
	connection.websocket = ws
	connection.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ws.waitOpen(ctx); err != nil {
		return &terminalConnectError{state: "transport-error", code: "connection_failed", message: "The encrypted terminal connection could not be opened.", cause: err}
	}
	identityMessage, err := ws.readInitialMessage(ctx)
	if err != nil || !identityMessage.binary {
		return &terminalConnectError{state: "transport-error", code: "machine_identity_unverified", message: "The machine did not provide a valid signed identity.", cause: err}
	}
	rootBytes, _ := base64.RawURLEncoding.DecodeString(connection.config.RootPublicKey)
	peer, err := verifyIdentityEnvelope(identityMessage.data, ed25519.PublicKey(rootBytes), connection.config.RootKeyID, connection.config.OwnerAccountID, connection.config.MachineID, time.Now())
	if err != nil {
		return &terminalConnectError{state: "denied", code: "machine_identity_unverified", message: "The machine key did not match the trusted owner key and expected machine.", cause: err}
	}
	if ws.socket.Get("protocol").String() != websocketSubprotocol {
		return &terminalConnectError{state: "transport-error", code: "subprotocol_mismatch", message: "The browser terminal transport did not select its encrypted protocol."}
	}
	clientTLS := tls.Client(ws, tlsClientConfig(connection.identity, peer, time.Now))
	if err := clientTLS.HandshakeContext(ctx); err != nil {
		return &terminalConnectError{state: "denied", code: "machine_identity_unverified", message: "The machine did not prove the key in its signed identity.", cause: err}
	}
	connection.mu.Lock()
	connection.tls = clientTLS
	connection.mu.Unlock()
	connection.emit("identity-verified", map[string]any{
		"owner_account_id": connection.config.OwnerAccountID,
		"machine_id":       connection.config.MachineID,
		"root_key_id":      connection.config.RootKeyID,
	})
	var (
		inspector          bool
		attachState        terminalAttachState
		checkpoint         []byte
		checkpointSequence uint64
	)
	setupErr := withAttachDeadline(ws, attachSetupTimeout, func() error {
		if err := connection.sendHello(clientTLS); err != nil {
			return &terminalConnectError{state: "transport-error", code: "protocol_error", message: "The machine rejected the encrypted terminal handshake.", cause: err}
		}
		welcome, err := readStructuredFrame(clientTLS)
		if err != nil || welcome.Type != "welcome" || welcome.Version != protocol.ProtocolVersion {
			return &terminalConnectError{state: "transport-error", code: "protocol_error", message: "The machine rejected the encrypted terminal handshake.", cause: err}
		}
		var negotiation struct {
			Capabilities []string `json:"capabilities"`
		}
		if json.Unmarshal(welcome.Payload, &negotiation) != nil {
			return errors.New("invalid capabilities")
		}
		for _, capability := range negotiation.Capabilities {
			if capability == "file-transfer.v1" {
				connection.filesAvailable = true
			}
			if capability == inspectorCapability {
				connection.inspectorReady = true
			}
		}
		if connection.config.Mode == connectionModeInspector {
			inspector = true
			return nil
		}
		requestID, err := randomID("request")
		if err != nil {
			return err
		}
		operationID, err := randomID("operation")
		if err != nil {
			return err
		}
		attachPayload, err := json.Marshal(map[string]any{"action": "attach", "session_id": connection.config.TerminalSessionID, "from_sequence": connection.config.FromSequence, "at_live_boundary": connection.config.AtLiveBoundary})
		if err != nil {
			return err
		}
		attach := protocol.Frame{Type: "request", RequestID: requestID, Version: protocol.ProtocolVersion, OperationID: operationID, Capability: "terminal.v1", DeadlineMS: 10000, Payload: attachPayload}
		if err := writeStructuredFrame(clientTLS, attach); err != nil {
			return &terminalConnectError{state: "transport-error", code: "connection_failed", message: "The terminal attachment could not be requested.", cause: err}
		}
		response, err := readStructuredFrame(clientTLS)
		if err != nil {
			return &terminalConnectError{state: "transport-error", code: "connection_failed", message: "The terminal attachment response could not be read.", cause: err}
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
			return &terminalConnectError{state: "transport-error", code: "protocol_error", message: "The machine could not attach this terminal."}
		}
		if response.Type != "response" || response.RequestID != requestID {
			return &terminalConnectError{state: "transport-error", code: "invalid_attach_response", message: "The machine returned an invalid terminal attachment."}
		}
		attachState, err = parseAttachResponse(response.Payload)
		if err != nil {
			return &terminalConnectError{state: "transport-error", code: "invalid_attach_response", message: "The machine returned an invalid terminal attachment.", cause: err}
		}
		connection.mu.Lock()
		connection.streamID = attachState.StreamID
		connection.attachmentID = attachState.AttachmentID
		connection.inputSequence = attachState.InputSequence
		connection.resizeSequence = 0
		connection.attached = true
		connection.mu.Unlock()
		checkpoint, checkpointSequence, err = readScreenCheckpoint(clientTLS, attachState.StreamID)
		if err != nil {
			return &terminalConnectError{state: "transport-error", code: "invalid_checkpoint", message: "The machine could not restore the terminal screen.", cause: err}
		}
		return nil
	})
	if setupErr != nil {
		// A stall past the deadline fails the read inside setup, which reports a transport error the
		// page shows with Reconnect.
		return setupErr
	}
	if inspector {
		connection.mu.Lock()
		connection.attached = true
		connection.mu.Unlock()
		// inspectorRequest enqueues its frames on connection.writes; without
		// the write loop they would never leave the browser.
		go connection.writeLoop(clientTLS, ws)
		connection.emit("state", map[string]any{"state": "connected"})
		return connection.readTerminalStream(clientTLS)
	}
	connection.keyEvents = make(chan browserbroadcast.Epoch, 4)
	connection.mu.Lock()
	connection.attachState = attachState
	connection.resynced = make(chan uint64, 1)
	connection.mu.Unlock()
	go connection.writeLoop(clientTLS, ws)
	connection.emitAttached(attachState, attachState.InputSequence, checkpoint, checkpointSequence)
	go connection.readBroadcast(ws, peer.Certificate.Claims.QUICPublicKey, attachState.Generation, checkpointSequence)
	return connection.readTerminalStream(clientTLS)
}

// emitAttached tells the page its terminal is attached at a coherent screen. A resync on the same connection sends
// the same event again with a fresh screen; the page resets to it in one paint.
func (connection *browserTerminal) emitAttached(attachState terminalAttachState, inputSequence uint64, checkpoint []byte, checkpointSequence uint64) {
	connection.emit("attached", map[string]any{
		"role":              connection.config.Role,
		"stream_id":         attachState.StreamID,
		"input_sequence":    strconv.FormatUint(inputSequence, 10),
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
}

func readScreenCheckpoint(stream io.Reader, streamID uint32) ([]byte, uint64, error) {
	checkpoint := screenCheckpoint{data: make([]byte, 0, 64<<10)}
	for {
		kind, payload, err := readApplicationFrame(stream)
		if err != nil || kind != appKindBinary {
			return nil, 0, errors.New("missing terminal screen checkpoint")
		}
		frame, err := protocol.DecodeTerminalOutput(payload)
		if err != nil {
			return nil, 0, errors.New("invalid terminal screen checkpoint")
		}
		if done, err := checkpoint.add(frame, streamID); err != nil {
			return nil, 0, err
		} else if done {
			return checkpoint.data, checkpoint.sequence, nil
		}
	}
}

func (connection *browserTerminal) sendHello(writer io.Writer) error {
	requestID, err := randomID("request")
	if err != nil {
		return err
	}
	return writeStructuredFrame(writer, protocol.Frame{Type: "hello", RequestID: requestID, Version: protocol.ProtocolVersion, Payload: helloPayload(connection.config.Mode)})
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
	var resync screenCheckpoint
	for {
		kind, payload, err := readApplicationFrame(stream)
		if err != nil {
			lost := &terminalConnectError{state: "reconnecting", code: "connection_lost", message: "The terminal connection was interrupted. Reconnect to resume.", cause: err}
			// Name why the transport ended when this side ended it (a rejected record, a full queue): those
			// reasons are fixed strings, never terminal content, and otherwise read as a plain drop.
			connection.mu.Lock()
			ws := connection.websocket
			connection.mu.Unlock()
			if ws != nil {
				if cause := ws.terminalError(); cause != nil && !errors.Is(cause, io.EOF) {
					lost.details = map[string]any{"transport_cause": cause.Error()}
				}
			}
			return lost
		}
		if kind == appKindBinary {
			frame, decodeErr := protocol.DecodeTerminalOutput(payload)
			if decodeErr != nil {
				return &terminalConnectError{state: "transport-error", code: "invalid_output", message: "The machine sent invalid terminal output.", cause: decodeErr}
			}
			connection.mu.Lock()
			streamID := connection.streamID
			connection.mu.Unlock()
			if frame.StreamID != streamID {
				return &terminalConnectError{state: "transport-error", code: "wrong_stream", message: "Terminal output did not match this attachment."}
			}
			if frame.Channel == protocol.TerminalScreenCheckpoint {
				done, err := resync.add(frame, streamID)
				if err != nil {
					return &terminalConnectError{state: "transport-error", code: "invalid_checkpoint", message: "The machine could not restore the terminal screen.", cause: err}
				}
				if done {
					connection.finishResync(resync.data, resync.sequence)
					resync = screenCheckpoint{}
				}
				continue
			}
			if frame.Channel == protocol.TerminalBroadcastKey {
				if len(frame.Data) != 49 || frame.Data[0] != 1 {
					return &terminalConnectError{state: "transport-error", code: "invalid_output_key", message: "The machine sent an invalid output key."}
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
		if connection.deliverFileResponse(frame) || connection.deliverInspectorResponse(frame) {
			continue
		}
		connection.mu.Lock()
		screenResponse := frame.RequestID != "" && frame.RequestID == connection.screenRequestID
		connection.mu.Unlock()
		if screenResponse {
			if frame.Type == "response" {
				continue // the screen itself follows as checkpoint frames
			}
			return &terminalConnectError{state: "reconnecting", code: "resync_failed", message: "The terminal fell behind and could not restore its screen. Reconnecting."}
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
			return &terminalConnectError{state: "transport-error", code: "protocol_error", message: "The machine ended the terminal connection."}
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
		if frame.Type == "heartbeat" {
			if strings.HasPrefix(frame.RequestID, "probe_") {
				connection.emit("probe", map[string]any{"request_id": frame.RequestID})
			}
			continue
		}
		connection.emit("frame", map[string]any{"type": frame.Type, "capability": frame.Capability, "payload": json.RawMessage(frame.Payload)})
	}
}

// readBroadcast consumes one ordered edge fanout feed. Output is accepted only
// after the machine sends its epoch key over the independently authenticated
// inner TLS channel; every record must also carry the machine's Ed25519 proof.
// broadcastWorkSlice bounds how long readBroadcast keeps the thread before the JS event loop runs again.
const broadcastWorkSlice = 20 * time.Millisecond

func (connection *browserTerminal) readBroadcast(ws *browserWSConn, machinePublic ed25519.PublicKey, generation, checkpointSequence uint64) {
	keys := make(map[[16]byte]browserbroadcast.Epoch)
	indexes := make(map[[16]byte]uint64)
	keyOrder := make([][16]byte, 0, 4)
	pending := make([][]byte, 0, 16)
	defer func() {
		for _, record := range pending {
			ws.broadcasts.release(record)
		}
	}()
	cursor := checkpointSequence
	// A dropped record (here, or by the edge behind a flood) leaves a gap: instead of failing, ask the machine for a
	// fresh screen and hold output until it arrives; its boundary becomes the cursor.
	resyncing := false
	resync := func() error {
		if !resyncing {
			resyncing = true
			if err := connection.requestScreen(); err != nil {
				return err
			}
		}
		return nil
	}
	process := func() error {
		for len(pending) > 0 && !resyncing {
			hint, err := browserbroadcast.EpochHint(pending[0])
			if err != nil {
				return err
			}
			epoch, ok := keys[hint]
			if !ok {
				return nil
			}
			record, err := browserbroadcast.Open(pending[0], epoch, machinePublic)
			if err != nil || record.SessionID != connection.config.TerminalSessionID || record.Generation != generation || record.Channel != protocol.TerminalStdout && record.Channel != protocol.TerminalStderr {
				return browserbroadcast.ErrInvalidRecord
			}
			previous := indexes[hint]
			if record.Index == 0 || previous != 0 && record.Index <= previous {
				return browserbroadcast.ErrInvalidRecord
			}
			if previous != 0 && record.Index != previous+1 {
				return resync() // records were dropped
			}
			end := record.StartSequence + uint64(len(record.Data))
			if end < record.StartSequence {
				return browserbroadcast.ErrInvalidRecord
			}
			if record.StartSequence > cursor {
				return resync() // output before this record was dropped
			}
			indexes[hint] = record.Index
			ws.broadcasts.release(pending[0])
			pending[0] = nil
			pending = pending[1:]
			if end <= cursor {
				continue
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
	busySince := time.Now()
	for {
		// Go hands the thread back to JS only when every goroutine waits. A flood keeps this loop busy (each record is
		// decrypted and verified), which starved the worker's timers that post events to the page: the page froze ~0.1 MB
		// in (W9). Yield after a slice of work; records arriving meanwhile overflow the queue and resync as designed.
		if time.Since(busySince) > broadcastWorkSlice {
			time.Sleep(time.Millisecond)
			busySince = time.Now()
		}
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
		case record := <-ws.broadcasts.items:
			if len(pending) >= terminalBroadcastQueueDepth {
				ws.broadcasts.release(pending[0]) // the fresh screen covers it
				pending[0] = nil
				pending = pending[1:]
				if err := resync(); err != nil {
					ws.fail(err)
					return
				}
			}
			pending = append(pending, record)
		case <-ws.broadcasts.lagged:
			if err := resync(); err != nil {
				ws.fail(err)
				return
			}
		case boundary := <-connection.resynced:
			// Index continuity restarts after the gap; sequences still reject any reordering.
			cursor, resyncing = boundary, false
			clear(indexes)
		}
		if err := process(); err != nil {
			ws.fail(err)
			return
		}
	}
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
	sequence := connection.inputSequence
	item := terminalWrite{kind: appKindBinary}
	for len(data) > 0 {
		count := min(len(data), terminalInputLimit)
		sequence++
		encoded, err := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: connection.streamID, Sequence: sequence, Data: data[:count]}, nil)
		if err != nil {
			return err
		}
		item.payloads = append(item.payloads, encoded)
		item.bytes += len(encoded) + 5
		data = data[count:]
	}
	if err := connection.writes.enqueue(item); err != nil {
		return err
	}
	connection.inputSequence = sequence
	return nil
}

func (connection *browserTerminal) sendResize(columns, rows uint16) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.config.Role != "owner" && connection.config.Role != "interactive" {
		return errors.New("a viewer does not resize the terminal")
	}
	if !connection.attached || connection.tls == nil || connection.stopping.Load() {
		return net.ErrClosed
	}
	sequence := connection.resizeSequence + 1
	encoded, err := protocol.EncodeTerminalResize(protocol.TerminalResizeFrame{StreamID: connection.streamID, Columns: columns, Rows: rows, Sequence: sequence}, nil)
	if err != nil {
		return err
	}
	if err := connection.writes.enqueue(terminalWrite{kind: appKindBinary, payloads: [][]byte{encoded}, bytes: len(encoded) + 5}); err != nil {
		return err
	}
	connection.resizeSequence = sequence
	return nil
}

// sendActivity sends the focus frame: a viewer never controls, so it sends nothing.
func (connection *browserTerminal) sendActivity() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.config.Role == "viewer" {
		return nil
	}
	if !connection.attached || connection.tls == nil || connection.stopping.Load() {
		return net.ErrClosed
	}
	encoded, err := protocol.EncodeTerminalActivity(protocol.TerminalActivityFrame{StreamID: connection.streamID}, nil)
	if err != nil {
		return err
	}
	return connection.writes.enqueue(terminalWrite{kind: appKindBinary, payloads: [][]byte{encoded}, bytes: len(encoded) + 5})
}

func (connection *browserTerminal) sendAck(nextSequence uint64) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if !connection.attached || connection.tls == nil || connection.stopping.Load() {
		return net.ErrClosed
	}
	return connection.writes.ack(nextSequence)
}

// This writer preserves command order after attach; graceful detach is sent
// during leave. Network operations never hold connection.mu or a JS callback.
func (connection *browserTerminal) writeLoop(writer io.Writer, ws *browserWSConn) {
	defer connection.writes.close()
	fail := func(err error) {
		connection.mu.Lock()
		requestID := connection.cursorRequestID
		connection.cursorRequestID = ""
		connection.attached = false
		connection.mu.Unlock()
		if requestID != "" {
			connection.emit("cursor-error", map[string]any{"request_id": requestID})
		}
		ws.fail(err)
	}
	for {
		select {
		case <-ws.done:
			return
		case item := <-connection.writes.items:
			var err error
			if !connection.stopping.Load() {
				for _, payload := range item.payloads {
					if err = writeApplicationFrame(writer, item.kind, payload); err != nil {
						break
					}
				}
			}
			connection.writes.release(item)
			if err != nil {
				fail(err)
				return
			}
			continue
		case <-connection.writes.ackNotify:
			// Credit goes back as soon as output is consumed, with no coalescing timer: Go timers
			// run on setTimeout in the WASM, which a hidden tab throttles to about once a minute,
			// and late credit filled the host's per-viewer budget and forced a reconnect. Acks
			// that arrive while a write is in progress still collapse into one (one-slot notify).
			if connection.writes.pendingACK() == 0 {
				continue
			}
		}
		if connection.stopping.Load() {
			continue
		}
		if sequence := connection.writes.pendingACK(); sequence != 0 {
			encoded, err := protocol.EncodeTerminalACK(protocol.TerminalACKFrame{StreamID: connection.streamID, NextSequence: sequence}, nil)
			if err == nil {
				err = writeApplicationFrame(writer, appKindBinary, encoded)
			}
			if err != nil {
				fail(err)
				return
			}
			connection.writes.sentACK(sequence)
		}
	}
}

// sendProbe checks the connection when the page becomes visible, focused or online: the host
// answers a heartbeat whose request id starts with probe_ at once, ahead of queued output, and the
// reply arrives as a "probe" event. The caller's deadline starts when it sends, so time spent hidden
// never counts.
func (connection *browserTerminal) sendProbe() (string, error) {
	requestID, err := randomID("probe")
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(protocol.Frame{Type: "heartbeat", RequestID: requestID, Version: protocol.ProtocolVersion})
	if err != nil {
		return "", err
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if !connection.attached || connection.tls == nil || connection.stopping.Load() {
		return "", net.ErrClosed
	}
	if err := connection.writes.enqueue(terminalWrite{kind: appKindStructured, payloads: [][]byte{encoded}, bytes: len(encoded) + 5}); err != nil {
		return "", err
	}
	return requestID, nil
}

func (connection *browserTerminal) sendCursor() (string, error) {
	requestID, err := randomID("request")
	if err != nil {
		return "", err
	}
	operationID, err := randomID("operation")
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
	encoded, err := json.Marshal(frame)
	if err == nil {
		err = frame.Validate()
	}
	if err == nil {
		err = connection.writes.enqueue(terminalWrite{kind: appKindStructured, payloads: [][]byte{encoded}, bytes: len(encoded) + 5})
	}
	if err != nil {
		connection.cursorRequestID = ""
		return "", err
	}
	return requestID, nil
}

// requestScreen asks the machine for a fresh screen on this stream after output was dropped behind a flood. One
// request is pending at a time; its screen covers everything dropped before it was taken.
func (connection *browserTerminal) requestScreen() error {
	requestID, err := randomID("request")
	if err != nil {
		return err
	}
	operationID, err := randomID("operation")
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"action": "screen", "session_id": connection.config.TerminalSessionID})
	if err != nil {
		return err
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.screenRequestID != "" {
		return nil
	}
	if !connection.attached || connection.tls == nil || connection.stopping.Load() {
		return net.ErrClosed
	}
	frame := protocol.Frame{Type: "request", RequestID: requestID, Version: protocol.ProtocolVersion, OperationID: operationID, Capability: "terminal.v1", DeadlineMS: 10000, Payload: payload}
	encoded, err := json.Marshal(frame)
	if err == nil {
		err = frame.Validate()
	}
	if err == nil {
		err = connection.writes.enqueue(terminalWrite{kind: appKindStructured, payloads: [][]byte{encoded}, bytes: len(encoded) + 5})
	}
	if err == nil {
		connection.screenRequestID = requestID
	}
	return err
}

// markLagged makes the shared-output reader resync as if the edge had dropped records; false once the feed is gone.
func (connection *browserTerminal) markLagged() bool {
	connection.mu.Lock()
	ws := connection.websocket
	connection.mu.Unlock()
	if ws == nil {
		return false
	}
	ws.broadcasts.markLagged()
	return true
}

// finishResync paints the fresh screen and moves the shared-output cursor to its boundary.
func (connection *browserTerminal) finishResync(checkpoint []byte, boundary uint64) {
	connection.mu.Lock()
	state, inputSequence := connection.attachState, connection.inputSequence
	connection.screenRequestID = ""
	resynced := connection.resynced
	connection.mu.Unlock()
	connection.emitAttached(state, inputSequence, checkpoint, boundary)
	select {
	case <-resynced:
	default:
	}
	resynced <- boundary
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
	requestID, err := randomID("request")
	if err != nil {
		return
	}
	_ = writeStructuredFrame(writer, protocol.Frame{Type: "detach", RequestID: requestID, Version: protocol.ProtocolVersion, Payload: payload})
}

func (connection *browserTerminal) leave() {
	connection.stopOnce.Do(func() {
		connection.stopping.Store(true)
		connection.closeFileRequests()
		connection.mu.Lock()
		websocket := connection.websocket
		connection.mu.Unlock()
		if websocket != nil {
			_ = websocket.SetWriteDeadline(time.Now().Add(time.Second))
		}
		connection.writes.close()
		connection.emit("state", map[string]any{"state": "left"})
		go func() {
			connection.sendDetach()
			connection.closeTransport()
		}()
	})
}

func (connection *browserTerminal) cleanup() {
	connection.closeFileRequests()
	connection.writes.close()
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
