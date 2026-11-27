package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

// terminal runs pbh as main does with Sentry off and returns the exit code and everything the operator would see on
// stderr: the diagnostic writer and the process's own stderr.
func terminal(t *testing.T, args ...string) (int, string) {
	t.Helper()
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "false")
	reporter, err := reporting.New("paperboat-selfhost")
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	file, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	original := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = original }()
	code := execute(context.Background(), reporter, args, io.Discard, file)
	seen, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(seen)
}

func installationState(t *testing.T, capability string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := selfhost.Initialize(dir, selfhost.Config{Name: "n", EndpointHost: "203.0.113.10", Listen: "127.0.0.1:38443", Components: []selfhost.Component{
		{Capability: capability, TCPPort: 7274, QUICPort: 7275, Region: "selfhost", FailureDomain: "n"},
	}}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMistypedCommandLineIsAUsageError(t *testing.T) {
	t.Setenv("PAPERBOAT_DEBUG", "")
	var cases [][]string
	for _, command := range [][]string{{"install"}, {"selfhost", "code"}, {"serve"}, {"logs"}, {"restart"}, {"update"}, {"update-apply"}, {"doctor"}, {"uninstall"}} {
		cases = append(cases, append(append([]string{}, command...), "--bogus"))
	}
	for _, args := range cases {
		name := strings.Join(args[:len(args)-1], " ")
		code, seen := terminal(t, args...)
		if want := "pbh: " + name + ": unknown flag --bogus; run pbh " + name + " --help\n"; code != 2 || seen != want {
			t.Errorf("%v: exit %d, stderr %q, want exit 2 and %q", args, code, seen, want)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"logs", "--lines", "many"}, `invalid value "many" for flag -lines`},
		{[]string{"restart", "--state-dir"}, "flag needs an argument: -state-dir"},
		{[]string{"install", "--relay=perhaps"}, `invalid boolean value "perhaps" for -relay`},
		{[]string{"logs", "--lines", "0"}, "--lines must be 1 to"},
		{[]string{"logs", "relay", "edge"}, "at most one component"},
		{[]string{"install", "--relay", "--edge"}, "choose only one of"},
		{[]string{"uninstall", "--remove-data"}, "requires both"},
		{[]string{"doctor", "extra"}, "unexpected doctor arguments"},
		{[]string{"status", "--bogus"}, "status accepts no arguments"},
		{[]string{"--bogus"}, "unknown flag --bogus; run pbh help"},
		{[]string{"bogus"}, "unknown command; run pbh help"},
		{nil, "pbh help lists every command"},
	} {
		code, seen := terminal(t, c.args...)
		if code != 2 || !strings.HasPrefix(seen, "pbh: ") || !strings.Contains(seen, c.want) || strings.Count(seen, "\n") != 1 || strings.Contains(seen, "internal") {
			t.Errorf("%v: exit %d, stderr %q, want exit 2 and one line with %q", c.args, code, seen, c.want)
		}
	}
}

func TestRefusalPrintsOnlyItsMessage(t *testing.T) {
	t.Setenv("PAPERBOAT_DEBUG", "")
	// pbh logs on an installation without the named component refuses with an authored message and no cause.
	dir := installationState(t, "relay")
	code, seen := terminal(t, "logs", "--state-dir", dir, "edge")
	if want := "pbh: this installation does not run the edge; it runs relay\n"; code != 1 || seen != want {
		t.Fatalf("exit %d, stderr %q, want exit 1 and %q", code, seen, want)
	}
}

func TestFailureOnATerminalPrintsNoStructuredRecord(t *testing.T) {
	t.Setenv("PAPERBOAT_DEBUG", "")
	missing := filepath.Join(t.TempDir(), "missing")
	code, seen := terminal(t, "selfhost", "code", "--state-dir", missing)
	if code != 1 || !strings.HasPrefix(seen, "pbh: ") || !strings.Contains(seen, "support reference ") || strings.Count(seen, "\n") != 1 || strings.Contains(seen, `"schema"`) {
		t.Fatalf("exit %d, stderr %q", code, seen)
	}
	t.Setenv("PAPERBOAT_DEBUG", "1")
	if _, seen = terminal(t, "selfhost", "code", "--state-dir", missing); !strings.Contains(seen, `"schema":"paperboat.edge_event.v1"`) {
		t.Fatalf("debug run lacks the structured record: %q", seen)
	}
}

// The service's stderr is the journal, which keeps the structured record.
func TestServiceKeepsStructuredRecordInItsJournal(t *testing.T) {
	for args, want := range map[string]bool{"serve": true, "update-apply": true, "install": false, "update": false, "": false} {
		if got := serviceCommand(strings.Fields(args)); got != want {
			t.Errorf("serviceCommand(%q) = %v", args, got)
		}
	}
}

// A command that re-runs itself with sudo adds nothing to what the elevated process printed, and keeps its status.
func TestElevatedRefusalIsNotReportedTwice(t *testing.T) {
	t.Setenv("PAPERBOAT_DEBUG", "")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\necho 'pbh: refused by the elevated run' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	previous := isRoot
	isRoot = func() bool { return false }
	t.Cleanup(func() { isRoot = previous })
	code, seen := terminal(t, "uninstall")
	if want := "pbh: refused by the elevated run\n"; code != 2 || seen != want {
		t.Fatalf("exit %d, stderr %q, want exit 2 and %q", code, seen, want)
	}
}
