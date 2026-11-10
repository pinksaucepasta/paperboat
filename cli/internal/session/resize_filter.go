package session

import "bytes"

// resizeFilter removes the in-band window resize request ESC [ 8 ; rows ; cols t from remote output. The host broadcasts it to every
// attached client when a resize is applied (hostruntime/session/manager_input.go); a Unix terminal ignores it, but a ConPTY obeys it by
// resizing itself for real, which pb's own size poll then reports back as the client's size: the last typer's size sticks and every other
// window shrinks to it for good (F3-14, N1/N15 on a Windows client). Chunks may split a request, so a possible start at the end of a chunk
// is held for the next one.
// shortcut: only ESC [ 8 is held back at a chunk end (a split between ESC and [ or inside a request that does not start like one passes through
// unfiltered); upgrade if a remote ever splits at those two bytes.
type resizeFilter struct{ held []byte }

var resizePrefix = []byte("\x1b[8;")

// strip returns chunk without resize requests. The result may alias chunk.
func (f *resizeFilter) strip(chunk []byte) []byte {
	data := chunk
	if len(f.held) > 0 {
		data = append(f.held, chunk...)
		f.held = nil
	}
	if !bytes.Contains(data, resizePrefix[:3]) {
		return data
	}
	var out []byte
	for {
		start := bytes.Index(data, resizePrefix[:3])
		if start < 0 {
			return append(out, data...)
		}
		out = append(out, data[:start]...)
		data = data[start:]
		if end := resizeRequestEnd(data); end > 0 {
			data = data[end:]
			continue
		} else if end < 0 { // may still become a request: wait for more bytes
			f.held = append([]byte(nil), data...)
			return out
		}
		out = append(out, data[:1]...)
		data = data[1:]
	}
}

// resizeRequestEnd returns the length of a complete ESC [ 8 ; digits ; digits t at the start of data (which starts with ESC [ 8), -1 if
// data is a proper prefix of one, 0 if it is not a request. A request is at most 24 bytes.
func resizeRequestEnd(data []byte) int {
	digits := 0 // digits seen in the current number
	numbers := 0
	for i := 3; i < len(data); i++ {
		switch c := data[i]; {
		case i == 3 && c == ';':
		case c >= '0' && c <= '9' && i > 3:
			digits++
		case c == ';' && digits > 0 && numbers == 0:
			numbers, digits = 1, 0
		case c == 't' && digits > 0 && numbers == 1:
			return i + 1
		default:
			return 0
		}
		if i >= 24 {
			return 0
		}
	}
	return -1
}
