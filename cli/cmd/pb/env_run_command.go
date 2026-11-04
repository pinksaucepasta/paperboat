package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
)

// envRunBudget bounds loading the ENV before the command starts; unlike shell
// startup, a first delivery may legitimately take a few seconds.
const envRunBudget = 10 * time.Second

func addEnvironmentRunCommands(root *cobra.Command) {
	root.AddCommand(&cobra.Command{
		Use:   "run -- <command> [args...]",
		Short: "Run a command with your workspace ENV in its environment",
		Long: "Run a command with the ENV of the active workspace merged into its environment, for scripts and CI " +
			"where no shell hook is installed. Values go straight to the child process: never to disk, logs or its " +
			"arguments. Stdin, stdout, stderr, signals and the exit code pass through. If the ENV cannot be loaded, " +
			"the command does not run.",
		Example: "  pb env run -- npm test\n  pb env run --workspace my-team -- ./deploy.sh --prod",
		Args:    commandArgs(cobra.MinimumNArgs(1)),
		RunE:    runEnvironmentCommand,
	})
	root.AddCommand(&cobra.Command{
		Use:   "load",
		Short: "Load your workspace ENV into the current shell",
		Long: "Load the ENV recorded by `pb env hook --install` into the current shell, like sourcing a file. " +
			"This works only through the pb shell function the hook defines in new shells; without the hook " +
			"this command only says how to set it up. Scripts and CI can use `pb env run -- <command>` instead.",
		Example: "  pb env hook --install   # once; then open a new shell\n  pb env load",
		Args:    commandArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			return localArgumentError("`pb env load` needs the shell hook: run `pb env hook --install` and open a new shell, " +
				"or run a single command with `pb env run -- <command>`")
		},
	})
}

func runEnvironmentCommand(command *cobra.Command, args []string) error {
	if command.ArgsLenAtDash() != 0 {
		return localArgumentError("put -- before the command: pb env run -- <command> [args...]")
	}
	values, err := loadEnvironmentForRun(command)
	if err != nil {
		return err
	}
	return runWithEnvironment(command, args, values)
}

func runWithEnvironment(command *cobra.Command, args, values []string) error {
	child := exec.Command(args[0], args[1:]...)
	child.Env = append(os.Environ(), values...) // the last duplicate wins
	child.Stdin, child.Stdout, child.Stderr = command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr()
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := child.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return localArgumentError(fmt.Sprintf("cannot run %s: not found on PATH", args[0]))
		}
		return fmt.Errorf("cannot run %s: %w", args[0], err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-signals:
				_ = child.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	err := child.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
		return exitCodeError{code: code}
	}
	return err
}

// loadEnvironmentForRun resolves the logged-in account and active workspace
// and asks the runtime through the same path as the shell hook.
func loadEnvironmentForRun(command *cobra.Command) ([]string, error) {
	fail := func(reason string) error {
		return &envCommandFailure{message: "Paperboat ENV not loaded: " + reason + "; the command did not run."}
	}
	cfg, err := workspaceConfig(configPathFlag(command), "")
	if err != nil {
		return nil, err
	}
	account, err := workspaceAccountID(cfg)
	if errors.Is(err, config.ErrNoCredentials) || err == nil && account == "" {
		return nil, fail("log in first with `pb login`")
	}
	if err != nil {
		return nil, fmt.Errorf("read the logged-in account: %w", err)
	}
	workspace, err := effectiveWorkspace(command)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(command.Context(), envRunBudget)
	defer cancel()
	result := loadEnvironmentHook(ctx, command, envHookRecord{Schema: envHookSchema, ServerURL: cfg.ServerURL, AccountID: account, WorkspaceID: workspace})
	switch {
	case result.quiet:
		return nil, fail("this machine belongs to another Paperboat account")
	case result.reason != "":
		return nil, fail(result.reason)
	}
	values := make([]string, 0, len(result.values))
	for _, entry := range result.values {
		if name, _, ok := strings.Cut(entry, "="); ok && envHookName(name) {
			values = append(values, entry)
		}
	}
	return values, nil
}
