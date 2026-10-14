package tunnel

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func TestHelperFinalSequenceWaitsForUnreadOutput(t *testing.T) {
	var sequences []int
	c := &helperTerminalConn{
		target: &resolver.TerminalTarget{SessionID: "session_cursor", SequenceSink: func(n int) { sequences = append(sequences, n) }},
		out:    make(chan helperOutput, 1), done: make(chan struct{}),
	}
	c.out <- helperOutput{data: []byte("abcdef"), endSequence: 6}
	if !c.handleEvent(helperFrame{Payload: json.RawMessage(`{"event":"terminal_stream_end","session_id":"session_cursor","final_sequence":9,"exit":{"code":0}}`)}) {
		t.Fatal("terminal end not recognized")
	}
	close(c.out)
	if len(sequences) != 0 {
		t.Fatal("terminal end advanced past unread output")
	}
	buffer := make([]byte, 1)
	for i := 0; i < 6; i++ {
		if n, err := c.Read(buffer); n != 1 || err != nil {
			t.Fatalf("read=(%d,%v)", n, err)
		}
		if i < 5 && len(sequences) != 0 {
			t.Fatal("partial read advanced the cursor")
		}
	}
	if len(sequences) != 1 || sequences[0] != 6 {
		t.Fatalf("consumed cursor=%v", sequences)
	}
	for i := 0; i < 2; i++ {
		if n, err := c.Read(buffer); n != 0 || err != io.EOF {
			t.Fatalf("EOF=(%d,%v)", n, err)
		}
	}
	if len(sequences) != 2 || sequences[1] != 9 {
		t.Fatalf("final cursor=%v", sequences)
	}
}
