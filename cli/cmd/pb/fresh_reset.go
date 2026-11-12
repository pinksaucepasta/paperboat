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
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/daemoncmd"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/spf13/cobra"
)

type freshResetCleanupError struct{ err error }

// errFreshResetStateRetained marks a reset that skipped removing user state (Inbox location or service termination unproven).
var errFreshResetStateRetained = errors.New("user state was retained")

func (e freshResetCleanupError) Error() string {
	steps := uninstallFailureStepNames(e.err)
	if len(steps) == 0 {
		return "Paperboat fresh enrollment reset was incomplete. Retry the installer."
	}
	message := "Paperboat fresh enrollment reset was incomplete. Failed: " + strings.Join(steps, ", ") + "." + purgeIncompleteDetail(e.err)
	// Credentials and state are kept exactly when performFreshEnrollmentCleanup skipped the state removal; a later
	// failed step, such as the SSH configuration, happens after they are gone.
	if errors.Is(e.err, errFreshResetStateRetained) {
		return message + " Credentials and state were retained so a retry can find them; retry the installer."
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
				return localArgumentError("pb reset cannot use the enrollment token file: it must be an absolute path without repeated slashes, a regular file (no links) only you can read (mode 0600; owner-only ACL on Windows), at most 512 bytes, holding the token")
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
// computer replaces; the note survives the wipe (freshEnrollmentPreserved) and keeps the records earlier
// resets and failed pairings left behind. It returns that ID, or "" when this computer has no record
// (never added, or a note from an earlier failed reset is kept as it is).
func rememberReplacedMachine() (string, error) {
	machineID, err := configuredMachineID()
	if err != nil || machineID == "" {
		return "", nil
	}
	return machineID, bootstrap.SaveReplacement(machineID, false, time.Now())
}

var replaceUserMachine = func(c *command.Context, replacementID, retiredID string) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	return client.ReplaceUserMachine(c.Context, replacementID, retiredID)
}

// finishMachineReplacement is the cutover of a reset (make-before-break): pairing calls it only after the
// new machine is verified (signed configuration fetched, daemon bound), and only then are the records this
// computer left behind removed. The server moves the old local address and name to the new machine in the
// same transaction; the newest record goes first, so the oldest one's address and name are the ones kept.
func finishMachineReplacement(c *command.Context, machineID string) error {
	left := bootstrap.LoadReplacements(time.Now())
	for index := len(left) - 1; index >= 0; index-- {
		retired := left[index].MachineID
		if retired == machineID {
			continue
		}
		if err := replaceUserMachine(c, machineID, retired); err != nil {
			return machineReplacementError{step: replaceStepCutover, retiredID: retired, cause: err}
		}
	}
	// A note left behind only makes the next pairing ask again, which is a no-op.
	_ = bootstrap.ClearReplacement()
	return nil
}

// pairingFailed runs when a pairing ended in failure. before and after are this computer's machine record
// before and after the attempt: a record that appeared was accepted by the server, but its local
// installation did not finish. It is noted as unfinished, and it takes over the unfinished records of
// earlier attempts right away (it holds a session of the account), so a computer whose pairing keeps
// failing has one record in the account, not one per attempt. Records that worked stay until a pairing
// succeeds (finishMachineReplacement). Failures here are ignored: the note keeps every record for the
// next pairing, and the pairing's own error is the one to report.
func pairingFailed(c *command.Context, before, after string) {
	if before != "" || after == "" {
		return
	}
	now := time.Now()
	left := bootstrap.LoadReplacements(now)
	for index := len(left) - 1; index >= 0; index-- {
		if retired := left[index].MachineID; left[index].Unfinished && retired != after && replaceUserMachine(c, after, retired) == nil {
			_ = bootstrap.ForgetReplacement(retired, now)
		}
	}
	_ = bootstrap.SaveReplacement(after, true, now)
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
	var apiErr *api.APIError
	if errors.As(e.cause, &apiErr) {
		switch apiErr.Code {
		case "user_machine_already_replaced":
			return "Another machine already replaced " + e.retiredID + ". This machine is paired and working; see `pb machine list`."
		case "user_machine_replace_team_mismatch":
			return "This machine is paired and working. The old machine record " + e.retiredID + " belongs to a different team or account, so it was left as it is; see `pb machine list`."
		}
	}
	if apiErr != nil && apiErr.Code != "" { // the server answered: its code says why, and a connection check would not help
		return "This machine is paired and working, but its old machine record " + e.retiredID + " could not be replaced (" + apiErr.Code + "). Remove the old machine in the dashboard."
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
			ctx, cancel := context.WithTimeout(context.WithoutCancel(command.Context()), daemoncmd.ServiceElevationTimeout())
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
				return fmt.Errorf("%w: Paperboat Inbox location could not be proven; state removal was skipped", errFreshResetStateRetained)
			}
			if !daemonStopped || !runtimePurged {
				return fmt.Errorf("%w: Paperboat service termination was not confirmed", errFreshResetStateRetained)
			}
			return purgeState()
		}},
	})
	if cleanupErr != nil {
		return freshResetCleanupError{err: cleanupErr}
	}
	return nil
}
