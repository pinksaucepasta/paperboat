package main

import (
	"encoding/json"
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

// pb config status once dropped the skipped paths the server holds, so a link or an unsafe file was never named.
func TestConfigStatusNamesSkippedPaths(t *testing.T) {
	var item api.ConfigSyncEnvironmentState
	if err := json.Unmarshal([]byte(`{"alias":"m","skipped":[{"path":"cfg/app/link.txt","reason":"unsupported_file_type"}]}`), &item); err != nil {
		t.Fatal(err)
	}
	lines := configSkippedLines(item)
	if len(lines) != 1 || lines[0] != "  skipped cfg/app/link.txt: unsupported file type" {
		t.Fatalf("lines = %q", lines)
	}
	if encoded, _ := json.Marshal(item); !strings.Contains(string(encoded), `"skipped":[{"path":"cfg/app/link.txt"`) {
		t.Fatalf("--json drops the skipped paths: %s", encoded)
	}
}

// A machine enrolled again with an old file on disk reports a conflict; status once printed only the word "conflict".
func TestConfigStatusNamesConflictsAndHowToResolve(t *testing.T) {
	var item api.ConfigSyncEnvironmentState
	if err := json.Unmarshal([]byte(`{"alias":"rx41n-l","state":"conflict","conflicts":[{"path":"cfg/settings.json","reason":"concurrent_update"}]}`), &item); err != nil {
		t.Fatal(err)
	}
	lines := configConflictLines(item)
	want := "  conflict cfg/settings.json: concurrent update; see both versions with `pb config conflict compare rx41n-l cfg/settings.json`, then keep one with `pb config conflict resolve rx41n-l cfg/settings.json --keep machine` or `--keep repository`"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("lines = %q", lines)
	}
}
