//go:build linux

package prompt

import (
	"bytes"
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
	var output bytes.Buffer
	_, err = Text(TextOptions{Context: ctx, Stdin: slave, Output: &output})
	if !errors.Is(err, tea.ErrProgramKilled) || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("run or context cause lost: %v", err)
	}
	if output.Len() != 0 {
		t.Fatal("canceled prompt wrote terminal output")
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

func TestConfirmationAndSecretCanceledBeforeStart(t *testing.T) {
	for _, kind := range []string{"confirmation", "secret"} {
		for _, custom := range []bool{false, true} {
			name := kind + "/ordinary"
			if custom {
				name = kind + "/custom"
			}
			t.Run(name, func(t *testing.T) {
				master, slave, err := pty.Open()
				if err != nil {
					t.Fatal(err)
				}
				defer master.Close()
				defer slave.Close()
				ctx, cancel := context.WithCancelCause(context.Background())
				var cause error
				if custom {
					cause = errors.New("private cancellation cause")
				}
				cancel(cause)
				var output bytes.Buffer
				message := "run confirmation failed"
				if kind == "confirmation" {
					var yes bool
					yes, err = Confirm(ConfirmOptions{Context: ctx, Stdin: slave, Output: &output})
					if yes {
						t.Fatal("canceled confirmation accepted")
					}
				} else {
					message = "run hidden input failed"
					var value []byte
					value, err = Secret(SecretOptions{Context: ctx, Stdin: slave, Output: &output, Initial: "private initial input"})
					if len(value) != 0 {
						t.Fatal("canceled secret returned input")
					}
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				if custom {
					if !errors.Is(err, cause) || !errors.Is(err, tea.ErrProgramKilled) || err.Error() != message {
						t.Fatalf("custom cancellation contract lost: %v", err)
					}
				} else if err != context.Canceled {
					t.Fatalf("ordinary cancellation = %v", err)
				}
				if output.Len() != 0 {
					t.Fatal("canceled prompt wrote terminal output")
				}
			})
		}
	}
}
