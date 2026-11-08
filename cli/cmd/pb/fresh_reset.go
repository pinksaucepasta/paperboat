package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/command"
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
	message := "Paperboat fresh enrollment reset was incomplete. Failed: " + strings.Join(steps, ", ") + "." + purgeIncompleteDetail(e.err)
	// Credentials and state are kept only when the services could not be stopped (performFreshEnrollmentCleanup
	// skips the state removal then); a later failed step, such as the SSH configuration, happens after they are gone.
	for _, step := range steps {
		if step == "stop local daemon" || step == "remove system Paperboat runtime" {
			return message + " Credentials and state were retained because service termination could not be confirmed; retry the installer."
		}
	}
	return message + " Retry the installer."
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
				return localArgumentError("pb reset cannot use the enrollment token file: it must be an absolute path without repeated slashes or dot segments, a regular file that only you can read, holding the token")
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
			// Make-before-break: the old machine record stays until pb pair has a working new machine,
			// which then replaces it (ReplaceUserMachine). A reset that fails leaves the record and sign-in.
			replaced, err := rememberReplacedMachineFunc()
			if err != nil {
				return machineReplacementError{step: replaceStepRemember, cause: err}
			}
			if err := freshEnrollmentCleanup(command); err != nil {
				return err
			}
			if err := source.Verify(executable); err != nil {
				return machineReplacementError{step: replaceStepVerifyCLI, cause: err}
			}
			result := map[string]any{"reset": true, "resume": false, "executable": executable, "hostname": hostname, "inbox_preserved": true}
			if replaced != "" {
				result["replaces_machine_id"] = replaced
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
var rememberReplacedMachineFunc = rememberReplacedMachine

// rememberReplacedMachine notes, before anything is removed, which machine record the next pairing of this
// computer replaces; the note survives the wipe (freshEnrollmentPreserved). It returns that ID, or "" when
// this computer has no record (never added, or a note from an earlier failed reset is kept as it is).
func rememberReplacedMachine() (string, error) {
	machineID, err := configuredMachineID()
	if err != nil || machineID == "" {
		return "", nil
	}
	return machineID, bootstrap.SaveReplacement(machineID, time.Now())
}

var replaceUserMachine = func(c *command.Context, replacementID, retiredID string) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	return client.ReplaceUserMachine(c.Context, replacementID, retiredID)
}

// finishMachineReplacement is the cutover of a reset (make-before-break): pairing calls it only after the
// new machine is verified (signed configuration fetched, daemon bound), and only then is the old record
// removed. The server moves the old local address and name to the new machine in the same transaction.
func finishMachineReplacement(c *command.Context, machineID string) error {
	retired := bootstrap.LoadReplacement(time.Now())
	if retired == "" || retired == machineID {
		return nil
	}
	if err := replaceUserMachine(c, machineID, retired); err != nil {
		return machineReplacementError{step: replaceStepCutover, retiredID: retired, cause: err}
	}
	// A note left behind only makes the next pairing ask again, which is a no-op.
	_ = bootstrap.ClearReplacement()
	return nil
}

type replaceStep int

const (
	replaceStepRemember replaceStep = iota + 1
	replaceStepVerifyCLI
	replaceStepCutover
)

// machineReplacementError names the reset or pairing step that failed and what is left, so a failed
// reset never ends in "could not finish the command" (F1-09).
type machineReplacementError struct {
	step      replaceStep
	retiredID string
	cause     error
}

func (e machineReplacementError) Unwrap() error { return e.cause }
func (e machineReplacementError) Error() string {
	switch e.step {
	case replaceStepRemember:
		return "pb reset could not save which machine record the new pairing replaces, so nothing was removed. Check that your user configuration directory is writable, then run the installer again."
	case replaceStepVerifyCLI:
		return "pb reset removed the local setup, but the pb executable it runs from changed during cleanup. The old machine record and its address are kept; run the installer again to pair."
	}
	return "This machine is paired and working, but its old machine record " + e.retiredID + " could not be replaced. Check your connection, then remove the old machine in the dashboard."
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
	preservedInboxes = append(preservedInboxes, freshEnrollmentPreserved(command)...)
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
		func() error { return discardUnfinishedEnrollment(command) },
		func() error { return purgeUserPaperboatState(command, preservedInboxes) })
}

// freshEnrollmentPreserved is what reset keeps until the new pairing works: the note naming the record it
// replaces, and the local diagnostics, so a failure after the wipe can still be investigated (F1-09).
func freshEnrollmentPreserved(command *cobra.Command) []string {
	var paths []string
	if path, err := bootstrap.ReplacementPath(); err == nil {
		paths = append(paths, path)
	}
	if path, err := processDiagnosticsDirectory(command); err == nil {
		paths = append(paths, path)
	}
	return paths
}

// discardUnfinishedEnrollment removes the pairing journal that makes pb pair
// demand a reset. Only pairing reads it (the runtime never does), so it goes
// even when the system runtime could not be removed; otherwise a failed purge
// leaves pair and reset pointing at each other.
func discardUnfinishedEnrollment(command *cobra.Command) error {
	root, _ := command.Flags().GetString("state-root")
	if root == "" {
		var err error
		if root, err = helperconfig.DefaultStateRoot(os.Getenv); err != nil {
			return err
		}
	}
	return bootstrap.ClearResume(root)
}

func performFreshEnrollmentCleanup(inboxErr error, removeDaemon, removeSSHConfig, removeSSHIdentity, purgeRuntime, discardEnrollment, purgeState func() error) error {
	daemonStopped := false
	runtimePurged := false
	cleanupErr := performUninstallCleanup([]uninstallCleanupStep{
		{name: "resolve Paperboat Inbox preservation", run: func() error { return inboxErr }},
		{name: "stop local daemon", run: func() error { err := removeDaemon(); daemonStopped = err == nil; return err }},
		{name: "remove managed OpenSSH configuration", run: removeSSHConfig},
		{name: "remove managed SSH public identity", run: removeSSHIdentity},
		{name: "remove system Paperboat runtime", run: func() error { err := purgeRuntime(); runtimePurged = err == nil; return err }},
		{name: "discard unfinished enrollment", run: discardEnrollment},
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
