//go:build windows

package hostdproto

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

// The Windows handover (W21 Part B step 3): a new generation that dies before its commit leaves the old one serving
// without ever stopping; one that fails after the old one stopped accepting makes the old one serve again; one that
// acknowledges ends the old one's handover. Each generation has its own pipes, so nothing is bound again by name
// (a pipe with a connected instance cannot be: winpool-4 2026-11-11).
func TestWindowsHandoverStopsOnlyAtCommitAndResumesWithoutAnAck(t *testing.T) {
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	path := `\\.\pipe\paperboat-handover-test-` + hex.EncodeToString(suffix)
	token := bytes.Repeat([]byte{7}, installationTokenBytes)
	listener, err := winio.ListenPipe(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var mu sync.Mutex
	stops, resumes := 0, 0
	config := HandoverConfig{
		Token:         token,
		Listener:      func() net.Listener { return listener },
		StopAccepting: func(context.Context) error { mu.Lock(); defer mu.Unlock(); stops++; return nil },
		Resume:        func(context.Context) error { mu.Lock(); defer mu.Unlock(); resumes++; return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- ServeHandover(ctx, config) }()
	// Between two attempts the old generation may not have its next pipe instance up yet.
	dial := func(token []byte) (*Handover, error) {
		deadline := time.Now().Add(2 * time.Second)
		for {
			h, err := DialHandover(ctx, path, token)
			if !errors.Is(err, ErrNoHandover) || time.Now().After(deadline) {
				return h, err
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	counts := func() (int, int) { mu.Lock(); defer mu.Unlock(); return stops, resumes }

	if _, err := dial(bytes.Repeat([]byte{8}, installationTokenBytes)); err == nil {
		t.Fatal("a wrong token was answered")
	}
	h, err := dial(token)
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close() // the new generation dies before its commit
	h, err = dial(token)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := counts(); s != 0 {
		t.Fatalf("the old generation stopped %d times before any commit", s)
	}
	bindErr := errors.New("activation failed")
	if err := h.Commit(func() error { return bindErr }); !errors.Is(err, bindErr) {
		t.Fatalf("Commit = %v, want the activation error", err)
	}
	_ = h.Close()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if s, r := counts(); s == 1 && r == 1 {
			break
		}
		if time.Now().After(deadline) {
			s, r := counts()
			t.Fatalf("after an unacknowledged commit: %d stops, %d resumes; want 1 and 1", s, r)
		}
	}
	h, err = dial(token)
	if err != nil {
		t.Fatal(err)
	}
	activated := false
	if err := h.Commit(func() error {
		// The old generation has stopped accepting before the new one activates.
		if s, _ := counts(); s != 2 {
			return errors.New("activated before the old generation stopped accepting")
		}
		activated = true
		return nil
	}); err != nil || !activated {
		t.Fatalf("Commit = %v (activated %v)", err, activated)
	}
	_ = h.Close()
	if err := <-served; err != nil {
		t.Fatalf("ServeHandover after a completed handover = %v", err)
	}
	if s, r := counts(); s != 2 || r != 1 {
		t.Fatalf("%d stops, %d resumes; want 2 and 1", s, r)
	}
}
