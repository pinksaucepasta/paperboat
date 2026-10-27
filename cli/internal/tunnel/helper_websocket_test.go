package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

type capturedHelperMessageConnection struct {
	mu     sync.Mutex
	writes [][]byte
}

func TestHelperResponseTerminalModesRestoresOnlyRecordedModes(t *testing.T) {
	frame := helperFrame{Payload: json.RawMessage(`{"result":{"session":{"snapshot":{"terminal_modes":{"alternate_screen":true,"mouse_sgr":true,"bracketed_paste":true}}}}}`)}
	if got, want := helperResponseTerminalModes(frame), "\x1b[?1049h\x1b[?1006h\x1b[?2004h"; got != want {
		t.Fatalf("modes = %q, want %q", got, want)
	}
}

func (c *capturedHelperMessageConnection) ReadMessage(ctx context.Context) (helperMessageType, []byte, error) {
	<-ctx.Done()
	return 0, nil, ctx.Err()
}
func (c *capturedHelperMessageConnection) WriteMessage(_ context.Context, _ helperMessageType, data []byte) error {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), data...))
	c.mu.Unlock()
	return nil
}
func (*capturedHelperMessageConnection) Close() error { return nil }

type earlyOutputMessageConnection struct {
	reads chan struct {
		kind helperMessageType
		data []byte
	}
	closed        chan struct{}
	once          sync.Once
	attachBinary  [][]byte
	healthPayload json.RawMessage
}

func newEarlyOutputMessageConnection() *earlyOutputMessageConnection {
	return &earlyOutputMessageConnection{reads: make(chan struct {
		kind helperMessageType
		data []byte
	}, 8), closed: make(chan struct{})}
}

func (c *earlyOutputMessageConnection) ReadMessage(ctx context.Context) (helperMessageType, []byte, error) {
	select {
	case message := <-c.reads:
		return message.kind, message.data, nil
	case <-c.closed:
		return 0, nil, io.EOF
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (c *earlyOutputMessageConnection) WriteMessage(_ context.Context, kind helperMessageType, data []byte) error {
	if kind != helperStructuredMessage {
		return nil
	}
	frame, err := decodeHelperFrame(data)
	if err != nil || frame.Type != "request" {
		return err
	}
	var payload map[string]any
	_ = json.Unmarshal(frame.Payload, &payload)
	response := helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion}
	if frame.Capability == "health.v1" {
		response.Payload = c.healthPayload
		if len(response.Payload) == 0 {
			response.Payload = json.RawMessage(`{"result":{"version":"2026.08.27.65"}}`)
		}
		encoded, _ := json.Marshal(response)
		c.reads <- struct {
			kind helperMessageType
			data []byte
		}{helperStructuredMessage, encoded}
		return nil
	}
	switch payload["action"] {
	case "snapshot":
		response.Type = "error"
		response.Payload = json.RawMessage(`{"code":"not_found_or_forbidden","message":"missing","retryable":false}`)
	case "create":
		response.Payload = json.RawMessage(`{"result":{"generation":1}}`)
	case "attach":
		frames := c.attachBinary
		if len(frames) == 0 {
			binaryFrame, _ := protocol.EncodeTerminalOutput(protocol.TerminalOutputFrame{Channel: protocol.TerminalStdout, StreamID: 7, Data: []byte("early")}, nil)
			frames = [][]byte{binaryFrame}
		}
		for _, binaryFrame := range frames {
			c.reads <- struct {
				kind helperMessageType
				data []byte
			}{helperBinaryMessage, binaryFrame}
		}
		response.Payload = json.RawMessage(`{"result":{"stream_id":7,"attachment_id":"att_early","session":{"snapshot":{"generation":1}}}}`)
	}
	encoded, _ := json.Marshal(response)
	c.reads <- struct {
		kind helperMessageType
		data []byte
	}{helperStructuredMessage, encoded}
	return nil
}

func TestDebugTerminalCapturesExactRemoteRuntimeVersion(t *testing.T) {
	message := newEarlyOutputMessageConnection()
	connection, err := newInitializedHelperTerminalConn(context.Background(), message, &resolver.TerminalTarget{SessionID: "ses_debug", CWD: "/workspace", Debug: true}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := TerminalRuntimeVersion(connection); got != "2026.08.27.65" {
		t.Fatalf("runtime version = %q", got)
	}
	_ = message.Close()
	connection.finish(0, nil)
}

func TestDebugTerminalRejectsMissingRemoteRuntimeVersion(t *testing.T) {
	message := newEarlyOutputMessageConnection()
	message.healthPayload = json.RawMessage(`{"result":{}}`)
	if _, err := newInitializedHelperTerminalConn(context.Background(), message, &resolver.TerminalTarget{SessionID: "ses_debug", CWD: "/workspace", Debug: true}, 4); err == nil || !strings.Contains(err.Error(), "invalid version") {
		t.Fatalf("error = %v", err)
	}
	_ = message.Close()
}

func TestHelperResponseRuntimeVersionRejectsMalformedResponse(t *testing.T) {
	if got := helperResponseRuntimeVersion(helperFrame{Type: "response", Payload: json.RawMessage(`{"result":{"version":" 2026.08.27.65 "}}`)}); got != "2026.08.27.65" {
		t.Fatalf("runtime version = %q", got)
	}
	if got := helperResponseRuntimeVersion(helperFrame{Type: "error", Payload: json.RawMessage(`{"result":{"version":"2026.08.27.65"}}`)}); got != "" {
		t.Fatalf("error response exposed runtime version %q", got)
	}
	if got := helperResponseRuntimeVersion(helperFrame{Type: "response", Payload: json.RawMessage(`{"result":`)}); got != "" {
		t.Fatalf("malformed response exposed runtime version %q", got)
	}
}

func (c *earlyOutputMessageConnection) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func TestNativeTerminalBuffersOutputThatPrecedesAttachResponse(t *testing.T) {
	message := newEarlyOutputMessageConnection()
	target := &resolver.TerminalTarget{SessionID: "ses_early", CWD: "/workspace"}
	connection := newHelperTerminalConn(message, target, 4)
	if err := connection.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len("early"))
	if _, err := io.ReadFull(connection, buffer); err != nil || string(buffer) != "early" {
		t.Fatalf("output=%q err=%v", buffer, err)
	}
	_ = message.Close()
	connection.finish(0, nil)
}

func TestPreAttachCompressedOutputIsBoundedByDecodedBytes(t *testing.T) {
	message := newEarlyOutputMessageConnection()
	data := bytes.Repeat([]byte("x"), protocol.MaxTerminalOutputBytes)
	for sequence := uint64(0); sequence < uint64(5*len(data)); sequence += uint64(len(data)) {
		wire, err := protocol.EncodeTerminalOutputAdaptive(protocol.TerminalOutputFrame{Channel: protocol.TerminalStdout, StreamID: 7, StartSequence: sequence, Data: data}, nil)
		if err != nil || wire[2] != protocol.TerminalOutputZstd {
			t.Fatalf("encoding=%d err=%v", wire[2], err)
		}
		message.attachBinary = append(message.attachBinary, wire)
	}
	connection := newHelperTerminalConn(message, &resolver.TerminalTarget{SessionID: "ses_bound", CWD: "/workspace"}, 4)
	err := connection.initialize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "excessive output") {
		t.Fatalf("err=%v", err)
	}
	if len(connection.initial) != 0 {
		t.Fatalf("decoded output admitted: %d", len(connection.initial))
	}
}

func TestHelperTerminalLargeInputUses32KiBRecords(t *testing.T) {
	message := &capturedHelperMessageConnection{}
	target := &resolver.TerminalTarget{SessionID: "ses_1"}
	connection := newHelperTerminalConn(message, target, 1)
	connection.streamID = 7
	go connection.writeLoop()
	payload := make([]byte, 70<<10)
	if written, err := connection.Write(payload); err != nil || written != len(payload) {
		t.Fatalf("written=%d err=%v", written, err)
	}
	connection.finish(0, nil)
	message.mu.Lock()
	defer message.mu.Unlock()
	if len(message.writes) != 3 {
		t.Fatalf("records=%d", len(message.writes))
	}
	for index, record := range message.writes {
		body := helperTestInputBody(t, record)
		if len(body) > 32<<10 || index < 2 && len(body) != 32<<10 {
			t.Fatalf("record %d body bytes=%d", index, len(body))
		}
	}
}

func TestHelperTerminalInputPrecedesQueuedControlTraffic(t *testing.T) {
	connection := newHelperTerminalConn(&capturedHelperMessageConnection{}, &resolver.TerminalTarget{}, 1)
	connection.controlWrites <- helperWrite{data: []byte("control")}
	connection.ackWrites <- helperWrite{data: []byte("ack")}
	connection.inputWrites <- helperWrite{data: []byte("input")}
	write, ok := connection.nextWrite()
	if !ok || string(write.data) != "input" {
		t.Fatalf("first write=%q ok=%v", write.data, ok)
	}
}

func TestCanonicalHelperTerminalFramingIOResizeAndExit(t *testing.T) {
	requests := make(chan helperFrame, 12)
	inputs := make(chan []byte, 1)
	acks := make(chan uint64, 1)
	resizes := make(chan [2]uint16, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime" || r.Header.Get("Authorization") != "Bearer helper-token" {
			t.Errorf("request path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{helperWebSocketSubprotocol}, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		for {
			messageType, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			if messageType == websocket.MessageBinary {
				switch data[0] {
				case 1:
					if len(data) < 14 || binary.BigEndian.Uint32(data[1:5]) != 7 || binary.BigEndian.Uint64(data[5:13]) != 1 {
						t.Errorf("invalid input frame: %x", data)
						return
					}
					inputs <- append([]byte(nil), data[13:]...)
				case 3:
					acks <- binary.BigEndian.Uint64(data[5:13])
				case 4:
					resizes <- [2]uint16{binary.BigEndian.Uint16(data[5:7]), binary.BigEndian.Uint16(data[7:9])}
					writeHelperTestFrame(t, ws, helperFrame{Type: "event", RequestID: "stream", Version: helperProtocolVersion, Capability: "terminal.v1", Payload: json.RawMessage(`{"event":"terminal_stream_end","session_id":"terminal_00000000-0000-4000-8000-000000000001","state":"exited","final_sequence":5,"exit":{"code":7}}`)})
				default:
					t.Errorf("invalid binary frame: %x", data)
				}
				continue
			}
			if messageType != websocket.MessageText {
				t.Errorf("client message type=%d", messageType)
				return
			}
			frame, err := decodeHelperFrame(data)
			if err != nil {
				t.Error(err)
				return
			}
			requests <- frame
			switch frame.Type {
			case "hello":
				writeHelperTestFrame(t, ws, helperFrame{Type: "welcome", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"version":"1.0","capabilities":["health.v1","terminal.v1"]}`)})
			case "ack", "detach":
				writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"result":{},"replay":false}`)})
			case "request":
				var payload map[string]any
				_ = json.Unmarshal(frame.Payload, &payload)
				switch payload["action"] {
				case "snapshot":
					writeHelperTestFrame(t, ws, helperFrame{Type: "error", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"code":"not_found_or_forbidden","message":"operation failed","retryable":false}`)})
				case "create":
					writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"result":{"id":"terminal_00000000-0000-4000-8000-000000000001","generation":1},"replay":false}`)})
				case "attach":
					writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"result":{"stream_id":7,"attachment_id":"att_1","session":{"snapshot":{"generation":1}}},"replay":false}`)})
					writeHelperTestBinary(t, ws, 7, 0, []byte("h"))
					writeHelperTestBinary(t, ws, 7, 1, []byte("el"))
					writeHelperTestBinary(t, ws, 7, 3, []byte("lo"))
				}
			}
		}
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
	u.Path = "/v1/runtime"
	target := &resolver.TerminalTarget{Protocol: "paperboat.terminal.v1", WSSEndpoint: u.String(), Auth: resolver.AuthTarget{Method: "bearer", Token: "helper-token"}, SessionID: "terminal_00000000-0000-4000-8000-000000000001", CWD: "/workspace", Cols: 100, Rows: 30, Env: map[string]string{"TERM": "xterm-ghostty", "COLORTERM": "truecolor"}}
	conn, err := dialTestHelperTerminal(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	buffer := make([]byte, 5)
	if _, err := io.ReadFull(conn, buffer); err != nil || string(buffer) != "hello" {
		t.Fatalf("output=%q err=%v", buffer, err)
	}
	if _, err := conn.Write([]byte("echo hi\n")); err != nil {
		t.Fatal(err)
	}
	if input := <-inputs; string(input) != "echo hi\n" {
		t.Fatalf("input=%q", input)
	}
	if err := conn.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	if code, err := conn.Wait(); err != nil || code != 7 {
		t.Fatalf("Wait()=%d,%v", code, err)
	}

	var sawCreate, sawAttach bool
	for len(requests) > 0 {
		frame := <-requests
		var payload map[string]any
		_ = json.Unmarshal(frame.Payload, &payload)
		switch {
		case payload["action"] == "create":
			environment, _ := payload["environment"].(map[string]any)
			sawCreate = payload["session_id"] == "terminal_00000000-0000-4000-8000-000000000001" && payload["name"] == "terminal_00000000-0000-4000-8000-000000000001" && payload["columns"] == float64(100) && payload["rows"] == float64(30) && environment["TERM"] == "xterm-ghostty" && environment["COLORTERM"] == "truecolor"
		case payload["action"] == "attach":
			sawAttach = payload["session_id"] == "terminal_00000000-0000-4000-8000-000000000001"
		}
	}
	if ack := <-acks; ack != 5 {
		t.Fatalf("ack=%d", ack)
	}
	if resize := <-resizes; resize != [2]uint16{120, 40} {
		t.Fatalf("resize=%v", resize)
	}
	if !sawCreate || !sawAttach {
		t.Fatalf("create=%v attach=%v", sawCreate, sawAttach)
	}
}

func TestHelperBinaryOutputRetainsOwnedWebSocketPayload(t *testing.T) {
	conn := &helperTerminalConn{streamID: 7}
	message, err := protocol.EncodeTerminalOutput(protocol.TerminalOutputFrame{Channel: protocol.TerminalStdout, StreamID: 7, StartSequence: 11, Data: []byte("abc")}, nil)
	if err != nil {
		t.Fatal(err)
	}

	output, err := conn.decodeHelperBinary(message)
	if err != nil || output.endSequence != 14 || string(output.data) != "abc" {
		t.Fatalf("output=%#v err=%v", output, err)
	}
	message[19] = 'z'
	if string(output.data) != "zbc" {
		t.Fatal("binary output payload was copied")
	}
	stats := conn.TerminalCompressionTelemetry()
	if stats.RawFrames != 1 || stats.ZstdFrames != 0 || stats.DecodedBytes != 3 || stats.EncodedBytes != 3 || stats.DecodeFailures != 0 {
		t.Fatalf("compression telemetry=%+v", stats)
	}
}

func TestCorruptCompressedOutputIsNeverQueuedOrAcknowledged(t *testing.T) {
	message := newEarlyOutputMessageConnection()
	conn := newHelperTerminalConn(message, &resolver.TerminalTarget{SessionID: "ses_corrupt"}, 1)
	conn.streamID = 7
	wire, err := protocol.EncodeTerminalOutputAdaptive(protocol.TerminalOutputFrame{Channel: protocol.TerminalStdout, StreamID: 7, StartSequence: 20, Data: bytes.Repeat([]byte("agent output\r\n"), 200)}, nil)
	if err != nil || wire[2] != protocol.TerminalOutputZstd {
		t.Fatalf("wire encoding=%d err=%v", wire[2], err)
	}
	wire[len(wire)-1] ^= 0xff
	message.reads <- struct {
		kind helperMessageType
		data []byte
	}{helperBinaryMessage, wire}
	go conn.readLoop()
	select {
	case <-conn.done:
	case <-time.After(time.Second):
		t.Fatal("corrupt frame did not close connection")
	}
	if got := conn.ackLatest.Load(); got != 0 {
		t.Fatalf("ack advanced to %d", got)
	}
	if output, ok := <-conn.out; ok {
		t.Fatalf("corrupt output queued: %#v", output)
	}
}

func TestCanonicalHelperExistingSessionDoesNotInjectTerminalInput(t *testing.T) {
	resizeColumns := make(chan int, 1)
	redrawInputs := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{helperWebSocketSubprotocol}, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		for {
			messageType, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			if messageType == websocket.MessageBinary {
				switch data[0] {
				case 1:
					redrawInputs <- helperTestInputBody(t, data)
				case 4:
					resizeColumns <- int(binary.BigEndian.Uint16(data[5:7]))
				}
				continue
			}
			frame, err := decodeHelperFrame(data)
			if err != nil {
				t.Error(err)
				return
			}
			switch frame.Type {
			case "hello":
				writeHelperTestFrame(t, ws, helperFrame{Type: "welcome", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"version":"1.0","capabilities":["health.v1","terminal.v1"]}`)})
			case "detach":
				return
			case "request":
				var payload map[string]any
				_ = json.Unmarshal(frame.Payload, &payload)
				switch payload["action"] {
				case "create":
					writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"result":{"state":"running","generation":1,"earliest_sequence":100,"latest_sequence":200,"existing":true},"replay":false}`)})
				case "attach":
					writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"result":{"stream_id":8,"attachment_id":"att_live","session":{"snapshot":{"generation":1}}},"replay":false}`)})
				}
			}
		}
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
	u.Path = "/v1/runtime"
	target := &resolver.TerminalTarget{Protocol: "paperboat.terminal.v1", WSSEndpoint: u.String(), Auth: resolver.AuthTarget{Method: "bearer", Token: "helper-token"}, SessionID: "ses_gap", CWD: "/workspace"}
	conn, err := dialTestHelperTerminal(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	helperConn, ok := conn.(*helperTerminalConn)
	if !ok {
		t.Fatalf("connection type %T", conn)
	}
	for _, output := range helperConn.initial {
		if strings.Contains(string(output.data), "\x1b[?") {
			t.Fatalf("connection synthesized terminal modes: %q", output.data)
		}
	}
	if err := conn.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	if columns := <-resizeColumns; columns != 120 {
		t.Fatalf("resize columns=%d", columns)
	}
	select {
	case input := <-redrawInputs:
		t.Fatalf("connection injected terminal input=%q", input)
	default:
	}
}

func TestCanonicalHelperRestartIsLimitedToInitialAttach(t *testing.T) {
	for _, test := range []struct {
		name         string
		restart      bool
		wantRestart  bool
		wantExitCode int
		generation   int
		attachmentID string
	}{
		{name: "initial attach", restart: true, wantRestart: true, wantExitCode: 11, generation: 4, attachmentID: "att_initial"},
		{name: "transport reconnect", restart: false, wantRestart: false, wantExitCode: 23, generation: 3, attachmentID: "att_reconnect"},
	} {
		t.Run(test.name, func(t *testing.T) {
			actions := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{helperWebSocketSubprotocol}, CompressionMode: websocket.CompressionDisabled})
				if err != nil {
					t.Error(err)
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "")
				for {
					messageType, data, err := ws.Read(context.Background())
					if err != nil {
						return
					}
					if messageType != websocket.MessageText {
						t.Errorf("client message type=%d", messageType)
						return
					}
					frame, err := decodeHelperFrame(data)
					if err != nil {
						t.Error(err)
						return
					}
					if frame.Type == "hello" {
						writeHelperTestFrame(t, ws, helperFrame{Type: "welcome", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"version":"1.0","capabilities":["health.v1","terminal.v1"]}`)})
						continue
					}
					if frame.Type != "request" {
						continue
					}
					var payload map[string]any
					if err := json.Unmarshal(frame.Payload, &payload); err != nil {
						t.Error(err)
						return
					}
					action, _ := payload["action"].(string)
					actions <- action
					switch action {
					case "create":
						writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"result":{"state":"exited","generation":3,"existing":true},"replay":false}`)})
					case "restart":
						writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"result":{"state":"running","generation":4},"replay":false}`)})
					case "attach":
						response := fmt.Sprintf(`{"result":{"stream_id":9,"attachment_id":%q,"session":{"snapshot":{"generation":%d}}},"replay":false}`, test.attachmentID, test.generation)
						writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(response)})
						event := fmt.Sprintf(`{"event":"terminal_stream_end","session_id":"ses_retained","state":"exited","final_sequence":17,"exit":{"code":%d}}`, test.wantExitCode)
						writeHelperTestFrame(t, ws, helperFrame{Type: "event", RequestID: "stream", Version: helperProtocolVersion, Capability: "terminal.v1", Payload: json.RawMessage(event)})
					}
				}
			}))
			defer server.Close()

			u, _ := url.Parse(server.URL)
			u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
			u.Path = "/v1/runtime"
			target := &resolver.TerminalTarget{Protocol: "paperboat.terminal.v1", WSSEndpoint: u.String(), Auth: resolver.AuthTarget{Method: "bearer", Token: "helper-token"}, SessionID: "ses_retained", RestartIfNotRunning: test.restart, AfterSequence: 9}
			conn, err := dialTestHelperTerminal(context.Background(), target)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if code, err := conn.Wait(); err != nil || code != test.wantExitCode {
				t.Fatalf("Wait()=%d,%v", code, err)
			}

			close(actions)
			var got []string
			for action := range actions {
				got = append(got, action)
			}
			want := []string{"create"}
			if test.wantRestart {
				want = append(want, "restart")
			}
			want = append(want, "attach")
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("actions=%v want=%v", got, want)
			}
		})
	}
}

func TestCanonicalHelperStaleReconnectCursorReportsReplayGap(t *testing.T) {
	attachFrom := make(chan uint64, 2)
	attachLive := make(chan bool, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{helperWebSocketSubprotocol}, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		for {
			messageType, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			if messageType != websocket.MessageText {
				return
			}
			frame, err := decodeHelperFrame(data)
			if err != nil {
				t.Error(err)
				return
			}
			if frame.Type == "hello" {
				writeHelperTestFrame(t, ws, helperFrame{Type: "welcome", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"version":"1.0","capabilities":["health.v1","terminal.v1"]}`)})
				continue
			}
			if frame.Type != "request" {
				continue
			}
			var payload struct {
				Action       string `json:"action"`
				FromSequence uint64 `json:"from_sequence"`
				AtLive       bool   `json:"at_live_boundary"`
			}
			_ = json.Unmarshal(frame.Payload, &payload)
			switch payload.Action {
			case "create":
				writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"result":{"state":"running","generation":1,"earliest_sequence":10,"latest_sequence":18,"existing":true},"replay":false}`)})
			case "attach":
				attachFrom <- payload.FromSequence
				attachLive <- payload.AtLive
				if payload.FromSequence < 10 {
					writeHelperTestFrame(t, ws, helperFrame{Type: "error", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"code":"replay_gap","message":"operation failed","retryable":false,"details":{"requested_sequence":2,"earliest_sequence":10,"latest_sequence":18}}`)})
					continue
				}
				writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"result":{"stream_id":10,"attachment_id":"att_recovered","session":{"snapshot":{"generation":1}}},"replay":true}`)})
				writeHelperTestBinary(t, ws, 10, 18, []byte("fresh\n"))
				writeHelperTestFrame(t, ws, helperFrame{Type: "event", RequestID: "stream", Version: helperProtocolVersion, Capability: "terminal.v1", Payload: json.RawMessage(`{"event":"terminal_stream_end","session_id":"ses_gap","state":"exited","final_sequence":24,"exit":{"code":0}}`)})
			}
		}
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
	u.Path = "/v1/runtime"
	var cursor atomic.Int64
	target := &resolver.TerminalTarget{Protocol: "paperboat.terminal.v1", WSSEndpoint: u.String(), Auth: resolver.AuthTarget{Method: "bearer", Token: "helper-token"}, SessionID: "ses_gap", AfterSequence: 2, SequenceSink: func(value int) { cursor.Store(int64(value)) }}
	conn, err := dialTestHelperTerminal(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	output, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "Earlier terminal output is unavailable") || !strings.HasSuffix(string(output), "fresh\n") {
		t.Fatalf("output=%q", output)
	}
	if boundary := <-attachFrom; boundary != 2 {
		t.Fatalf("initial attach boundary=%d", boundary)
	}
	if boundary := <-attachFrom; boundary != 18 {
		t.Fatalf("recovered attach boundary=%d", boundary)
	}
	if first, second := <-attachLive, <-attachLive; first || !second {
		t.Fatalf("reconnect live boundaries: %v, %v", first, second)
	}
	if got := cursor.Load(); got != 24 {
		t.Fatalf("cursor=%d want 24", got)
	}
}

func TestCanonicalHelperTerminalErrorCarriesValidatedReferenceWithoutRemoteText(t *testing.T) {
	const (
		reference   = "support_01234567-89ab-4def-8123-456789abcdef"
		privateText = "PRIVATE_TERMINAL_ERROR /private/runtime/state"
	)
	for _, tc := range []struct {
		name, code, remoteReference, wantReference, wantLocalCode, wantMessage string
	}{
		{"captured terminal failure", "terminal_create_failed", reference, reference, "", "remote terminal operation failed"},
		{"pure denial", "not_found_or_forbidden", reference, "", "", "terminal resource was not found or is not permitted"},
		{"unknown remote code", "PRIVATE_REMOTE_CODE", reference, "", "", "remote terminal operation failed"},
		{"invalid reference", "terminal_create_failed", "support_private-value", "", "", "remote terminal operation failed"},
		{"environment denial", "environment_unavailable", reference, "", "environment_unavailable", "Managed environment variables could not be delivered on the target. Check ENV delivery and retry."},
		{"session limit", "resource_limit", reference, "", "resource_limit", "This machine has reached its ceiling of 256 open terminals. Close some with `pb session close`, then retry."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{helperWebSocketSubprotocol}})
				if err != nil {
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "")
				for {
					_, data, err := ws.Read(r.Context())
					if err != nil {
						return
					}
					frame, err := decodeHelperFrame(data)
					if err != nil {
						t.Errorf("decode request: %v", err)
						return
					}
					switch frame.Type {
					case "hello":
						writeHelperTestFrame(t, ws, helperFrame{Type: "welcome", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"version":"1.0","capabilities":["health.v1","terminal.v1"]}`)})
					case "request":
						var request struct {
							Action string `json:"action"`
						}
						if err := json.Unmarshal(frame.Payload, &request); err != nil || request.Action != "create" {
							t.Errorf("initial request action=%q err=%v", request.Action, err)
							return
						}
						errorPayload, err := json.Marshal(map[string]any{
							"code": tc.code, "message": privateText, "retryable": true,
							"details": map[string]string{"support_reference": tc.remoteReference},
						})
						if err != nil {
							t.Errorf("marshal remote error: %v", err)
							return
						}
						writeHelperTestFrame(t, ws, helperFrame{Type: "error", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: errorPayload})
						return
					default:
						t.Errorf("unexpected request type %q", frame.Type)
						return
					}
				}
			}))
			defer server.Close()

			u, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
			u.Path = "/v1/runtime"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			target := &resolver.TerminalTarget{Protocol: "paperboat.terminal.v1", WSSEndpoint: u.String(), Auth: resolver.AuthTarget{Method: "bearer", Token: "helper-token"}, SessionID: "terminal_00000000-0000-4000-8000-000000000001"}
			_, err = dialTestHelperTerminal(ctx, target)
			if err == nil {
				t.Fatal("remote create failure returned a terminal connection")
			}
			var referenced interface{ SupportReference() string }
			gotReference := ""
			if errors.As(err, &referenced) {
				gotReference = referenced.SupportReference()
			}
			if gotReference != tc.wantReference {
				t.Fatalf("terminal error reference=%q, want %q; error=%T %v", gotReference, tc.wantReference, err, err)
			}
			var coded interface{ LocalAPICode() string }
			gotLocalCode := ""
			if errors.As(err, &coded) {
				gotLocalCode = coded.LocalAPICode()
			}
			if gotLocalCode != tc.wantLocalCode {
				t.Fatalf("terminal local API code=%q, want %q; error=%T %v", gotLocalCode, tc.wantLocalCode, err, err)
			}
			if strings.Contains(err.Error(), privateText) || strings.Contains(err.Error(), "/private/runtime/state") || strings.Contains(err.Error(), tc.code) {
				t.Fatalf("terminal error exposed remote prose or code: %q", err)
			}
			if err.Error() != tc.wantMessage {
				t.Fatalf("terminal error=%q, want %q", err, tc.wantMessage)
			}
		})
	}
}

const helperWebSocketSubprotocol = "paperboat.terminal.v1"

func dialTestHelperTerminal(ctx context.Context, target *resolver.TerminalTarget) (Conn, error) {
	connection, _, err := websocket.Dial(ctx, target.WSSEndpoint, &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": []string{"Bearer " + target.Auth.Token}},
		Subprotocols:    []string{helperWebSocketSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	message := &helperWebSocketConnection{ws: connection}
	if _, err := helperHandshake(ctx, message); err != nil {
		_ = message.Close()
		return nil, err
	}
	terminal, err := newInitializedHelperTerminalConn(ctx, message, target, terminalOutputQueueChunks)
	if err != nil {
		_ = message.Close()
		return nil, err
	}
	return terminal, nil
}

func writeHelperTestFrame(t *testing.T, ws *websocket.Conn, frame helperFrame) {
	t.Helper()
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
}

func writeHelperTestBinary(t *testing.T, ws *websocket.Conn, streamID uint32, sequence uint64, body []byte) {
	t.Helper()
	data, err := protocol.EncodeTerminalOutputAdaptive(protocol.TerminalOutputFrame{Channel: protocol.TerminalStdout, StreamID: streamID, StartSequence: sequence, Data: body}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(context.Background(), websocket.MessageBinary, data); err != nil {
		t.Fatal(err)
	}
}

func helperTestInputBody(t *testing.T, data []byte) []byte {
	t.Helper()
	if len(data) < 14 || data[0] != 1 {
		t.Fatalf("invalid terminal input frame: %x", data)
	}
	return append([]byte(nil), data[13:]...)
}

// A newer host may add events a terminal client does not use (participants for browser viewers);
// the client ignores them instead of ending the terminal with an invalid-event error.
func TestHelperTerminalIgnoresEventsItDoesNotUse(t *testing.T) {
	conn := &helperTerminalConn{streamID: 7}
	finished := conn.handleEvent(helperFrame{Type: "event", Payload: json.RawMessage(`{"event":"participants","session_id":"ses_1","participants":[]}`)})
	if finished {
		t.Fatal("an unknown event ended the terminal")
	}
}

// Every keyed input gets a terminal_input_result from the host. They used to go into a 256-slot
// channel nothing read, and the 257th closed the connection: a native terminal reconnected about
// every 256 keystrokes. Results now only complete the input queue.
func TestManyInputResultsDoNotEndTheTerminal(t *testing.T) {
	target := &resolver.TerminalTarget{SessionID: "ses_1", InputQueue: resolver.NewTerminalInputQueue(512)}
	connection := newHelperTerminalConn(&capturedHelperMessageConnection{}, target, 1)
	connection.streamID = 7
	for sequence := 1; sequence <= 1000; sequence++ {
		if _, err := target.InputQueue.Enqueue([]byte("x")); err != nil && sequence <= 512 {
			t.Fatal(err)
		}
		payload := fmt.Sprintf(`{"event":"terminal_input_result","stream_id":7,"sequence":%d,"status":"accepted"}`, sequence)
		if connection.handleEvent(helperFrame{Type: "event", Payload: json.RawMessage(payload)}) {
			t.Fatalf("input result %d ended the terminal: %v", sequence, connection.terminalError())
		}
	}
	select {
	case <-connection.done:
		t.Fatalf("terminal ended: %v", connection.terminalError())
	default:
	}
}

// A drop while typing must not lose or repeat a keystroke, nor stop later input: the next
// attachment sends the host's last sequence and everything after it again, in order and before
// new input, and reports what the host had already passed as decided without its answer.
func TestReattachResendsUnansweredInputExactlyOnce(t *testing.T) {
	target := &resolver.TerminalTarget{SessionID: "ses_1", InputQueue: resolver.NewTerminalInputQueue(8)}
	for _, key := range []string{"a", "b", "c"} {
		if _, err := target.InputQueue.Enqueue([]byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	message := &capturedHelperMessageConnection{}
	connection := newHelperTerminalConn(message, target, 1)
	attached := helperFrame{Payload: json.RawMessage(`{"result":{"stream_id":7,"attachment_id":"att_1","input_sequence":2,"session":{"snapshot":{"generation":1}}}}`)}
	if err := connection.finishAttachment(attached, true, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte("d")); err != nil {
		t.Fatalf("input after a reattach: %v", err)
	}
	connection.finish(0, nil)
	message.mu.Lock()
	defer message.mu.Unlock()
	var sent []string
	for _, record := range message.writes {
		if record[0] == 1 {
			sent = append(sent, fmt.Sprintf("%d:%s", binary.BigEndian.Uint64(record[5:13]), helperTestInputBody(t, record)))
		}
	}
	if strings.Join(sent, " ") != "2:b 3:c 4:d" {
		t.Fatalf("sent %v, want the host's last sequence, then the rest, then new input", sent)
	}
	// The keystroke the host had passed is settled; the rest stay queued until the host answers.
	decided, resend, _ := target.InputQueue.Reconcile(2)
	var pending []uint64
	for _, item := range resend {
		pending = append(pending, item.Sequence)
	}
	if len(decided) != 0 || fmt.Sprint(pending) != "[2 3 4]" {
		t.Fatalf("decided %d, still queued %v; want 0 and [2 3 4]", len(decided), pending)
	}
}

type silentThenMessageConnection struct {
	capturedHelperMessageConnection
	heard chan struct{}
}

func (c *silentThenMessageConnection) ReadMessage(ctx context.Context) (helperMessageType, []byte, error) {
	select {
	case <-c.heard:
		return helperStructuredMessage, []byte(`{"type":"heartbeat","request_id":"heartbeat","version":1}`), nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

// F3-15: a host that goes silent (a black hole sends no reset) is reported as stalled without ending the link; the
// next message reports it heard again.
func TestHelperTerminalSilentHostIsStalledNotLost(t *testing.T) {
	previous := helperStallAfter
	helperStallAfter = 50 * time.Millisecond
	defer func() { helperStallAfter = previous }()
	message := &silentThenMessageConnection{heard: make(chan struct{})}
	connection := newHelperTerminalConn(message, &resolver.TerminalTarget{SessionID: "ses_silent"}, 1)
	states := make(chan bool, 4)
	connection.ObserveLinkState(func(stalled bool) { states <- stalled })
	go connection.readLoop()
	defer connection.finish(0, nil)
	select {
	case stalled := <-states:
		if !stalled {
			t.Fatal("reported heard before any silence")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a silent host was not reported")
	}
	select {
	case <-connection.done:
		t.Fatal("a stall ended the link")
	default:
	}
	message.heard <- struct{}{}
	if stalled := <-states; stalled {
		t.Fatal("hearing the host did not report it")
	}
}

// A host that does not know our input numbering is a new session or shell: unanswered keys could
// type into the wrong shell, so (as EternalTerminal refuses such a recovery) nothing is resent and
// the client is told once that its last keys may not have arrived.
func TestReattachToHostWithoutOurNumberingResendsNothingAndSaysSo(t *testing.T) {
	var notices [][]string
	target := &resolver.TerminalTarget{SessionID: "ses_1", InputQueue: resolver.NewTerminalInputQueue(8),
		LaunchWarningSink: func(codes []string) { notices = append(notices, codes) }}
	for _, key := range []string{"a", "b", "c", "d"} {
		if _, err := target.InputQueue.Enqueue([]byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	target.InputQueue.Complete(1) // the old shell answered a and b; c and d are unanswered
	target.InputQueue.Complete(2)
	message := &capturedHelperMessageConnection{}
	connection := newHelperTerminalConn(message, target, 1)
	attached := helperFrame{Payload: json.RawMessage(`{"result":{"stream_id":7,"attachment_id":"att_2","input_sequence":0,"session":{"snapshot":{"generation":1}}}}`)}
	if err := connection.finishAttachment(attached, true, 0, 0); err != nil {
		t.Fatal(err)
	}
	connection.finish(0, nil)
	message.mu.Lock()
	defer message.mu.Unlock()
	for _, record := range message.writes {
		if record[0] == 1 {
			t.Fatalf("resent input %q to a host that does not know it", helperTestInputBody(t, record))
		}
	}
	if fmt.Sprint(notices) != "[["+resolver.TerminalInputUnconfirmed+"]]" {
		t.Fatalf("notices %v, want one %s", notices, resolver.TerminalInputUnconfirmed)
	}
}

// N6: Ctrl-C read together with the tail of a paste still reaches the host alone, so it overtakes the paste.
func TestHelperTerminalSendsCtrlCInARecordOfItsOwn(t *testing.T) {
	message := &capturedHelperMessageConnection{}
	connection := newHelperTerminalConn(message, &resolver.TerminalTarget{SessionID: "ses_1"}, 1)
	connection.streamID = 7
	go connection.writeLoop()
	payload := append(bytes.Repeat([]byte("x"), 1000), 0x03, 'y')
	if written, err := connection.Write(payload); err != nil || written != len(payload) {
		t.Fatalf("written=%d err=%v", written, err)
	}
	connection.finish(0, nil)
	message.mu.Lock()
	defer message.mu.Unlock()
	var bodies []string
	for _, record := range message.writes {
		bodies = append(bodies, string(helperTestInputBody(t, record)))
	}
	if len(bodies) != 3 || len(bodies[0]) != 1000 || bodies[1] != "\x03" || bodies[2] != "y" {
		t.Fatalf("records = %d: lengths %v", len(bodies), func() []int {
			n := []int{}
			for _, b := range bodies {
				n = append(n, len(b))
			}
			return n
		}())
	}
}
