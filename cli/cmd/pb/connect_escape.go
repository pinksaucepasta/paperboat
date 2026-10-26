package main

import (
	"bytes"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/paste"
)

var (
	bracketedPasteStart = []byte("\x1b[200~")
	bracketedPasteEnd   = []byte("\x1b[201~")
)

// escapeInput reads ssh-style escapes from typed input: Enter ~ r reconnects, Enter ~ . disconnects, ~~ sends ~.
// The ~ counts only at the start of a line and only as a keystroke of its own (a read of one byte), never inside a
// bracketed paste, so pasted text and ordinary typing are never eaten.
type escapeInput struct {
	*paste.Interceptor              // Abort, Discard, Close and Errors pass through
	out                   io.Writer // the interceptor; a recorder in tests
	reconnect, disconnect func()
	lineStart, tilde      bool
	inPaste               bool
	tail                  []byte
}

func newEscapeInput(dest *paste.Interceptor, reconnect, disconnect func()) *escapeInput {
	return &escapeInput{Interceptor: dest, out: dest, reconnect: reconnect, disconnect: disconnect, lineStart: true}
}

func (e *escapeInput) Write(p []byte) (int, error) {
	seen := append(e.tail, p...)
	if start, end := bytes.LastIndex(seen, bracketedPasteStart), bytes.LastIndex(seen, bracketedPasteEnd); start >= 0 || end >= 0 {
		e.inPaste = start > end
	}
	e.tail = append([]byte(nil), seen[max(0, len(seen)-len(bracketedPasteStart)+1):]...)
	out := p
	switch {
	case e.tilde:
		e.tilde = false
		switch {
		case len(p) > 0 && p[0] == 'r' && !e.inPaste:
			e.lineStart = true
			e.reconnect()
			out = p[1:]
		case len(p) > 0 && p[0] == '.' && !e.inPaste:
			e.disconnect()
			return len(p), nil
		case len(p) > 0 && p[0] == '~':
		default:
			out = append([]byte{'~'}, p...)
		}
	case e.lineStart && !e.inPaste && len(p) == 1 && p[0] == '~':
		e.tilde = true
		return 1, nil
	}
	if len(out) == 0 {
		return len(p), nil
	}
	e.lineStart = out[len(out)-1] == '\r' || out[len(out)-1] == '\n'
	if _, err := e.out.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
