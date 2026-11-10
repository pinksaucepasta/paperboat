package session

import (
	"strings"
	"testing"
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

func TestResizeFilterHandlesARequestSplitAcrossChunks(t *testing.T) {
	request := "\x1b[8;30;100t"
	for cut := 1; cut < len(request); cut++ {
		var f resizeFilter
		var got strings.Builder
		got.Write(f.strip([]byte("a" + request[:cut])))
		got.Write(f.strip([]byte(request[cut:] + "b")))
		// A cut inside ESC [ passes the first bytes through (documented shortcut); every later cut is removed whole.
		if cut >= 3 && got.String() != "ab" {
			t.Errorf("cut %d: got %q want ab", cut, got.String())
		}
	}
}
