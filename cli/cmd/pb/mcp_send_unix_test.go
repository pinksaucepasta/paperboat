//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMCPSendRefusesFIFOWithoutWaitingForWriter(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	b := &mcpBackend{}
	if err := b.openRoots([]string{directory}); err != nil {
		t.Fatal(err)
	}
	defer b.closeRoots()
	finished := make(chan error, 1)
	go func() { _, _, err := b.prepareSend(t.Context(), path); finished <- err }()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("FIFO accepted as a regular send source")
		}
	case <-time.After(time.Second):
		// Release a blocking open before failing, so this test leaves no worker.
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = writer.Close()
		<-finished
		t.Fatal("send_file blocked waiting for a FIFO writer")
	}
}
