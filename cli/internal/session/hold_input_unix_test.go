//go:build !windows

package session

import (
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// Type-ahead typed before the shell attaches must arrive with its CR, not as the LF a cooked terminal makes of it.
func TestHoldInputKeepsEnterAsCR(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()
	before, err := term.GetState(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	oldIn := os.Stdin
	os.Stdin = tty
	defer func() { os.Stdin = oldIn }()
	restore := HoldInput()
	if _, err := ptmx.Write([]byte("echo a\r")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	// Run starts here in real use: raw mode, then the first read.
	raw, err := term.MakeRaw(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	_ = tty.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := tty.Read(buf)
	if err != nil || string(buf[:n]) != "echo a\r" {
		t.Fatalf("type-ahead = %q, %v; want %q", buf[:n], err, "echo a\r")
	}
	_ = term.Restore(int(tty.Fd()), raw)
	restore()
	after, err := term.GetState(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if *after != *before {
		t.Fatal("HoldInput did not put the terminal back")
	}
}
