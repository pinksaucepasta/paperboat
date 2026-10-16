package tunnel

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func TestHelperFinalSequenceWaitsForUnreadOutput(t *testing.T) {
	for _, screen := range []bool{false, true} {
		name := "raw"
		if screen {
			name = "screen"
		}
		t.Run(name, func(t *testing.T) {
			var sequences []int
			c := &helperTerminalConn{target: &resolver.TerminalTarget{SessionID: "session_cursor", SequenceSink: func(n int) { sequences = append(sequences, n) }}, streamID: 1, out: make(chan helperOutput, 2), done: make(chan struct{})}
			frames := []protocol.TerminalOutputFrame{{Channel: protocol.TerminalStdout, StreamID: 1, Data: []byte("abcdef")}}
			consumed, final := 6, 9
			if screen {
				frames = []protocol.TerminalOutputFrame{{Channel: protocol.TerminalScreenCheckpoint, StreamID: 1, StartSequence: 41, Data: append([]byte{0}, []byte("abc")...)}, {Channel: protocol.TerminalScreenCheckpoint, StreamID: 1, StartSequence: 41, Data: append([]byte{1}, []byte("def")...)}}
				consumed, final = 41, 49
			}
			for _, frame := range frames {
				encoded, err := protocol.EncodeTerminalOutputAdaptive(frame, nil)
				if err != nil {
					t.Fatal(err)
				}
				output, err := c.decodeHelperBinary(encoded)
				if err != nil {
					t.Fatal(err)
				}
				c.out <- output
			}
			event, _ := json.Marshal(map[string]any{"event": "terminal_stream_end", "session_id": "session_cursor", "final_sequence": final, "exit": map[string]int{"code": 0}})
			if !c.handleEvent(helperFrame{Payload: event}) {
				t.Fatal("terminal end not recognized")
			}
			close(c.out)
			if len(sequences) != 0 {
				t.Fatal("terminal end advanced past unread output")
			}
			buffer := make([]byte, 1)
			var rendered []byte
			for i := 0; i < 6; i++ {
				if n, err := c.Read(buffer); n != 1 || err != nil {
					t.Fatalf("read=(%d,%v)", n, err)
				}
				rendered = append(rendered, buffer[0])
				if i < 5 && len(sequences) != 0 {
					t.Fatal("partial read advanced the cursor")
				}
			}
			if string(rendered) != "abcdef" {
				t.Fatal("terminal rendered checkpoint framing instead of screen bytes")
			}
			if len(sequences) != 1 || sequences[0] != consumed {
				t.Fatalf("consumed cursor=%v", sequences)
			}
			for i := 0; i < 2; i++ {
				if n, err := c.Read(buffer); n != 0 || err != io.EOF {
					t.Fatalf("EOF=(%d,%v)", n, err)
				}
			}
			if len(sequences) != 2 || sequences[1] != final {
				t.Fatalf("final cursor=%v", sequences)
			}
		})
	}
}

func TestHelperScreenCheckpointRejectsInvalidWireSequences(t *testing.T) {
	for _, name := range []string{"invalid flag", "changed boundary", "interrupted screen", "oversized screen"} {
		t.Run(name, func(t *testing.T) {
			c := &helperTerminalConn{streamID: 1}
			send := func(channel byte, boundary uint64, data []byte) error {
				encoded, err := protocol.EncodeTerminalOutputAdaptive(protocol.TerminalOutputFrame{Channel: channel, StreamID: 1, StartSequence: boundary, Data: data}, nil)
				if err != nil {
					t.Fatal(err)
				}
				_, err = c.decodeHelperBinary(encoded)
				return err
			}
			var err error
			switch name {
			case "invalid flag":
				err = send(protocol.TerminalScreenCheckpoint, 41, []byte{2})
			case "changed boundary", "interrupted screen":
				if err = send(protocol.TerminalScreenCheckpoint, 41, []byte{0, 'a'}); err != nil {
					t.Fatal(err)
				}
				if name == "changed boundary" {
					err = send(protocol.TerminalScreenCheckpoint, 42, []byte{1, 'b'})
				} else {
					err = send(protocol.TerminalStdout, 41, []byte{'b'})
				}
			case "oversized screen":
				chunk := make([]byte, 1+(64<<10))
				for i := 0; i < protocol.MaxTerminalScreenCheckpointBytes/(64<<10); i++ {
					if err = send(protocol.TerminalScreenCheckpoint, 41, chunk); err != nil {
						t.Fatal(err)
					}
				}
				err = send(protocol.TerminalScreenCheckpoint, 41, []byte{1, 'b'})
			}
			if err == nil {
				t.Fatal("invalid terminal screen stream was accepted")
			}
		})
	}
}
