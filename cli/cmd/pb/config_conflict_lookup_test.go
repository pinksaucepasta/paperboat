package main

import (
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// A comparison of a path with no conflict once printed only the generic "could not finish the command".
func TestConfigLookupRefusalsReachTheUser(t *testing.T) {
	item := api.ConfigSyncEnvironmentState{Alias: "dadape", MachineID: "machine_1", EnvironmentID: "env_1"}
	_, conflictErr := findConfigConflict(item, "/home/u/.bashrc")
	_, environmentErr := selectConfigEnvironments([]api.ConfigSyncEnvironmentState{item}, "missing")
	for want, err := range map[string]error{"no configuration conflict": conflictErr, "was not found": environmentErr} {
		message := commandFailureMessage(err, classifyCommandFailure(err))
		if !strings.Contains(message, want) || !strings.Contains(message, "pb config status") {
			t.Fatalf("message = %q, want %q and the next step", message, want)
		}
	}
}

// The refusal once ended with the whole `Usage:` help, as if the arguments were wrong.
func TestConfigRefusalsDoNotPrintCommandHelp(t *testing.T) {
	_, err := findConfigConflict(api.ConfigSyncEnvironmentState{Alias: "dadape"}, "/x")
	if printsUsage(err) || !printsUsage(usageError{publicMessage: "bad flag"}) {
		t.Fatalf("printsUsage: refusal=%v, misuse=%v", printsUsage(err), printsUsage(usageError{publicMessage: "bad flag"}))
	}
}
