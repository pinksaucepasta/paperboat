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

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/cli/internal/windows/elevation"
)

func TestInstallSuppliesCurrentExecutableAndPropagatesFailure(t *testing.T) {
	source := installsource.Source{Version: "custom-test", Distribution: installsource.Custom, SHA256: "source-digest"}
	var failure error
	installExecutable := func(ctx context.Context, path string, got installsource.Source, directory, _ string) (string, error) {
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
	command.SetContext(withInstallDependencies(t.Context(), func(d *installDependencies) {
		d.runningSource = func() (string, installsource.Source, error) { return "/supplied/pb", source, nil }
		d.installExecutable = installExecutable
	}))
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

// A reinstall on a revoked computer printed "Enrollment is unchanged" while `pb status` said to sign in again
// (2026-11-26). The message says whether the computer is signed in and what to run next.
func TestInstallSaysWhenTheComputerMustSignIn(t *testing.T) {
	source := installsource.Source{Version: "custom-test", Distribution: installsource.Custom, SHA256: "source-digest"}
	for _, awaiting := range []bool{true, false} {
		command := platformInstallCommand()
		command.SetContext(withInstallDependencies(t.Context(), func(d *installDependencies) {
			d.runningSource = func() (string, installsource.Source, error) { return "/supplied/pb", source, nil }
			d.installExecutable = func(context.Context, string, installsource.Source, string, string) (string, error) {
				return "/commands/pb", nil
			}
			d.awaitingEnrollment = func(context.Context) bool { return awaiting }
		}))
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"--install-dir", "/commands"})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "`pb login`") != awaiting || strings.Contains(out.String(), "Enrollment is unchanged") {
			t.Fatalf("awaiting=%t: %q", awaiting, out.String())
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
	refusal := &hostinstall.SuppliedExecutableError{Path: "/tmp/pb", Reason: "is writable by other users (mode 0775)", Fix: "run `chmod go-w /tmp/pb`"}
	command := platformInstallCommand()
	command.SetContext(withInstallDependencies(t.Context(), func(d *installDependencies) {
		d.runningSource = func() (string, installsource.Source, error) {
			return "/tmp/pb", installsource.Source{Version: "custom-test", Distribution: installsource.Custom}, nil
		}
		d.installExecutable = func(context.Context, string, installsource.Source, string, string) (string, error) {
			return "", refusal
		}
	}))
	command.SetArgs([]string{})
	err := command.Execute()
	failure := classifyCommandFailure(err)
	if err == nil || !strings.Contains(commandFailureMessage(err, failure), "chmod go-w /tmp/pb") {
		t.Fatalf("message=%q err=%v", commandFailureMessage(err, failure), err)
	}
	// The refusal is about the file, not the arguments: no usage text after it (stg-m1).
	if failure.kind == commandUsage {
		t.Fatal("a refused executable is reported as a usage error and prints the usage text")
	}
}

// L30: a recipe install without root or passwordless sudo is refused by pb install itself, before anything is
// installed, with pb pair's message in full on stderr (the installer runs pb install --json first).
func TestInstallRefusesARecipeWithoutRootBeforeInstalling(t *testing.T) {
	t.Setenv("PB_RECIPE", "pbr_"+strings.Repeat("a", 63))
	noSystemd := hostruntimecmd.NoSystemd
	t.Cleanup(func() { hostruntimecmd.NoSystemd = noSystemd })
	hostruntimecmd.NoSystemd = func() bool { return false }
	command := platformInstallCommand()
	ctx := withInstallDependencies(t.Context(), func(d *installDependencies) {
		d.runningSource = func() (string, installsource.Source, error) {
			t.Fatal("pb install went on after the refusal")
			return "", installsource.Source{}, nil
		}
	})
	command.SetContext(withAuthDependencies(ctx, func(d *authDependencies) {
		d.recipeInstallPreflight = func(context.Context) error { return errors.New("needs root") }
	}))
	var stderr bytes.Buffer
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&stderr)
	command.SetArgs([]string{"--json"})
	err := command.Execute()
	if err == nil || err.Error() != hostruntimecmd.RecipeNeedsRootMessage || !strings.Contains(stderr.String(), hostruntimecmd.RecipeNeedsRootMessage) {
		t.Fatalf("err = %v, stderr = %q", err, stderr.String())
	}
}
