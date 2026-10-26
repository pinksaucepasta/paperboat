//go:build linux || darwin

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/spf13/cobra"
)

// Lane H's round-33 finding: `pb update --from` to 2026.10.25.1 installed, doctor said healthy, and the
// native carrier then restarted every few seconds ("peer network authority is invalid") while every ping and
// exec failed: the service keeps sending a machine that once reported peer endpoints a signed configuration
// field the older build rejects. Releases older than the oldest supported one are refused before anything
// is installed.
func TestUpdateFromRefusesAnOfficialBuildOlderThanTheOldestSupportedRelease(t *testing.T) {
	previousInstall, previousInspect := installUpdateSourceForCommand, inspectUpdateCandidate
	t.Cleanup(func() { installUpdateSourceForCommand, inspectUpdateCandidate = previousInstall, previousInspect })
	candidate := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(candidate, []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	var version string
	inspectUpdateCandidate = func(context.Context, string) (installsource.Source, error) {
		return installsource.Source{Version: version, Distribution: installsource.Custom}, nil
	}
	installs := 0
	installUpdateSourceForCommand = func(context.Context, string, installsource.Source, string, bool, string, string) (string, error) {
		installs++
		return candidate, nil
	}
	run := func() error {
		command := &cobra.Command{Use: "update"}
		command.Flags().Bool("json", true, "")
		return installStagedUpdateSource(command, t.Context(), candidate, true)
	}

	version = "2026.10.25.1"
	err := run()
	if err == nil || installs != 0 {
		t.Fatalf("2026.10.25.1: err=%v installs=%d, want a refusal before any install", err, installs)
	}
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	for _, want := range []string{"2026.10.25.1", "2026.10.26.0", "oldest release this service supports", "Nothing was changed", "pb uninstall"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}
	if failure.kind != commandRejected {
		t.Fatalf("kind=%v, want a refusal", failure.kind)
	}
	result := classifyCLIJSONError(err)
	if result.Code != "update_version_unsupported" || result.StateChanged != false || result.Retryable || !strings.Contains(result.Recovery, "pb uninstall") {
		t.Fatalf("json = %+v", result)
	}

	// The oldest supported release, newer ones, and builds without a release version are installed.
	for _, allowed := range []string{"2026.10.26.0", "2026.10.26.1", "2026.11.2.0", "dev-afaac96d", "dev"} {
		installs, version = 0, allowed
		if err := run(); err != nil && installs == 0 {
			t.Fatalf("%s was refused before the install: %v", allowed, err)
		}
		if installs != 1 {
			t.Fatalf("%s: installs=%d, want 1", allowed, installs)
		}
	}
}
