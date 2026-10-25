package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

func TestInstallSuppliesCurrentExecutableAndPropagatesFailure(t *testing.T) {
	oldSource, oldInstall := runningInstallSource, installSuppliedExecutable
	t.Cleanup(func() { runningInstallSource, installSuppliedExecutable = oldSource, oldInstall })
	source := installsource.Source{Version: "custom-test", Distribution: installsource.Custom, SHA256: "source-digest"}
	runningInstallSource = func() (string, installsource.Source, error) { return "/supplied/pb", source, nil }
	var failure error
	installSuppliedExecutable = func(ctx context.Context, path string, got installsource.Source, directory, _ string) (string, error) {
		if path != "/supplied/pb" || got != source || directory != "/commands" {
			t.Fatal("supplied executable identity changed")
		}
		if deadline, ok := ctx.Deadline(); !ok {
			t.Fatal("installation has no deadline")
		} else if remaining := time.Until(deadline); remaining < suppliedInstallTimeout(runtime.GOOS)-time.Second {
			t.Fatal("caller truncated installation recovery deadline", remaining)
		}
		return "/commands/pb", failure
	}
	command := platformInstallCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetArgs([]string{"--json", "--install-dir", "/commands"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Data struct {
			Executable       string `json:"executable"`
			AutomaticUpdates bool   `json:"automatic_update_checks"`
			Enrollment       string `json:"enrollment"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Executable != "/commands/pb" || result.Data.AutomaticUpdates || result.Data.Enrollment != "unchanged" {
		t.Fatalf("unexpected result: %s", out.String())
	}
	failure = errors.New("installation failed")
	out.Reset()
	if err := command.Execute(); !errors.Is(err, failure) {
		t.Fatal("install failure not returned", err)
	}
	if out.Len() != 0 {
		t.Fatal("failed installation reported success")
	}
	for _, flag := range []string{"skip-verification", "skip-download", "fresh", "source", "source-version"} {
		if command.Flags().Lookup(flag) != nil {
			t.Fatal("unexpected install bypass flag", flag)
		}
	}
}

func TestSuppliedInstallTimeoutPreservesNativeRecoveryBudget(t *testing.T) {
	if got := suppliedInstallTimeout("windows"); got != elevation.RuntimeInstallDuration || got <= elevation.RuntimeInstallRecoveryDuration {
		t.Fatal("Windows install caller must allow recovery and local installation", got)
	}
	for _, platform := range []string{"linux", "darwin"} {
		if got := suppliedInstallTimeout(platform); got != 3*time.Minute {
			t.Fatal("Unix install deadline changed", platform, got)
		}
	}
}

// A refused executable must reach the user with its reason and fix, not as the
// generic "could not finish the command" text.
func TestInstallShowsWhyTheSuppliedExecutableWasRefused(t *testing.T) {
	oldSource, oldInstall := runningInstallSource, installSuppliedExecutable
	t.Cleanup(func() { runningInstallSource, installSuppliedExecutable = oldSource, oldInstall })
	runningInstallSource = func() (string, installsource.Source, error) {
		return "/tmp/pb", installsource.Source{Version: "custom-test", Distribution: installsource.Custom}, nil
	}
	refusal := &hostinstall.SuppliedExecutableError{Path: "/tmp/pb", Reason: "is writable by other users (mode 0775)", Fix: "run `chmod go-w /tmp/pb`"}
	installSuppliedExecutable = func(context.Context, string, installsource.Source, string, string) (string, error) {
		return "", refusal
	}
	command := platformInstallCommand()
	command.SetArgs([]string{})
	err := command.Execute()
	failure := classifyCommandFailure(err)
	if err == nil || !strings.Contains(commandFailureMessage(err, failure), "chmod go-w /tmp/pb") {
		t.Fatalf("message=%q err=%v", commandFailureMessage(err, failure), err)
	}
	// The refusal is about the file, not the arguments: no usage text after it (Lane W, stg-m1).
	if failure.kind == commandUsage {
		t.Fatal("a refused executable is reported as a usage error and prints the usage text")
	}
}
