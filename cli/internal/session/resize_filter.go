package session

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// resizeFilter removes the in-band window resize request ESC [ 8 ; rows ; cols t from remote output. The host broadcasts it to every
// attached client when a resize is applied (hostruntime/session/manager_input.go); xterm ignores it, but Terminal.app (a 100x1 window on the Mac VM, X-24) and a ConPTY obey it by
// resizing itself for real, which pb's own size poll then reports back as the client's size: the last typer's size sticks and every other
// window shrinks to it for good (F3-14, N1/N15 on a Windows client). A chunk may end inside a request, so a tail that could still become
// one is held for the next chunk; the caller flushes what stays held (take) when the stream goes quiet or ends.
type resizeFilter struct{ held []byte }

// strip returns chunk without resize requests. The result may alias chunk.
func (f *resizeFilter) strip(chunk []byte) []byte {
	data := chunk
	if len(f.held) > 0 {
		data = append(f.held, chunk...)
		f.held = nil
	}
	if bytes.IndexByte(data, 0x1b) < 0 {
		return data
	}
	var out []byte
	for {
		start := bytes.IndexByte(data, 0x1b)
		if start < 0 {
			return append(out, data...)
		}
		out = append(out, data[:start]...)
		data = data[start:]
		switch end := resizeRequestEnd(data); {
		case end > 0:
			data = data[end:]
		case end < 0: // may still become a request: wait for more bytes
			f.held = append([]byte(nil), data...)
			return out
		default:
			out = append(out, data[0])
			data = data[1:]
		}
	}
}

// take returns the bytes held back and forgets them.
func (f *resizeFilter) take() []byte {
	held := f.held
	f.held = nil
	return held
}

// resizeRequestEnd returns the length of a complete ESC [ 8 ; digits ; digits t at the start of data (which starts with ESC), -1 if data is
// a proper prefix of one, 0 if it is not a request. A request is at most 24 bytes.
func resizeRequestEnd(data []byte) int {
	const head = "\x1b[8;"
	for i := 0; i < len(head); i++ {
		if i >= len(data) {
			return -1
		}
		if data[i] != head[i] {
			return 0
		}
	}
	digits, numbers := 0, 0
	for i := len(head); i < len(data); i++ {
		switch c := data[i]; {
		case c >= '0' && c <= '9':
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

// resizeWriter writes remote output to out without resize requests. A tail held back by the filter is written after quiet (a prefix that
// never completes must not be swallowed) and by Flush when the stream ends.
type resizeWriter struct {
	mu     sync.Mutex
	filter resizeFilter
	out    io.Writer
	timer  *time.Timer
}

const resizeHoldLimit = 100 * time.Millisecond

func (w *resizeWriter) Write(chunk []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	shown := w.filter.strip(chunk)
	if len(w.filter.held) > 0 {
		w.timer = time.AfterFunc(resizeHoldLimit, func() { _ = w.Flush() })
	}
	if len(shown) > 0 {
		if _, err := w.out.Write(shown); err != nil {
			return 0, err
		}
	}
	return len(chunk), nil
}

// Flush writes what the filter still holds.
func (w *resizeWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if held := w.filter.take(); len(held) > 0 {
		_, err := w.out.Write(held)
		return err
	}
	return nil
}
