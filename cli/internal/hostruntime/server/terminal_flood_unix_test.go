//go:build darwin || linux

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

// F3-11 on the wire: a program that floods output and never reads stdin (yes) must not stop the connection taking
// input. A real client writes with a 10 s bound and drops the connection when a write cannot finish, so every input
// frame of a 256 KiB paste must be taken promptly, and the Ctrl-C after it must stop the program.
func TestFloodingProgramStillTakesPasteAndCtrlC(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	server := verticalServerCommand(t, []string{"-c", "exec yes"})
	handler, err := NewWebSocketHandler(WebSocketHandlerConfig{Server: server, MaxConnections: 2, Authorizer: func(string) (Authorizer, error) {
		return server.config.Authorizer, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	connection, cleanup := dialPipe(t, handler, http.Header{"Authorization": []string{"Bearer token"}}, []string{DefaultWebSocketSubprotocol})
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	structured := make(chan protocol.Frame, 16)
	var outputSequence atomic.Uint64
	var outputBytes atomic.Int64
	go func() {
		for {
			kind, data, err := connection.Read(ctx)
			if err != nil {
				if ctx.Err() == nil {
					t.Logf("connection ended: %v", err)
				}
				close(structured)
				return
			}
			if kind == websocket.MessageText {
				var frame protocol.Frame
				if json.Unmarshal(data, &frame) == nil {
					select {
					case structured <- frame:
					default:
					}
				}
				continue
			}
			if output, err := protocol.DecodeTerminalOutput(data); err == nil && (output.Channel == protocol.TerminalStdout || output.Channel == protocol.TerminalStderr) {
				outputSequence.Store(output.StartSequence + uint64(len(output.Data)))
				outputBytes.Add(int64(len(output.Data)))
			}
		}
	}()
	send := func(frame protocol.Frame) protocol.Frame {
		t.Helper()
		wire, _ := json.Marshal(frame)
		if err := connection.Write(ctx, websocket.MessageText, wire); err != nil {
			t.Fatal(err)
		}
		for response := range structured {
			if response.RequestID == frame.RequestID {
				return response
			}
		}
		t.Fatalf("no response to %s", frame.RequestID)
		return protocol.Frame{}
	}
	send(protocol.Frame{Type: "hello", RequestID: "req_hello", Version: "1.0", Payload: json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1"]}`)})
	var created struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(send(request("req_create", "op_create_flood", json.RawMessage(`{"action":"create","name":"flood","columns":80,"rows":24}`))).Payload, &created); err != nil || created.Result.ID == "" {
		t.Fatalf("create: %v", err)
	}
	attachPayload, _ := json.Marshal(map[string]any{"action": "attach", "session_id": created.Result.ID, "at_live_boundary": true})
	var attached struct {
		Result struct {
			StreamID uint32 `json:"stream_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(send(request("req_attach", "op_attach_flood", attachPayload)).Payload, &attached); err != nil || attached.Result.StreamID == 0 {
		t.Fatalf("attach: %v", err)
	}
	stream := attached.Result.StreamID
	write := func(message []byte) time.Duration {
		t.Helper()
		started := time.Now()
		writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		if err := connection.Write(writeCtx, websocket.MessageBinary, message); err != nil {
			t.Fatalf("a write did not finish within 5 s (a real client drops the connection at 10 s): %v", err)
		}
		return time.Since(started)
	}
	// Acknowledge output like a real client, so the flood keeps flowing.
	acking := make(chan struct{})
	go func() {
		defer close(acking)
		for ctx.Err() == nil {
			if next := outputSequence.Load(); next > 0 {
				ack, _ := protocol.EncodeTerminalACK(protocol.TerminalACKFrame{StreamID: stream, NextSequence: next}, nil)
				ackCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				_ = connection.Write(ackCtx, websocket.MessageBinary, ack)
				stop()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	time.Sleep(500 * time.Millisecond)
	line := append(bytes.Repeat([]byte("x"), 1023), '\n')
	sequence := uint64(0)
	for range 256 {
		sequence++
		input, _ := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: stream, Sequence: sequence, Data: line}, nil)
		write(input)
	}
	sequence++
	interrupt, _ := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: stream, Sequence: sequence, Data: []byte{3}}, nil)
	write(interrupt)
	// yes has stopped once output stays quiet.
	deadline := time.Now().Add(5 * time.Second)
	for last := outputBytes.Load(); ; {
		time.Sleep(500 * time.Millisecond)
		now := outputBytes.Load()
		if now == last {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("output still flowing 5 s after Ctrl-C")
		}
		last = now
	}
	cancel()
	<-acking
}
