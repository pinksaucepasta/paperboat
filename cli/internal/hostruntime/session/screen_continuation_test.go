package session

import (
	"bytes"
	"testing"

	xterm "github.com/gitpod-io/xterm-go"
)

func TestScreenCheckpointContinuesSplitControlAndUTF8(t *testing.T) {
	for _, test := range []struct {
		name, before, after, pending string
	}{
		{"csi", "hello\x1b[31", "mred", "\x1b[31"},
		{"utf8", "hello\xf0\x9f", "\x8c\x8d", "\xf0\x9f"},
		{"osc", "hello\x1b]0;title\x1b", "\\done", "\x1b]0;title\x1b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			live := xterm.New(xterm.WithCols(80), xterm.WithRows(24))
			defer live.Dispose()
			live.Write([]byte(test.before))
			var continuation screenContinuation
			continuation.feed([]byte(test.before))
			if !bytes.Equal(continuation.pending, []byte(test.pending)) {
				t.Fatalf("pending sequence length = %d, want %d", len(continuation.pending), len(test.pending))
			}
			restored := xterm.New(xterm.WithCols(80), xterm.WithRows(24))
			defer restored.Dispose()
			checkpoint := append(xterm.NewSerializeAddon(live).Serialize(nil), continuation.pending...)
			restored.Write(checkpoint)
			live.Write([]byte(test.after))
			restored.Write([]byte(test.after))
			if live.String() != restored.String() || live.CursorX() != restored.CursorX() || live.CursorY() != restored.CursorY() {
				t.Fatal("restored screen differs after continuation")
			}
		})
	}
}
