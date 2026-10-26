package session

import (
	"bytes"
	"fmt"
	"testing"

	xterm "github.com/gitpod-io/xterm-go"
)

// A browser screen restored after a flood (a resize, a soft-wrapped 400 KB line, the cursor mid-row) holds exactly the
// host's cells across the whole scrollback, and later output lands on the same rows (F3-13 stale-cell report).
func TestBrowserScreenRestoresExactly(t *testing.T) {
	for _, cut := range []int{0, 1, 3, 7} {
		host := xterm.New(xterm.WithCols(80), xterm.WithRows(24), xterm.WithScrollback(browserScreenScrollbackLines))
		host.Write(bytes.Repeat([]byte("x\r\n"), 3))
		host.Resize(100, 30)
		host.Resize(80, 24)
		host.Write(bytes.Repeat([]byte("0123456789"), 40000))
		host.Write([]byte("\r\n$ "))
		for i := 1; i <= cut; i++ {
			fmt.Fprintf(host, "WH%d\r\n", i)
		}
		cp := xterm.NewSerializeAddon(host).Serialize(nil)
		page := xterm.New(xterm.WithCols(80), xterm.WithRows(24), xterm.WithScrollback(browserScreenScrollbackLines))
		page.Write(cp)
		for i := cut + 1; i <= cut+30; i++ {
			fmt.Fprintf(host, "WH%d\r\n", i)
			fmt.Fprintf(page, "WH%d\r\n", i)
		}
		hb, pb := host.Buffer(), page.Buffer()
		if hb.Lines.Length() != pb.Lines.Length() {
			t.Errorf("cut %d lines %d vs %d", cut, hb.Lines.Length(), pb.Lines.Length())
		}
		for y := 0; y < min(hb.Lines.Length(), pb.Lines.Length()); y++ {
			if a, b := hb.Lines.Get(y).TranslateToString(false, 0, -1), pb.Lines.Get(y).TranslateToString(false, 0, -1); a != b {
				t.Errorf("cut %d row %d: host %q page %q", cut, y, a, b)
			}
		}
	}
}
