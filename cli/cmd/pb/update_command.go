package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/autoupdate"
	service "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/selfupdate"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type updateResult struct {
	PreviousVersion   string `json:"previous_version"`
	Version           string `json:"version"`
	CLIUpdated        bool   `json:"cli_updated"`
	RuntimeUpdated    bool   `json:"runtime_updated"`
	ActivationPending bool   `json:"activation_pending,omitempty"`
	// HealthDeadline: when the pending activation's health check ends, if the updater knows it.
	HealthDeadline time.Time `json:"health_deadline,omitempty"`
}

func updateCommand() *cobra.Command {
	command := &cobra.Command{Use: "update", Short: "Update pb from the signed Paperboat release", Args: commandArgs(cobra.NoArgs), RunE: actionUpdate}
	command.Flags().Bool("json", false, "print JSON")
	command.Flags().Bool("force", false, "install now, allowing interruption of work that cannot be preserved")
	command.Flags().String("from", "", "install an executable, package, or verified offline bundle from a local path or HTTPS URL")
	check := &cobra.Command{Use: "check", Short: "Check the signed Paperboat release without installing it", Args: commandArgs(cobra.NoArgs), RunE: actionUpdateCheck}
	check.Flags().Bool("json", false, "print JSON")
	status := &cobra.Command{Use: "status", Short: "Show installed Paperboat update state", Args: commandArgs(cobra.NoArgs), RunE: actionUpdateStatus}
	status.Flags().Bool("json", false, "print JSON")
	command.Flags().String("approve", "", "install the exact downloaded candidate ID after reviewing it")
	download := &cobra.Command{Use: "download", Short: "Download and verify an update without installing it", Args: commandArgs(cobra.NoArgs), RunE: actionUpdateDownload}
	download.Flags().Bool("json", false, "print JSON")
	command.AddCommand(check, status, download, updateSettingsCommand(), updateBundleCommand())
	return command
}

type updateCheckResult struct {
	InstalledVersion string `json:"installed_version"`
	LatestVersion    string `json:"latest_version"`
	UpdateAvailable  bool   `json:"update_available"`
	Verified         bool   `json:"verified"`
}

const updateDaemonSnapshotTimeout = 2 * time.Second

// readLocalDaemonSnapshot is deliberately a direct read-only probe. Update
// status must report whether the authenticated daemon is actually present; it
// must not use localDaemonSnapshot, whose recovery path can install and start
// a daemon as a side effect.
func readLocalDaemonSnapshot(ctx context.Context) (localapi.Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	paths, err := hostDependenciesOf(ctx).daemonPaths()
	if err != nil {
		return localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, updateDaemonSnapshotTimeout)
	if err != nil {
		return localapi.Snapshot{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, updateDaemonSnapshotTimeout)
	defer cancel()
	return client.Snapshot(probeCtx)
}

func actionUpdateCheck(command *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(command.Context(), 30*time.Second)
	defer cancel()
	// During an activation the check cannot decide eligibility (hostd does not answer): say what is happening instead.
	if err := refuseUpdateWhileActivating(ctx); err != nil {
		return err
	}
	client, err := newUpdateControlClient(ctx, 30*time.Second)
	if err != nil {
		return err
	}
	latest, err := checkSignedRelease(ctx, client)
	if err == nil {
		available, err := signedUpdateAvailable(buildinfo.Version, latest)
		if err != nil {
			return fmt.Errorf("validate signed update version: %w", err)
		}
		return writeUpdateCheckResult(command, latest, available)
	}
	return updateControlFailure("check with the Paperboat updater", err)
}

// updaterRestarting reports an updater that dropped the request or does not answer while its socket exists: it
// restarts after an activation. A refusal it answered, or a denied peer, is a real answer.
func updaterRestarting(ctx context.Context, err error) bool {
	var remote *updated.ControlError
	if errors.As(err, &remote) || errors.Is(err, updated.ErrControlDenied) {
		return false
	}
	return errors.Is(err, updated.ErrUnavailable) || !updaterNotInstalled(ctx)
}

// checkSignedRelease asks the updater for the signed release. Without a reachable updater (an install that was never
// paired, or was signed out, has none) it reads the same signed public TUF metadata itself: checking needs no sign-in
// (prior art: `tailscale update` works logged out, ~/ref/tailscale/clientupdate/clientupdate.go).
func checkSignedRelease(ctx context.Context, client *updated.Client) (string, error) {
	response, err := client.Check(ctx)
	if errors.Is(err, updated.ErrUnavailable) {
		return updateDependenciesOf(ctx).releaseVersion(ctx)
	}
	if err != nil && updaterRestarting(ctx, err) {
		return "", commandRejection{reason: commandRejectUpdaterRestarting, cause: err}
	}
	return response.Version, err
}

// updaterNotInstalled reports that the updater's control socket does not exist, as on an install that was never paired;
// a socket that exists but does not answer is an updater restarting, which an install must not go around.
func updaterNotInstalled(ctx context.Context) bool {
	socket, err := updateDependenciesOf(ctx).socket()
	if err != nil {
		return false
	}
	_, err = os.Lstat(socket)
	return errors.Is(err, os.ErrNotExist)
}

func writeUpdateCheckResult(command *cobra.Command, latest string, available bool) error {
	result := updateCheckResult{InstalledVersion: buildinfo.Version, LatestVersion: latest, UpdateAvailable: available, Verified: true}
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": result})
	}
	if result.UpdateAvailable {
		fmt.Fprintf(command.OutOrStdout(), "Paperboat %s is available; installed version is %s.\n", result.LatestVersion, buildinfo.Version)
	} else {
		fmt.Fprintf(command.OutOrStdout(), "pb %s is up to date.\n", buildinfo.Version)
	}
	return nil
}

func signedUpdateAvailable(installed, latest string) (bool, error) {
	if latest == "" {
		return false, nil
	}
	comparison, err := selfupdate.CompareVersions(latest, installed)
	if err != nil {
		return false, err
	}
	return comparison > 0, nil
}

type updateStatusResult struct {
	OwnerMaintenance  *autoupdate.OwnerMaintenanceNotice `json:"owner_maintenance,omitempty"`
	Settings          *autoupdate.Preferences            `json:"settings,omitempty"`
	NextMaintenance   time.Time                          `json:"next_maintenance_at,omitempty"`
	LatestVersion     string                             `json:"latest_version,omitempty"`
	UpdateAvailable   bool                               `json:"update_available"`
	BlockedReason     string                             `json:"blocked_reason,omitempty"`
	RequiredVersion   string                             `json:"required_version,omitempty"`
	CLIVersion        string                             `json:"cli_version"`
	RuntimeVersion    string                             `json:"runtime_version"`
	RuntimeAvailable  bool                               `json:"runtime_available"`
	RuntimeState      string                             `json:"runtime_state,omitempty"`
	ActivationPending bool                               `json:"activation_pending"`
	ActivationFailure string                             `json:"activation_failure,omitempty"`
	// FailedUpdate is the last update that was rolled back, with where it failed and when.
	FailedUpdate   *failedUpdate `json:"failed_update,omitempty"`
	HealthDeadline time.Time     `json:"health_deadline,omitempty"`
	// ActivationFinishing: the update is installed and the last step (hostd commit, manuals) is being retried.
	ActivationFinishing bool                            `json:"activation_finishing,omitempty"`
	LastCheck           time.Time                       `json:"last_check,omitempty"`
	NextCheck           time.Time                       `json:"next_check,omitempty"`
	LastFailure         string                          `json:"last_failure,omitempty"`
	Candidate           *workerupdate.PreparedCandidate `json:"candidate,omitempty"`
	// CandidateRejected: a downloaded update failed verification (unsafe file, another target, signature).
	CandidateRejected *updated.CandidateRejection `json:"candidate_rejected,omitempty"`
}

func actionUpdateStatus(command *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(command.Context(), 10*time.Second)
	defer cancel()
	client, err := newUpdateControlClient(ctx, 10*time.Second)
	if err != nil {
		return err
	}
	response, err := client.Status(ctx)
	if err == nil {
		var snapshot *localapi.Snapshot
		if daemonSnapshot, snapshotErr := updateDependenciesOf(ctx).daemonSnapshot(ctx); snapshotErr == nil {
			snapshot = &daemonSnapshot
		}
		result := updateStatusCommandResult(buildinfo.Version, response, snapshot)
		return writeUpdateStatusResult(command, result)
	}
	if updaterRestarting(ctx, err) {
		// The updater socket is gone while the updater restarts to activate an approved update (the test host 2026-10-27).
		return commandRejection{reason: commandRejectUpdaterRestarting}
	}
	return fmt.Errorf("read the Paperboat updater status: %w", err)
}

func updateStatusCommandResult(cliVersion string, response updated.ControlResponse, snapshot *localapi.Snapshot) updateStatusResult {
	result := updateStatusResult{BlockedReason: response.Observation.BlockedReason, RequiredVersion: response.Observation.RequiredVersion, CLIVersion: cliVersion, ActivationPending: response.Activating, HealthDeadline: response.Transaction.HealthDeadline, ActivationFailure: response.ActivationFailure, ActivationFinishing: response.Transaction.CommitPending, LastCheck: response.Observation.CheckedAt, NextCheck: response.Observation.NextCheckAt, LastFailure: response.Observation.Failure, Candidate: response.Candidate, CandidateRejected: response.CandidateRejected}
	result.LatestVersion = response.Observation.Version
	if state := response.Transaction; result.ActivationFailure != "" && state.FailedVersion != "" {
		result.FailedUpdate = &failedUpdate{Version: state.FailedVersion, Step: state.FailedStep, Reason: state.FailedReason, RolledBackAt: state.UpdatedAt}
		// The rolled-back release is still available to retry, even if the last check is older.
		if comparison, err := selfupdate.CompareVersions(state.FailedVersion, result.LatestVersion); result.LatestVersion == "" || err == nil && comparison > 0 {
			result.LatestVersion = state.FailedVersion
		}
	}
	if result.ActivationFailure == "" && (response.Transaction.Failure == updateflow.FailureCommit || response.Transaction.Failure == updateflow.FailureRevoked) {
		result.ActivationFailure = string(response.Transaction.Failure)
	}
	// A prepared candidate is verified release evidence even when the last
	// periodic observation predates its download. Preserve a newer observation.
	candidateVersion := ""
	if response.Candidate != nil {
		candidateVersion = response.Candidate.Version
	} else if response.Pending {
		candidateVersion = response.Transaction.CandidateVersion
	}
	if candidateVersion != "" {
		observedVersion := result.LatestVersion
		if observedVersion == "" {
			observedVersion = candidateVersion
		}
		if comparison, compareErr := selfupdate.CompareVersions(candidateVersion, observedVersion); compareErr == nil && comparison >= 0 {
			result.LatestVersion = candidateVersion
		}
	}
	result.Settings, result.NextMaintenance, result.OwnerMaintenance = response.Settings, response.NextMaintenanceAt, response.OwnerMaintenance
	installed := response.Transaction.ActiveVersion
	if installed == "" {
		installed = cliVersion
	}
	// Checks from before the installed version (failed while an activation held the updater) are not current: the
	// installed release is the latest known, and their failure is dropped unless a check after the install failed too.
	if older, err := signedUpdateAvailable(result.LatestVersion, installed); err == nil && older {
		result.LatestVersion = installed
		if response.Transaction.UpdatedAt.IsZero() || !result.LastCheck.After(response.Transaction.UpdatedAt) {
			result.LastFailure = ""
		}
	}
	if available, err := signedUpdateAvailable(installed, result.LatestVersion); err == nil {
		result.UpdateAvailable = available
	}
	if snapshot != nil {
		result.RuntimeState = snapshot.DaemonState
		if daemonSnapshotAvailable(*snapshot) {
			result.RuntimeVersion = snapshot.DaemonVersion
			result.RuntimeAvailable = true
		}
	}
	return result
}

// updateRejectionText is the plain-language reason of a rejected downloaded update.
func updateRejectionText(reason string) string {
	switch reason {
	case "unsafe_file":
		return "its file has unsafe ownership or permissions"
	case "wrong_target":
		return "it is not built for this machine"
	case "signature":
		return "its signature did not verify"
	default:
		return "it is not a valid downloaded update"
	}
}

func daemonSnapshotAvailable(snapshot localapi.Snapshot) bool {
	return snapshot.DaemonVersion != "" && (snapshot.DaemonState == "ready" || snapshot.DaemonState == "degraded")
}

func writeUpdateStatusResult(command *cobra.Command, result updateStatusResult) error {
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": result})
	}
	fmt.Fprintf(command.OutOrStdout(), "CLI: %s\n", result.CLIVersion)
	writeOwnerMaintenanceNotice(command.OutOrStdout(), result.OwnerMaintenance)
	if result.Settings != nil {
		if result.Settings.Enabled {
			fmt.Fprintf(command.OutOrStdout(), "Automatic updates: on, at %s machine local time\n", result.Settings.LocalTime)
		} else {
			fmt.Fprintln(command.OutOrStdout(), "Automatic updates: off")
		}
	}
	if !result.NextMaintenance.IsZero() {
		fmt.Fprintf(command.OutOrStdout(), "Next maintenance: %s\n", result.NextMaintenance.Local().Format(time.RFC3339))
	}
	if result.RuntimeAvailable {
		fmt.Fprintf(command.OutOrStdout(), "Runtime: %s\n", result.RuntimeVersion)
	} else {
		fmt.Fprintln(command.OutOrStdout(), "Runtime: unavailable")
	}
	if result.RuntimeState != "" {
		fmt.Fprintf(command.OutOrStdout(), "Runtime state: %s\n", result.RuntimeState)
	}
	if result.UpdateAvailable && result.Candidate == nil && !result.ActivationPending {
		fmt.Fprintf(command.OutOrStdout(), "Paperboat %s is available. Run `pb update` to download and review it.\n", result.LatestVersion)
	}
	if result.CandidateRejected != nil {
		fmt.Fprintf(command.OutOrStdout(), "The downloaded update %s failed verification (%s). Run `pb update download` to fetch it again.\n", result.CandidateRejected.Version, updateRejectionText(result.CandidateRejected.Reason))
	}
	if result.Candidate != nil {
		writePreparedUpdate(command, result.Candidate)
	} else if result.ActivationFinishing {
		fmt.Fprintln(command.OutOrStdout(), "Activation: installed, finishing the last step (retrying)")
	} else if result.ActivationPending {
		fmt.Fprintln(command.OutOrStdout(), "Activation: "+activationFinishingText(result.HealthDeadline, time.Now()))
	} else if result.ActivationFailure == string(updateflow.FailureRevoked) {
		fmt.Fprintf(command.OutOrStdout(), "Activation: failed (%s). The installed version is no longer allowed. Run `pb update` to install the current release, or `pb uninstall` to remove Paperboat.\n", result.ActivationFailure)
	} else if result.FailedUpdate != nil {
		fmt.Fprintln(command.OutOrStdout(), "Activation: "+result.FailedUpdate.text(result.ActivationFailure))
	} else if result.ActivationFailure != "" {
		fmt.Fprintf(command.OutOrStdout(), "Activation: failed (%s)\n", result.ActivationFailure)
	} else {
		fmt.Fprintln(command.OutOrStdout(), "Activation: complete")
	}
	if !result.NextCheck.IsZero() {
		fmt.Fprintf(command.OutOrStdout(), "Next automatic check: %s\n", result.NextCheck.Local().Format(time.RFC3339))
	}
	if result.LastFailure != "" {
		fmt.Fprintf(command.OutOrStdout(), "Last update failure: %s\n", result.LastFailure)
	}

	return nil
}

func actionUpdateDownload(command *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(command.Context(), 15*time.Minute)
	defer cancel()
	if err := refuseUpdateWhileActivating(ctx); err != nil {
		return err
	}
	client, err := newUpdateControlClient(ctx, 15*time.Minute)
	if err != nil {
		return err
	}
	response, err := updateWithProgress(command, ctx, client.Download)
	if err != nil {
		return updateControlFailure("download signed update", err)
	}
	return writeDownloadedUpdate(command, response)
}

func writePreparedUpdate(command *cobra.Command, candidate *workerupdate.PreparedCandidate) {
	fmt.Fprintf(command.OutOrStdout(), "Downloaded Paperboat %s (%s/%s, %d bytes).\nSHA256: %s\n", candidate.Version, candidate.Platform, candidate.Architecture, candidate.Length, candidate.SHA256)
	fmt.Fprintf(command.OutOrStdout(), "Review this update, then install with:\n  pb update --approve %s\n", candidate.ID)
	if candidate.OwnerMaintenance {
		fmt.Fprintln(command.OutOrStdout(), "This update replaces the process owner. Installation waits for protected sessions to close. Use --force only to permit interruption.")
	} else {
		fmt.Fprintln(command.OutOrStdout(), "Installation briefly interrupts active connections. Running shells and commands stay alive while clients reconnect.")
	}
}

func askUpdateInstall(command *cobra.Command, version string) (bool, error) {
	fmt.Fprintf(command.OutOrStdout(), "Install Paperboat %s now? [y/N] ", version)
	answer, err := bufio.NewReader(io.LimitReader(command.InOrStdin(), 128)).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read update approval: %w", err)
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes", nil
}

// updatePendingEnrollmentInstall installs the signed release when it is newer than this build; the check is the same
// signed metadata read `pb update check` makes. It follows the rules of a downloaded candidate: without --force, JSON
// or unattended runs only report the release, and a terminal asks first. The bundle is bound to the version checked here.
func updatePendingEnrollmentInstall(command *cobra.Command, ctx context.Context, client *updated.Client, force bool) error {
	latest, err := checkSignedRelease(ctx, client)
	if err != nil {
		return updateControlFailure("check with the Paperboat updater", err)
	}
	available, err := signedUpdateAvailable(buildinfo.Version, latest)
	if err != nil {
		return fmt.Errorf("validate signed update version: %w", err)
	}
	if !available {
		return writeDownloadedUpdate(command, updated.ControlResponse{})
	}
	if !force && (updateJSON(command) || !updateDependenciesOf(ctx).inputIsTerminal(command)) {
		if err := writeUpdateCheckResult(command, latest, true); err != nil {
			return err
		}
		if !updateJSON(command) {
			fmt.Fprintln(command.OutOrStdout(), "Install it with `pb update` in a terminal, or `pb update --force`.")
		}
		return nil
	}
	if !force {
		approved, err := askUpdateInstall(command, latest)
		if err != nil {
			return err
		}
		if !approved {
			fmt.Fprintln(command.OutOrStdout(), "The update was not installed.")
			return nil
		}
	}
	return installOfficialBundle(command, ctx, force, latest)
}

func writeDownloadedUpdate(command *cobra.Command, response updated.ControlResponse) error {
	if updateJSON(command) {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": response})
	}
	if response.Candidate != nil {
		writePreparedUpdate(command, response.Candidate)
	} else {
		fmt.Fprintln(command.OutOrStdout(), "Paperboat is up to date.")
	}
	return nil
}

func updateInputIsTerminal(command *cobra.Command) bool {
	input, ok := command.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(input.Fd()))
}

// updateApprovalIDPattern is the shape of a candidate ID: the updater refuses anything else, and the user is told so before
// any request is made.
var updateApprovalIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func actionUpdate(command *cobra.Command, _ []string) error {
	if err := refuseMacRootInstall(runtime.GOOS, os.Geteuid()); err != nil {
		return err
	}
	from, _ := command.Flags().GetString("from")
	force, _ := command.Flags().GetBool("force")
	if from != "" {
		return actionUpdateFrom(command, from, force)
	}
	ctx, cancel := context.WithTimeout(command.Context(), 15*time.Minute)
	defer cancel()
	client, err := newUpdateControlClient(ctx, 15*time.Minute)
	if err != nil {
		return err
	}
	approvalID, _ := command.Flags().GetString("approve")
	if approvalID != "" && !updateApprovalIDPattern.MatchString(approvalID) {
		return commandRejection{reason: commandRejectUpdateApprovalID}
	}
	if err := refuseUpdateWhileActivating(ctx); err != nil {
		return err
	}
	if approvalID == "" {
		response, err := updateWithProgress(command, ctx, client.Download)
		if err != nil {
			var controlErr *updated.ControlError
			if force && errors.As(err, &controlErr) && controlErr.Code == "custom_installation" {
				return installOfficialBundle(command, ctx, true, "")
			}
			// Updating is fetching the signed public release, not an account feature: an install whose enrollment is still
			// pending cannot take the updater's prepared update (it would replace the slot the enrollment commit verifies),
			// so it installs the same signed bundle through the supplied-install transaction, which keeps the enrollment
			// pending and re-points it at the new slot.
			if errors.As(err, &controlErr) && controlErr.Code == "installation_pending" || errors.Is(err, updated.ErrUnavailable) && updaterNotInstalled(ctx) {
				return updatePendingEnrollmentInstall(command, ctx, client, force)
			}
			return updateControlFailure("download signed update", err)
		}
		if response.Candidate == nil || (!force && (updateJSON(command) || !updateDependenciesOf(ctx).inputIsTerminal(command))) {
			return writeDownloadedUpdate(command, response)
		}
		if !force {
			writePreparedUpdate(command, response.Candidate)
			approved, err := askUpdateInstall(command, response.Candidate.Version)
			if err != nil {
				return err
			}
			if !approved {
				fmt.Fprintln(command.OutOrStdout(), "Update remains downloaded; installation was not requested.")
				return nil
			}
		}
		approvalID = response.Candidate.ID
	}
	if refusal := refuseSignedUpdateWhileConnectionsOpen(ctx, runtime.GOOS, force); refusal != nil {
		return refusal
	}
	response, err := updateWithProgress(command, ctx, func(ctx context.Context) (updated.ControlResponse, error) {
		return client.Install(ctx, approvalID, force)
	})
	if err != nil {
		return updateControlFailure("install approved update", err)
	}
	var snapshot *localapi.Snapshot
	if response.Updated && !response.Pending {
		if value, err := updateDependenciesOf(ctx).daemonSnapshot(ctx); err == nil {
			snapshot = &value
		}
	}
	return writeUpdateResult(command, updateCommandResult(buildinfo.Version, response, snapshot), response.Version)
}

// refuseSignedUpdateWhileConnectionsOpen is the Windows counterpart of the Unix updater's own refusal: replacing the
// daemon ends open `pb ssh` connections and access forwards, so a manual update says so first and --force overrides.
// Unix refuses inside the updater service, which the Windows service (a different account) cannot ask for the user's
// connections; an automatic Windows update is checked by the updater service itself (updated.refuseWhileConnectionsAreOpen),
// and a probe that cannot answer proceeds in both and is recorded (liveconnections.RecordUnconfirmed).
func refuseSignedUpdateWhileConnectionsOpen(ctx context.Context, goos string, force bool) error {
	if goos != "windows" || force {
		return nil
	}
	return updateOpenConnectionsFromFile(ctx)
}

func updateCommandResult(previousVersion string, response updated.ControlResponse, snapshot *localapi.Snapshot) updateResult {
	completed := response.Updated && !response.Pending
	runtimeUpdated := completed && snapshot != nil && daemonSnapshotAvailable(*snapshot) && snapshot.DaemonVersion == response.Version
	return updateResult{PreviousVersion: previousVersion, Version: response.Version, CLIUpdated: completed, RuntimeUpdated: runtimeUpdated, ActivationPending: response.Pending, HealthDeadline: response.Transaction.HealthDeadline}
}

// activationFinishingText says what a pending activation is doing and roughly when it ends. Without a
// recorded deadline the signed stability window bounds it (releasepolicy StabilityWindowSeconds, 600).
func activationFinishingText(deadline, now time.Time) string {
	const text = "installed, finishing its health check"
	if deadline.IsZero() {
		return text + " (up to 10 minutes)"
	}
	left := deadline.Sub(now)
	if left <= time.Minute {
		return text + " (about a minute left)"
	}
	return fmt.Sprintf("%s (about %d minutes left)", text, int((left+time.Minute-1)/time.Minute))
}

// updateWithProgress keeps the control protocol single-response and bounded,
// while making the deliberately long health-monitoring hold visible to human
// users. JSON mode remains machine-clean: progress is omitted entirely.
func updateWithProgress(command *cobra.Command, ctx context.Context, update func(context.Context) (updated.ControlResponse, error)) (updated.ControlResponse, error) {
	if update == nil {
		return updated.ControlResponse{}, errors.New("update operation is unavailable")
	}
	if updateJSON(command) {
		return update(ctx)
	}
	fmt.Fprintln(command.ErrOrStderr(), "Processing the signed Paperboat update...")
	type result struct {
		response updated.ControlResponse
		err      error
	}
	resultCh := make(chan result, 1)
	started := time.Now()
	go func() {
		response, err := update(ctx)
		resultCh <- result{response: response, err: err}
	}()
	interval := timingsOf(ctx).updateProgress
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case result := <-resultCh:
			return result.response, result.err
		case <-ctx.Done():
			// The control client closes its connection on cancellation, so its result follows at once; a call
			// that does not is abandoned after the grace instead of holding the CLI past its deadline.
			grace := time.NewTimer(timingsOf(ctx).updateCancelGrace)
			defer grace.Stop()
			select {
			case finished := <-resultCh:
				return finished.response, errors.Join(finished.err, ctx.Err())
			case <-grace.C:
				return updated.ControlResponse{}, ctx.Err()
			}
		case <-ticker.C:
			fmt.Fprintf(command.ErrOrStderr(), "Update is still in progress (%s)...\n", time.Since(started).Round(time.Second))
		}
	}
}

func updateJSON(command *cobra.Command) bool {
	jsonOutput, _ := command.Flags().GetBool("json")
	return jsonOutput
}

func writeUpdateResult(command *cobra.Command, result updateResult, version string) error {
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": result})
	}
	if result.ActivationPending {
		fmt.Fprintf(command.OutOrStdout(), "Paperboat %s is %s. Run `pb update status` to confirm completion.\n", version, activationFinishingText(result.HealthDeadline, time.Now()))
		return nil
	}
	if !result.CLIUpdated && !result.RuntimeUpdated {
		fmt.Fprintf(command.OutOrStdout(), "pb %s is already up to date.\n", version)
		return nil
	}
	if result.CLIUpdated && !result.RuntimeUpdated {
		fmt.Fprintf(command.OutOrStdout(), "Updated pb to %s.\n", version)
		return nil
	}
	fmt.Fprintf(command.OutOrStdout(), "Updated Paperboat runtime to %s.\n", version)
	return nil
}

func newUpdateControlClient(ctx context.Context, timeout time.Duration) (*updated.Client, error) {
	socket, err := updateDependenciesOf(ctx).socket()
	if err != nil {
		return nil, fmt.Errorf("resolve the current user's updater: %w", err)
	}
	return updated.NewClient(socket, timeout)
}

func updatedControlSocket() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	var layout service.Layout
	if runtime.GOOS == "windows" {
		layout, err = service.WindowsUserLayout(account.Uid)
	} else {
		uid, parseErr := strconv.Atoi(account.Uid)
		if parseErr != nil {
			return "", parseErr
		}
		layout, err = service.UserLayout(runtime.GOOS, uid)
	}
	return layout.UpdaterSocket, err
}

type failedUpdate struct {
	Version      string    `json:"version"`
	Step         string    `json:"step,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	RolledBackAt time.Time `json:"rolled_back_at"`
}

// text says which update was rolled back, where and when (canary 2026-11-26: status said only
// "failed (drain_failed)").
func (f *failedUpdate) text(failure string) string {
	step := failure
	if f.Step != "" {
		step = f.Step + " (" + f.Reason + ")"
	}
	return fmt.Sprintf("Last update to %s failed at %s and was rolled back at %s; run `pb update` to retry.", f.Version, step, f.RolledBackAt.Local().Format(time.RFC3339))
}
