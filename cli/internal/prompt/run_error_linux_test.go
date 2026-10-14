//go:build linux

package prompt

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

func TestTextPreservesCustomCancellationCause(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	cause := errors.New("private cancellation cause")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	_, err = Text(TextOptions{Context: ctx, Stdin: slave, Output: io.Discard})
	if !errors.Is(err, tea.ErrProgramKilled) || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("run or context cause lost: %v", err)
	}
	if err.Error() != "run input failed" || strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("run error was not safely summarized: %q", err)
	}
}

func TestTextOrdinaryContextCancellationRemainsQuiet(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Text(TextOptions{Context: ctx, Stdin: slave, Output: io.Discard})
	if !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
		t.Fatalf("ordinary cancellation = %v, want context.Canceled", err)
	}
}
