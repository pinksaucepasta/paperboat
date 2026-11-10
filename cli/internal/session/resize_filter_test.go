package session

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResizeFilterRemovesTheRequestAndNothingElse(t *testing.T) {
	var f resizeFilter
	for input, want := range map[string]string{
		"plain":                       "plain",
		"a\x1b[8;24;80tb":             "ab",
		"\x1b[8;30;100t\x1b[8;24;80t": "",
		"x\x1b[31mred\x1b[0m":         "x\x1b[31mred\x1b[0m",
		"\x1b[8;24tkeep":              "\x1b[8;24tkeep", // not a request: one number
		"\x1b[8;;80tkeep":             "\x1b[8;;80tkeep",
		"\x1b[8;24;80;5t":             "\x1b[8;24;80;5t",
		"\x1b[18tkeep":                "\x1b[18tkeep", // a different report request
		"\x1b[8;24;80t\x1b[8;1H":      "\x1b[8;1H",    // a cursor move with the same start survives
	} {
		if got := string(f.strip([]byte(input))); got != want {
			t.Errorf("strip(%q)=%q want %q", input, got, want)
		}
	}
}

func TestResizeFilterHandlesARequestSplitAtEveryByte(t *testing.T) {
	request := "\x1b[8;30;100t"
	for cut := 1; cut < len(request); cut++ {
		var f resizeFilter
		var got strings.Builder
		got.Write(f.strip([]byte("a" + request[:cut])))
		got.Write(f.strip([]byte(request[cut:] + "b")))
		if got.String() != "ab" {
			t.Errorf("cut %d: got %q want ab", cut, got.String())
		}
	}
}

// A prefix that never completes is not lost: the next byte that cannot continue it releases it, and take releases it when the stream
// goes quiet or ends.
func TestResizeFilterReleasesAPrefixThatNeverCompletes(t *testing.T) {
	var f resizeFilter
	if got := f.strip([]byte("x\x1b[8;24")); string(got) != "x" {
		t.Fatalf("held tail leaked: %q", got)
	}
	if got := string(f.take()); got != "\x1b[8;24" {
		t.Fatalf("take()=%q", got)
	}
	if got := f.strip([]byte("\x1b[8;24")); string(got) != "" {
		t.Fatalf("got %q", got)
	}
	if got := string(f.strip([]byte("Z"))); got != "\x1b[8;24Z" {
		t.Fatalf("a byte that cannot continue the prefix must release it, got %q", got)
	}
}

func TestResizeWriterFlushesAHeldPrefixAfterQuietAndAtTheEnd(t *testing.T) {
	var out bytes.Buffer
	var mu sync.Mutex
	w := &resizeWriter{out: lockedWriter{&mu, &out}}
	if _, err := w.Write([]byte("a\x1b[8;24")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * resizeHoldLimit)
	mu.Lock()
	got := out.String()
	mu.Unlock()
	if got != "a\x1b[8;24" {
		t.Fatalf("after quiet got %q", got)
	}
	out.Reset()
	if _, err := w.Write([]byte("\x1b[")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "\x1b[" {
		t.Fatalf("flush gave %q", out.String())
	}
}

type lockedWriter struct {
	mu  *sync.Mutex
	out *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}
