//go:build darwin || linux || windows

package pty

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

func TestStalledInputIsBoundedAndDescriptorRemainsUsable(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	started := time.Now()
	n, err := writeInput(writer, bytes.Repeat([]byte("x"), 1<<20), 40*time.Millisecond)
	if !errors.Is(err, os.ErrDeadlineExceeded) || n >= 1<<20 || time.Since(started) > time.Second {
		t.Fatalf("n=%d err=%v elapsed=%v", n, err, time.Since(started))
	}
	drained := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, reader); drained <- err }()
	if _, err := writeInput(writer, []byte("y"), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
}

func TestQueuedInputLockHonorsCancellation(t *testing.T) {
	var mutex sync.Mutex
	mutex.Lock()
	defer mutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := LockInput(ctx, &mutex); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestAlreadyDueInputDeadlineCannotMissSynchronousWrite(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	started := time.Now()
	_, err = writeInput(writer, bytes.Repeat([]byte("x"), 1<<20), 0)
	if !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(started))
	}
}
