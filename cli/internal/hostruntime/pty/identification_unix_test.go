//go:build darwin || linux

package pty

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func identificationShell(t *testing.T) (*Process, string) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	process, err := adapter.Start(Command{Path: shellPath(t), Args: []string{"-i"}, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm", "PS1=", "PS2="}, CWD: root, Dimensions: Dimensions{80, 24}})
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	t.Cleanup(func() {
		// An interactive shell gives children their own foreground group.
		// Clean that group as well if a failed assertion interrupted the test.
		if connection, err := process.file.SyscallConn(); err == nil {
			_ = connection.Control(func(fd uintptr) {
				group, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
				if err == nil && group > 0 && group != process.cmd.Process.Pid {
					_ = unix.Kill(-group, unix.SIGKILL)
				}
			})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := process.Terminate(ctx, 10*time.Millisecond)
		if err != nil {
			t.Errorf("terminate test shell: %v", err)
		}
		_ = process.CloseIO()
		select {
		case <-drained:
		case <-time.After(time.Second):
			t.Error("test shell output reader did not stop")
		}
	})
	// On Darwin /bin/sh initially reports "sh", then initializes as "bash".
	// Exercise a shell command before recording the idle-shell baseline so the
	// comparison cannot mistake that startup transition for a foreground job.
	ready := make(chan error, 1)
	go func() {
		defer close(drained)
		reader := bufio.NewReader(process)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "paperboat-identification-ready" {
				ready <- err
				// Darwin may wait for pending terminal output to drain before
				// reporting process exit. Keep consuming the real PTY throughout
				// the test, as the runtime's output loop does.
				if err == nil {
					_, _ = io.Copy(io.Discard, reader)
				}
				return
			}
		}
	}()
	if _, err := process.Write([]byte("printf 'paperboat-identification-ready\\n'\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interactive shell did not become ready")
	}
	return process, root
}

func awaitIdentification(t *testing.T, process *Process, match func(Identification) bool) Identification {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last Identification
	for time.Now().Before(deadline) {
		last = process.Identification()
		if match(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("foreground metadata did not reach expected state: %+v", last)
	return Identification{}
}

func TestIdentificationTracksLiveDirectoryAndForegroundJob(t *testing.T) {
	process, root := identificationShell(t)
	initial := awaitIdentification(t, process, func(value Identification) bool {
		return value.ForegroundProcess != "" && value.CurrentDirectory == root
	})
	directory := filepath.Join(root, "next")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Write([]byte("cd next\n")); err != nil {
		t.Fatal(err)
	}
	awaitIdentification(t, process, func(value Identification) bool {
		return value.ForegroundProcess == initial.ForegroundProcess && value.CurrentDirectory == directory
	})
	if _, err := process.Write([]byte("sleep 30\n")); err != nil {
		t.Fatal(err)
	}
	awaitIdentification(t, process, func(value Identification) bool {
		return value.ForegroundProcess == "sleep" && value.CurrentDirectory == directory
	})
	if _, err := process.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	awaitIdentification(t, process, func(value Identification) bool {
		return value.ForegroundProcess == initial.ForegroundProcess && value.CurrentDirectory == directory
	})
}

func TestIdentificationConcurrentCloseReturnsUnavailable(t *testing.T) {
	process, _ := identificationShell(t)
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			for range 100 {
				_ = process.Identification()
			}
		})
	}
	if err := process.CloseIO(); err != nil {
		t.Fatal(err)
	}
	group.Wait()
	if value := process.Identification(); value != (Identification{}) {
		t.Fatalf("closed PTY metadata = %+v", value)
	}
}
