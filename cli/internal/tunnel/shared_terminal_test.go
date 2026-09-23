package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

type sharedOnlyMessageConnection struct {
	*earlyOutputMessageConnection
	requests   int
	firstError string
}

func (c *sharedOnlyMessageConnection) WriteMessage(ctx context.Context, kind helperMessageType, data []byte) error {
	if kind == helperStructuredMessage {
		frame, err := decodeHelperFrame(data)
		if err != nil {
			return err
		}
		if frame.Type == "request" {
			var payload struct {
				Action string `json:"action"`
				From   uint64 `json:"from_sequence"`
				Live   bool   `json:"at_live_boundary"`
			}
			if err := json.Unmarshal(frame.Payload, &payload); err != nil {
				return err
			}
			if payload.Action != "attach" || payload.Live {
				return errors.New("shared join attempted owner operation or skipped recent replay")
			}
			c.requests++
			if c.requests == 1 && c.firstError != "" {
				if payload.From != 1 {
					return errors.New("reconnect did not use committed cursor")
				}
				response, _ := json.Marshal(helperFrame{Type: "error", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(c.firstError)})
				c.reads <- struct {
					kind helperMessageType
					data []byte
				}{helperStructuredMessage, response}
				return nil
			}
			if payload.From != 0 {
				return errors.New("shared recovery skipped bounded recent replay")
			}
		}
	}
	return c.earlyOutputMessageConnection.WriteMessage(ctx, kind, data)
}

func TestSharedTerminalJoinsRecentOutputWithoutOwnerOperations(t *testing.T) {
	for _, scope := range []string{"terminal:view", "terminal:control"} {
		t.Run(scope, func(t *testing.T) {
			message := &sharedOnlyMessageConnection{earlyOutputMessageConnection: newEarlyOutputMessageConnection()}
			target := &resolver.TerminalTarget{SessionID: "ses_shared", Auth: resolver.AuthTarget{Scopes: []string{scope}}, RestartIfNotRunning: true}
			connection := newHelperTerminalConn(message, target, 4)
			t.Cleanup(func() { _ = message.Close(); connection.finish(0, nil) })
			if err := connection.initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			output := make([]byte, 5)
			if _, err := io.ReadFull(connection, output); err != nil || string(output) != "early" {
				t.Fatalf("recent output missing: %v", err)
			}
			if message.requests != 1 {
				t.Fatalf("join requests=%d", message.requests)
			}
			if target.ViewOnly() {
				if n, err := connection.Write([]byte("x")); n != 0 || !errors.Is(err, ErrTerminalViewOnly) {
					t.Fatalf("viewer input=%d,%v", n, err)
				}
				if err := connection.Resize(24, 80); !errors.Is(err, ErrTerminalViewOnly) {
					t.Fatalf("viewer resize=%v", err)
				}
			}
		})
	}
}

func TestSharedTerminalReconnectGapRetainsOutputButDenialDoesNotRetry(t *testing.T) {
	for _, denied := range []bool{false, true} {
		name := "retention gap"
		firstError := `{"code":"replay_gap","message":"expired cursor","details":{"requested_sequence":1,"earliest_sequence":10,"latest_sequence":15}}`
		if denied {
			name = "revoked access"
			firstError = `{"code":"not_found_or_forbidden","message":"unavailable"}`
		}
		t.Run(name, func(t *testing.T) {
			message := &sharedOnlyMessageConnection{earlyOutputMessageConnection: newEarlyOutputMessageConnection(), firstError: firstError}
			target := &resolver.TerminalTarget{SessionID: "ses_shared", AfterSequence: 1, Auth: resolver.AuthTarget{Scopes: []string{"terminal:view"}}}
			connection := newHelperTerminalConn(message, target, 4)
			t.Cleanup(func() { _ = message.Close(); connection.finish(0, nil) })
			err := connection.initialize(context.Background())
			if denied {
				if err == nil || message.requests != 1 {
					t.Fatalf("denied reconnect err=%v requests=%d", err, message.requests)
				}
				return
			}
			if err != nil || message.requests != 2 {
				t.Fatalf("gap recovery err=%v requests=%d", err, message.requests)
			}
			output := make([]byte, len(helperReplayGapMarker)+len("early"))
			if _, err := io.ReadFull(connection, output); err != nil || !strings.HasSuffix(string(output), "early") || !strings.HasPrefix(string(output), helperReplayGapMarker) {
				t.Fatalf("gap notice and retained output not delivered: %v", err)
			}
		})
	}
}
