//go:build linux || darwin

package session

import (
	"bytes"
	"context"
	"golang.org/x/sys/unix"
	"os"
	"sync"
	"testing"
	"time"
)

// blockedSink holds its first Write until release closes, like a transport
// whose input window is full of a paste the remote program never reads.
type blockedSink struct {
	release chan struct{}
	mu      sync.Mutex
	writes  [][]byte
}

func (s *blockedSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	first := len(s.writes) == 0
	s.writes = append(s.writes, append([]byte(nil), p...))
	s.mu.Unlock()
	if first {
		<-s.release
	}
	return len(p), nil
}
func (s *blockedSink) Close() error { return nil }

// F3-11 on macOS: a paste behind a blocked sink stopped stdin reads, so the
// Ctrl-C after it was never read.
func TestRunKeepsReadingStdinWhileInputIsBlocked(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn := os.Stdin
	os.Stdin = inR
	defer func() { os.Stdin = oldIn; _ = inR.Close(); _ = inW.Close() }()
	s := &blockedSink{release: make(chan struct{})}
	c := &testConn{Reader: bytes.NewBuffer(nil), wait: make(chan struct{})}
	ran := make(chan struct{})
	go func() { defer close(ran); _, _ = Run(context.Background(), c, s, WithOutput(&bytes.Buffer{})) }()

	if _, err := inW.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	for blocked := time.After(5 * time.Second); ; time.Sleep(time.Millisecond) {
		s.mu.Lock()
		n := len(s.writes)
		s.mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-blocked:
			close(s.release)
			t.Fatal("the first input never reached the sink")
		default:
		}
	}
	line := append(bytes.Repeat([]byte("x"), 1023), '\n')
	wrote := make(chan error, 1)
	go func() {
		for range 256 {
			if _, err := inW.Write(line); err != nil {
				wrote <- err
				return
			}
		}
		_, err := inW.Write([]byte{3})
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(s.release)
		t.Fatal("stdin was not read while the sink was blocked; Ctrl-C never reached the client")
	}
	// A pipe write returns once buffered, not once read, and FIONREAD 0 only says Run read the Ctrl-C, not that it
	// queued it: a sink released in between rightly sends the paste first (CI flakes, c74bfb329, f8677c627). Run
	// reads and queues in order, so once a later "z" is read too, the Ctrl-C is queued behind the stuck paste.
	unread := func() int {
		n := -1
		raw, err := inR.SyscallConn()
		if err == nil {
			_ = raw.Control(func(fd uintptr) { n, _ = unix.IoctlGetInt(int(fd), fionread) })
		}
		return n
	}
	waitRead := func() {
		for drained := time.After(5 * time.Second); unread() != 0; {
			select {
			case <-drained:
				close(s.release)
				t.Fatal("stdin still held unread input while the sink was blocked")
			case <-time.After(time.Millisecond):
			}
		}
	}
	waitRead()
	if _, err := inW.Write([]byte("z")); err != nil {
		t.Fatal(err)
	}
	waitRead()
	close(s.release)
	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		first, after := string(s.writes[0]), bytes.Join(s.writes[1:], nil)
		s.mu.Unlock()
		if bytes.IndexByte(after, 'z') >= 0 {
			if first != "a" || string(after) != "\x03z" {
				t.Fatalf("sink got %q then %d bytes, want a then only Ctrl-C and z: the stuck paste was sent", first, len(after))
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("Ctrl-C was not written after the sink unblocked")
		case <-time.After(10 * time.Millisecond):
		}
	}
	_ = c.Close()
	<-ran
}
