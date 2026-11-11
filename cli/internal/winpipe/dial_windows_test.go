//go:build windows

package winpipe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// A hostd generation handover leaves its fixed pipes unbound for milliseconds: a client dialing then reaches the
// new generation instead of failing (W21 Part B). A pipe that never appears is reported after AbsentWait.
func TestDialWaitsForAPipeThatAppears(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\paperboat-winpipe-test-%d`, os.Getpid())
	go func() {
		time.Sleep(200 * time.Millisecond)
		listener, err := winio.ListenPipe(path, nil)
		if err != nil {
			return
		}
		t.Cleanup(func() { _ = listener.Close() })
		connection, err := listener.Accept()
		if err == nil {
			_ = connection.Close()
		}
	}()
	connection, err := Dial(context.Background(), path)
	if err != nil {
		t.Fatalf("a pipe bound 200 ms later was not reached: %v", err)
	}
	_ = connection.Close()

	began := time.Now()
	if _, err := Dial(context.Background(), path+"-never"); !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || time.Since(began) < AbsentWait {
		t.Fatalf("a missing pipe: err %v after %v, want ERROR_FILE_NOT_FOUND after %v", err, time.Since(began), AbsentWait)
	}
}
