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
	sddl := "D:P(A;;GA;;;SY)(A;;GA;;;" + currentUser(t) + ")"
	go func() {
		time.Sleep(200 * time.Millisecond)
		listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl})
		if err != nil {
			return
		}
		t.Cleanup(func() { _ = listener.Close() })
		connection, err := listener.Accept()
		if err == nil {
			_ = connection.Close()
		}
	}()
	connection, err := Dial(context.Background(), path, "")
	if err != nil {
		t.Fatalf("a pipe bound 200 ms later was not reached: %v", err)
	}
	_ = connection.Close()

	began := time.Now()
	if _, err := Dial(context.Background(), path+"-never", ""); !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || time.Since(began) < AbsentWait {
		t.Fatalf("a missing pipe: err %v after %v, want ERROR_FILE_NOT_FOUND after %v", err, time.Since(began), AbsentWait)
	}
}

func currentUser(t *testing.T) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return user.User.Sid.String()
}

// A pipe the caller created but whose DACL lets every user create instances could be served by any account: Dial
// refuses it before a byte is written (tailscale safesocket checkPipeExclusiveToCurrentUser).
func TestDialRefusesAPipeAnyAccountCouldServe(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\paperboat-winpipe-open-%d`, os.Getpid())
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;" + currentUser(t) + ")(A;;GRGW;;;WD)"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		if connection, err := listener.Accept(); err == nil {
			_ = connection.Close()
		}
	}()
	if connection, err := Dial(context.Background(), path, ""); err == nil {
		_ = connection.Close()
		t.Fatal("Dial accepted a pipe that every account may serve")
	}
}
