package session

import (
	"bytes"
	"sync/atomic"
)

const (
	bracketedPasteEnableSequence = "\x1b[?2004h"
	// win32InputDisableSequence keeps keys and pastes coming from the local console as the plain bytes pb reads. A Windows
	// target's ConPTY asks for win32-input-mode (DECSET 9001) when it starts; a local console that obeys encodes every
	// byte as a key record (ESC[Vk;Sc;Uc;Kd;Cs;Rc_), which hides a paste or a dropped file, Ctrl-C and the ~ escapes.
	// The target takes plain input, as from Linux or macOS. As tmux, which never passes the mode to the outer terminal.
	win32InputDisableSequence = "\x1b[?9001l"
)

// RemotePaste is bracketed paste as the remote program asked for it. The local terminal has it on for the whole attach,
// so pb can tell a paste or a dropped file from typing; a paste is framed for the remote only while Enabled. As tmux:
// tty.c:393 enables it on the outer terminal, input.c:2099 tracks the pane's DECSET 2004 and input-keys.c:667 drops
// the markers for a pane without it.
// shortcut: output lost in a replay gap can leave this stale until the program next sets the mode; read the host's
// snapshot modes on a gap if that is ever seen.
type RemotePaste struct {
	enabled atomic.Bool
	seq     []byte // the escape sequence being read, which may span output chunks; only follow touches it
	// unframed is a shared-terminal join, whose input is not framed by pb: its bracketed paste is left as the remote set it
	unframed bool
}

// Enabled reports whether the remote program has bracketed paste on.
func (p *RemotePaste) Enabled() bool { return p.enabled.Load() }

// follow records the remote program's changes in output and returns it with pb's local modes restored right after every
// sequence that changed them. The result is output itself when nothing did.
func (p *RemotePaste) follow(output []byte) []byte {
	var restored []byte
	copied := 0
	for i, b := range output {
		if restore := p.read(b); restore != "" {
			restored = append(append(restored, output[copied:i+1]...), restore...)
			copied = i + 1
		}
	}
	if copied == 0 {
		return output
	}
	return append(restored, output[copied:]...)
}

// read adds b to the sequence being read and returns what restores the local modes if b completed one that changed them.
func (p *RemotePaste) read(b byte) string {
	if b == 0x1b {
		p.seq = append(p.seq[:0], b)
		return ""
	}
	if len(p.seq) == 0 {
		return ""
	}
	p.seq = append(p.seq, b)
	if len(p.seq) == 2 {
		if b == '[' {
			return ""
		}
		p.seq = p.seq[:0]
		if b != 'c' {
			return ""
		}
		p.enabled.Store(false) // a full reset (RIS) turns every mode off
		return p.restorePaste()
	}
	if b >= 0x20 && b <= 0x3f && len(p.seq) < 64 { // a parameter or intermediate byte
		return ""
	}
	modes, private := bytes.CutPrefix(p.seq[2:len(p.seq)-1], []byte("?"))
	p.seq = p.seq[:0]
	if !private || b != 'h' && b != 'l' {
		return ""
	}
	restore := ""
	for _, mode := range bytes.Split(modes, []byte(";")) {
		switch string(mode) {
		case "2004":
			p.enabled.Store(b == 'h')
			if b == 'l' {
				restore += p.restorePaste()
			}
		case "9001":
			if b == 'h' {
				restore += win32InputDisableSequence
			}
		}
	}
	return restore
}

func (p *RemotePaste) restorePaste() string {
	if p.unframed {
		return ""
	}
	return bracketedPasteEnableSequence
}
