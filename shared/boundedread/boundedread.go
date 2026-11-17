// Package boundedread reads a whole io.Reader up to a caller-chosen limit. Callers map ErrTooLarge to their own errors
// and keep their own limits and decoders (tolerant wirejson, closed-world strictjson, or plain text parsing).
package boundedread

import (
	"errors"
	"io"
	"math"
)

// ErrTooLarge is returned when the reader holds more than limit bytes.
var ErrTooLarge = errors.New("input exceeds its size limit")

// firstBuffer is the first read buffer. io.ReadAll starts at 512 bytes, which cost a syscall per three lines on
// /proc/net/tcp (85% of an idle worker's CPU, pprof 2026-11-11), so the first read is large enough for the usual input.
const firstBuffer = 64 << 10

// All reads r to EOF and returns its bytes, or ErrTooLarge when there are more than limit of them. A negative limit
// is always too large. It never reads more than limit+1 bytes.
func All(r io.Reader, limit int64) ([]byte, error) {
	if limit < 0 || limit == math.MaxInt64 {
		return nil, ErrTooLarge
	}
	limited := io.LimitReader(r, limit+1)
	data := make([]byte, 0, min(limit+1, firstBuffer))
	for {
		if len(data) == cap(data) {
			data = append(data, 0)[:len(data)]
		}
		n, err := limited.Read(data[len(data):cap(data)])
		data = data[:len(data)+n]
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}
