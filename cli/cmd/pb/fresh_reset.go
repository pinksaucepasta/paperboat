package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/spf13/cobra"
)

type freshResetCleanupError struct{ err error }

func (e freshResetCleanupError) Error() string {
	steps := uninstallFailureStepNames(e.err)
	if len(steps) == 0 {
		return "Paperboat fresh enrollment reset was incomplete. Retry the installer."
	}
	return "Paperboat fresh enrollment reset was incomplete. Failed: " + strings.Join(steps, ", ") + ". Credentials and state were retained when service termination could not be confirmed; retry the installer."
}
func (e freshResetCleanupError) Unwrap() error { return e.err }

func freshEnrollmentResetCommand() *cobra.Command {
	command := &cobra.Command{
		Use:           "reset",
		Short:         "Remove the current Paperboat setup before fresh enrollment",
		Args:          commandArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(command *cobra.Command, _ []string) error {
			hostname, err := os.Hostname()
			if err != nil || strings.TrimSpace(hostname) == "" {
				return errors.New("could not resolve this machine hostname")
			}
			confirmation, _ := command.Flags().GetString("confirmation")
			confirmedHostname, _ := command.Flags().GetString("hostname")
			if strings.TrimSpace(confirmation) != "RESET PAPERBOAT" || strings.TrimSpace(confirmedHostname) != hostname {
				return invocationError(errors.New("fresh enrollment reset confirmation did not match"))
			}
			tokenFile, _ := command.Flags().GetString("enrollment-token-file")
			if tokenFile == "" {
				return invocationError(errors.New("fresh enrollment reset requires --enrollment-token-file"))
			}
			token, err := bootstrap.ReadEnrollmentTokenFile(tokenFile)
			if err != nil {
				return err
			}
			stateRoot, _ := command.Flags().GetString("state-root")
			if stateRoot == "" {
				stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
				if err != nil {
					return err
				}
			}
			resume, err := bootstrap.ResumeMatchesEnrollmentToken(stateRoot, token)
			if err != nil {
				return fmt.Errorf("classify fresh enrollment retry: %w", err)
			}
			asJSON, _ := command.Flags().GetBool("json")
			if resume {
				executable, err := resolveFreshEnrollmentExecutable()
				if err != nil {
					return fmt.Errorf("resolve existing enrollment executable: %w", err)
				}
				result := map[string]any{"reset": false, "resume": true, "executable": executable, "hostname": hostname, "inbox_preserved": true}
				if asJSON {
					return writeCLIJSON(command.OutOrStdout(), result)
				}
				_, err = fmt.Fprintln(command.OutOrStdout(), "Matching enrollment recovery was preserved.")
				return err
			}
			if err := freshEnrollmentCleanup(command); err != nil {
				return err
			}
			result := map[string]any{"reset": true, "resume": false, "executable": "", "hostname": hostname, "inbox_preserved": true}
			if asJSON {
				return writeCLIJSON(command.OutOrStdout(), result)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), "Paperboat setup was removed. The Paperboat Inbox was preserved.")
			return err
		},
	}
	command.Flags().String("confirmation", "", "exact confirmation phrase: RESET PAPERBOAT")
	command.Flags().String("hostname", "", "exact current hostname confirmation")
	command.Flags().String("enrollment-token-file", "", "absolute owner-only dashboard enrollment token file")
	command.Flags().String("state-root", "", "additional Paperboat runtime state directory to remove")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

var freshEnrollmentCleanup = runFreshEnrollmentCleanup
var resolveFreshEnrollmentExecutable = existingFreshEnrollmentExecutable

func runFreshEnrollmentCleanup(command *cobra.Command) error {
	preservedInboxes, inboxErr := uninstallInboxPaths(command)
	executable, executableErr := os.Executable()
	home, homeErr := os.UserHomeDir()
	return performFreshEnrollmentCleanup(inboxErr,
		func() error {
			if executableErr != nil {
				return executableErr
			}
			ctx, cancel := context.WithTimeout(context.WithoutCancel(command.Context()), 30*time.Second)
			defer cancel()
			return removeLocalDaemonService(ctx, executable)
		}, func() error {
			if homeErr != nil {
				return homeErr
			}
			_, err := managedssh.UninstallOpenSSHConfig(home, uint32(os.Geteuid()))
			return err
		}, func() error {
			if homeErr != nil {
				return homeErr
			}
			return managedssh.UninstallManagedIdentityPublicKey(home, uint32(os.Geteuid()))
		}, func() error { return freshEnrollmentRuntimePurge(command) },
		func() error { return purgeUserPaperboatState(command, preservedInboxes) })
}

func performFreshEnrollmentCleanup(inboxErr error, removeDaemon, removeSSHConfig, removeSSHIdentity, purgeRuntime, purgeState func() error) error {
	daemonStopped := false
	runtimePurged := false
	cleanupErr := performUninstallCleanup([]uninstallCleanupStep{
		{name: "resolve Paperboat Inbox preservation", run: func() error { return inboxErr }},
		{name: "stop local daemon", run: func() error { err := removeDaemon(); daemonStopped = err == nil; return err }},
		{name: "remove managed OpenSSH configuration", run: removeSSHConfig},
		{name: "remove managed SSH public identity", run: removeSSHIdentity},
		{name: "remove system Paperboat runtime", run: func() error { err := purgeRuntime(); runtimePurged = err == nil; return err }},
		{name: "remove user Paperboat state", run: func() error {
			if inboxErr != nil {
				return errors.New("Paperboat Inbox location could not be proven; state removal was skipped")
			}
			if !daemonStopped || !runtimePurged {
				return errors.New("Paperboat service termination was not confirmed; credentials and state were retained for a safe retry")
			}
			return purgeState()
		}},
	})
	if cleanupErr != nil {
		return freshResetCleanupError{err: cleanupErr}
	}
	return nil
}
