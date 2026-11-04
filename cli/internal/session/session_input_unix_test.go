//go:build !windows

package session

import (
	"bytes"
	"context"
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
	time.Sleep(50 * time.Millisecond)
	close(s.release)
	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		last := s.writes[len(s.writes)-1]
		s.mu.Unlock()
		if len(last) > 0 && last[len(last)-1] == 3 {
			if len(last) != 1 {
				t.Fatalf("Ctrl-C write carried %d bytes of the stuck paste", len(last)-1)
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
