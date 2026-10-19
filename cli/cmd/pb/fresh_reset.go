package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
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
			executable, source, err := installsource.Current()
			if err != nil {
				return fmt.Errorf("verify reset CLI before cleanup: %w", err)
			}
			retired, err := removeOwnMachineRecordFunc(command)
			if err != nil {
				return err
			}
			if err := freshEnrollmentCleanup(command); err != nil {
				return err
			}
			if err := source.Verify(executable); err != nil {
				return fmt.Errorf("reset CLI did not survive cleanup: %w", err)
			}
			result := map[string]any{"reset": true, "resume": false, "executable": executable, "hostname": hostname, "inbox_preserved": true}
			if retired != "" {
				result["retired_machine_id"] = retired
			}
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
var removeOwnMachineRecordFunc = removeOwnMachineRecord

// removeOwnMachineRecord removes this computer's current machine record
// (the normal machine removal: its ENV access, peer identity, SSH host keys and
// sessions are released; its Activity history stays) before the local credentials
// are deleted, so the fresh enrollment takes the exact name instead of a numbered
// one beside a dead record. It returns the removed machine ID, or "" when there is
// nothing to remove (never enrolled, signed out, or already removed). The server
// revokes the machine's own CLI session with the record, so this is the last
// server call reset makes.
func removeOwnMachineRecord(command *cobra.Command) (string, error) {
	machineID, err := configuredMachineID()
	if err != nil || machineID == "" {
		return "", nil
	}
	ctx := actionContext(command, nil)
	client, err := backendClient(ctx)
	switch {
	case err == nil:
	case errors.Is(err, config.ErrNoCredentials), errors.Is(err, commandRejection{reason: commandRejectMissingServer}):
		return "", nil
	default:
		return "", commandRejection{reason: commandRejectResetRetire}
	}
	err = client.DeleteUserMachine(ctx.Context, machineID)
	switch {
	case err == nil:
		return machineID, nil
	case api.IsNotFound(err), errors.Is(err, api.ErrUnauthenticated):
		return "", nil
	}
	return "", commandRejection{reason: commandRejectResetRetire}
}

var resolveFreshEnrollmentExecutable = existingFreshEnrollmentExecutable

func runFreshEnrollmentCleanup(command *cobra.Command) error {
	preservedInboxes, inboxErr := uninstallInboxPaths(command)
	executable, executableErr := os.Executable()
	home, homeErr := os.UserHomeDir()
	if executableErr == nil {
		preservedInboxes = append(preservedInboxes, executable)
	}
	if homeErr == nil {
		// The privileged reset scope retains its canonical CLI slot. Keep its
		// public command link while deleting credentials and user runtime state.
		preservedInboxes = append(preservedInboxes, filepath.Join(home, ".local", "bin", "pb"))
	}
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
