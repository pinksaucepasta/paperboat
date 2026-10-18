package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func TestUnknownAndAmbiguousMachinesAreExplained(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: %q", resolver.ErrMachineNotFound, "nosuchmachine"), "No machine has that name or ID. Run `pb machine list`"},
		{fmt.Errorf("%w: %q matches machine IDs a, b; use an exact ID", resolver.ErrMachineAmbiguous, "hp"), "More than one machine has that name"},
	} {
		failure := classifyCommandFailure(tc.err)
		if failure.kind != commandRejected || !strings.Contains(commandFailureMessage(tc.err, failure), tc.want) {
			t.Fatalf("kind=%v message=%q err=%v", failure.kind, commandFailureMessage(tc.err, failure), tc.err)
		}
	}
}
