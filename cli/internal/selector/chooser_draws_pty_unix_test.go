//go:build !windows

package selector

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Bubble Tea v2 draws only into an output it can size; the error-recording output wrapper must still reach the
// terminal, in the alternate screen and inside a persistent Paperboat screen.
func TestChooserDrawsThroughItsOutputWrapper(t *testing.T) {
	for _, persistent := range []int32{0, 1} {
		master, slave, err := pty.Open()
		if err != nil {
			t.Fatal(err)
		}
		_ = pty.Setsize(slave, &pty.Winsize{Rows: 30, Cols: 100})
		persistentScreen.Store(persistent)
		go func() {
			_, _ = Choose(Options{Title: "ZZTITLE", Items: []Item{{ID: "one", Title: "ZZONE"}}, Stdin: slave, Output: slave})
		}()
		var mu sync.Mutex
		var out bytes.Buffer
		go func() {
			buf := make([]byte, 65536)
			for {
				n, err := master.Read(buf)
				mu.Lock()
				out.Write(buf[:n])
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
		time.Sleep(3 * time.Second)
		mu.Lock()
		defer mu.Unlock()
		if !bytes.Contains(out.Bytes(), []byte("ZZTITLE")) || !bytes.Contains(out.Bytes(), []byte("ZZONE")) {
			t.Errorf("persistent=%d: the chooser drew nothing (%d bytes)", persistent, out.Len())
		}
		_ = master.Close()
		_ = slave.Close()
	}
	persistentScreen.Store(0)
}
