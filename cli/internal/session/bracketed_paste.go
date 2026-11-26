package session

import (
	"bytes"
	"sync/atomic"
)

const bracketedPasteEnableSequence = "\x1b[?2004h"

// RemotePaste is bracketed paste as the remote program asked for it. The local terminal has it on for the whole attach,
// so pb can tell a paste or a dropped file from typing; a paste is framed for the remote only while Enabled. As tmux:
// tty.c:393 enables it on the outer terminal, input.c:2099 tracks the pane's DECSET 2004 and input-keys.c:667 drops
// the markers for a pane without it.
// shortcut: output lost in a replay gap can leave this stale until the program next sets the mode; read the host's
// snapshot modes on a gap if that is ever seen.
type RemotePaste struct {
	enabled atomic.Bool
	seq     []byte // the escape sequence being read, which may span output chunks; only follow touches it
}

// Enabled reports whether the remote program has bracketed paste on.
func (p *RemotePaste) Enabled() bool { return p.enabled.Load() }

// follow records the remote program's changes in output and returns it with the local mode switched on again right
// after every sequence that switched it off. The result is output itself when nothing did.
func (p *RemotePaste) follow(output []byte) []byte {
	var restored []byte
	copied := 0
	for i, b := range output {
		if p.turnedOff(b) {
			restored = append(append(restored, output[copied:i+1]...), bracketedPasteEnableSequence...)
			copied = i + 1
		}
	}
	if copied == 0 {
		return output
	}
	return append(restored, output[copied:]...)
}

// turnedOff adds b to the sequence being read and reports whether b completed one that turned the mode off.
func (p *RemotePaste) turnedOff(b byte) bool {
	if b == 0x1b {
		p.seq = append(p.seq[:0], b)
		return false
	}
	if len(p.seq) == 0 {
		return false
	}
	p.seq = append(p.seq, b)
	if len(p.seq) == 2 {
		if b == '[' {
			return false
		}
		p.seq = p.seq[:0]
		if b != 'c' {
			return false
		}
		p.enabled.Store(false) // a full reset (RIS) turns every mode off
		return true
	}
	if b >= 0x20 && b <= 0x3f && len(p.seq) < 64 { // a parameter or intermediate byte
		return false
	}
	modes, private := bytes.CutPrefix(p.seq[2:len(p.seq)-1], []byte("?"))
	p.seq = p.seq[:0]
	if !private || b != 'h' && b != 'l' {
		return false
	}
	for _, mode := range bytes.Split(modes, []byte(";")) {
		if string(mode) == "2004" {
			p.enabled.Store(b == 'h')
			return b == 'l'
		}
	}
	return false
}
