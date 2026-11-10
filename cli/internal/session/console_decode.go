package session

import "unicode/utf16"

// consoleDecoder turns the UTF-16 units a Windows console read returns into the UTF-8 bytes the remote shell gets. It carries
// what a read leaves behind: the high half of a surrogate pair whose low half is in the next read (an emoji split at a
// buffer edge), and decoded bytes that did not fit the caller's buffer. It lives in a platform-neutral file so its
// tests run everywhere.
type consoleDecoder struct {
	high uint16 // pending high surrogate, 0 if none
	rest []byte // decoded bytes not yet handed out
}

func isHighSurrogate(unit uint16) bool { return unit >= 0xd800 && unit < 0xdc00 }

// pending reports whether decode can fill a buffer without another console read.
func (d *consoleDecoder) pending() bool { return len(d.rest) > 0 }

// decode appends units to the carried state and copies as many decoded bytes as fit into p. A trailing high surrogate is held
// back for the next call; it is never decoded alone (that would send U+FFFD for half an emoji).
func (d *consoleDecoder) decode(p []byte, units []uint16) int {
	if d.high != 0 {
		units = append([]uint16{d.high}, units...)
		d.high = 0
	}
	if n := len(units); n > 0 && isHighSurrogate(units[n-1]) {
		d.high = units[n-1]
		units = units[:n-1]
	}
	d.rest = append(d.rest, string(utf16.Decode(units))...)
	n := copy(p, d.rest)
	d.rest = d.rest[n:]
	return n
}
