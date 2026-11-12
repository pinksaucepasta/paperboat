//go:build windows

package hostdproto

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

// A new worker whose activation outlasts the old step deadline and the old 5 s acknowledgement window (its control
// plane component starts after components that call a slow control plane) still completes the handover: the old
// generation does not take its address back, and the acknowledgement is written (winpool-7, 2026-11-12).
func TestWindowsHandoverSurvivesAnActivationLongerThanTheStepDeadline(t *testing.T) {
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	path := `\\.\pipe\paperboat-handover-slow-test-` + hex.EncodeToString(suffix)
	token := bytes.Repeat([]byte{9}, installationTokenBytes)
	listener, err := winio.ListenPipe(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var resumes atomic.Int32
	config := HandoverConfig{
		Token:         token,
		Listener:      func() net.Listener { return listener },
		StopAccepting: func(context.Context) error { return nil },
		Resume:        func(context.Context) error { resumes.Add(1); return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- ServeHandover(ctx, config) }()
	var h *Handover
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if h, err = DialHandover(ctx, path, token); !errors.Is(err, ErrNoHandover) || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.Commit(func() error { time.Sleep(handoverStepTimeout + time.Second); return nil }); err != nil {
		t.Fatalf("Commit after a slow activation = %v", err)
	}
	if err := <-served; err != nil {
		t.Fatalf("ServeHandover = %v", err)
	}
	if n := resumes.Load(); n != 0 {
		t.Fatalf("the old generation took its address back %d times before the acknowledgement", n)
	}
}
