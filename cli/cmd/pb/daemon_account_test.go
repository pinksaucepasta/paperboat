package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/daemoncmd"
	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
)

func TestDaemonAccountMismatchIsExplainedToThePerson(t *testing.T) {
	message := daemonrpc.AccountMismatchMessage("owner@example.test")
	for name, err := range map[string]error{
		"direct": daemoncmd.ErrDaemonAccountMismatch{Message: message},
		"joined": errors.Join(daemoncmd.ErrDaemonAccountMismatch{Message: message}, nil),
	} {
		rejected := rejectDaemonAccount(err)
		failure := classifyCommandFailure(rejected)
		got := commandFailureMessage(rejected, failure)
		if failure.kind != commandRejected || got != message || !strings.Contains(got, "owner@example.test") || !strings.Contains(got, "pb reset") {
			t.Fatalf("%s: kind=%v message=%q", name, failure.kind, got)
		}
	}
	if err := errors.New("other"); rejectDaemonAccount(err) != err {
		t.Fatal("unrelated errors must pass through untouched")
	}
}
