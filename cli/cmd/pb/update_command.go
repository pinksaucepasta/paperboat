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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	service "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/selfupdate"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type updateResult struct {
	PreviousVersion   string `json:"previous_version"`
	Version           string `json:"version"`
	CLIUpdated        bool   `json:"cli_updated"`
	RuntimeUpdated    bool   `json:"runtime_updated"`
	ActivationPending bool   `json:"activation_pending,omitempty"`
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

var updateControlSocketForCommand = updatedControlSocket

const updateDaemonSnapshotTimeout = 2 * time.Second

// readLocalDaemonSnapshot is deliberately a direct read-only probe. Update
// status must report whether the authenticated daemon is actually present; it
// must not use localDaemonSnapshot, whose recovery path can install and start
// a daemon as a side effect.
var readLocalDaemonSnapshot = func(ctx context.Context) (localapi.Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	paths, err := currentLocalDaemonPaths()
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
	client, err := newUpdateControlClient(30 * time.Second)
	if err != nil {
		return err
	}
	response, err := client.Check(ctx)
	if err == nil {
		available, err := signedUpdateAvailable(buildinfo.Version, response.Version)
		if err != nil {
			return fmt.Errorf("validate signed update version: %w", err)
		}
		return writeUpdateCheckResult(command, response.Version, available)
	}
	return updateControlFailure("check with paperboat-updated", err)
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
	LastCheck         time.Time                          `json:"last_check,omitempty"`
	NextCheck         time.Time                          `json:"next_check,omitempty"`
	LastFailure       string                             `json:"last_failure,omitempty"`
	Candidate         *workerupdate.PreparedCandidate    `json:"candidate,omitempty"`
}

func actionUpdateStatus(command *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(command.Context(), 10*time.Second)
	defer cancel()
	client, err := newUpdateControlClient(10 * time.Second)
	if err != nil {
		return err
	}
	response, err := client.Status(ctx)
	if err == nil {
		var snapshot *localapi.Snapshot
		if daemonSnapshot, snapshotErr := readLocalDaemonSnapshot(ctx); snapshotErr == nil {
			snapshot = &daemonSnapshot
		}
		result := updateStatusCommandResult(buildinfo.Version, response, snapshot)
		return writeUpdateStatusResult(command, result)
	}
	return fmt.Errorf("read paperboat-updated status: %w", err)
}

func updateStatusCommandResult(cliVersion string, response updated.ControlResponse, snapshot *localapi.Snapshot) updateStatusResult {
	result := updateStatusResult{BlockedReason: response.Observation.BlockedReason, RequiredVersion: response.Observation.RequiredVersion, CLIVersion: cliVersion, ActivationPending: response.Pending, ActivationFailure: response.ActivationFailure, LastCheck: response.Observation.CheckedAt, NextCheck: response.Observation.NextCheckAt, LastFailure: response.Observation.Failure, Candidate: response.Candidate}
	result.LatestVersion = response.Observation.Version
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
	if result.Candidate != nil {
		writePreparedUpdate(command, result.Candidate)
	} else if result.ActivationPending {
		fmt.Fprintln(command.OutOrStdout(), "Activation: pending")
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
	client, err := newUpdateControlClient(15 * time.Minute)
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

var updateInputIsTerminal = func(command *cobra.Command) bool {
	input, ok := command.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(input.Fd()))
}

func actionUpdate(command *cobra.Command, _ []string) error {
	from, _ := command.Flags().GetString("from")
	force, _ := command.Flags().GetBool("force")
	if from != "" {
		return actionUpdateFrom(command, from, force)
	}
	ctx, cancel := context.WithTimeout(command.Context(), 15*time.Minute)
	defer cancel()
	client, err := newUpdateControlClient(15 * time.Minute)
	if err != nil {
		return err
	}
	approvalID, _ := command.Flags().GetString("approve")
	if approvalID == "" {
		response, err := updateWithProgress(command, ctx, client.Download)
		if err != nil {
			var controlErr *updated.ControlError
			if force && errors.As(err, &controlErr) && controlErr.Code == "custom_installation" {
				return actionForceOfficial(command, ctx)
			}
			return updateControlFailure("download signed update", err)
		}
		if response.Candidate == nil || (!force && (updateJSON(command) || !updateInputIsTerminal(command))) {
			return writeDownloadedUpdate(command, response)
		}
		if !force {
			writePreparedUpdate(command, response.Candidate)
			fmt.Fprintf(command.OutOrStdout(), "Install Paperboat %s now? [y/N] ", response.Candidate.Version)
			answer, err := bufio.NewReader(io.LimitReader(command.InOrStdin(), 128)).ReadString('\n')
			if err != nil {
				return fmt.Errorf("read update approval: %w", err)
			}
			answer = strings.TrimSpace(strings.ToLower(answer))
			if answer != "y" && answer != "yes" {
				fmt.Fprintln(command.OutOrStdout(), "Update remains downloaded; installation was not requested.")
				return nil
			}
		}
		approvalID = response.Candidate.ID
	}
	response, err := updateWithProgress(command, ctx, func(ctx context.Context) (updated.ControlResponse, error) {
		return client.Install(ctx, approvalID, force)
	})
	if err != nil {
		return updateControlFailure("install approved update", err)
	}
	var snapshot *localapi.Snapshot
	if response.Updated && !response.Pending {
		if value, err := readLocalDaemonSnapshot(ctx); err == nil {
			snapshot = &value
		}
	}
	return writeUpdateResult(command, updateCommandResult(buildinfo.Version, response, snapshot), response.Version)
}

func updateCommandResult(previousVersion string, response updated.ControlResponse, snapshot *localapi.Snapshot) updateResult {
	completed := response.Updated && !response.Pending
	runtimeUpdated := completed && snapshot != nil && daemonSnapshotAvailable(*snapshot) && snapshot.DaemonVersion == response.Version
	return updateResult{PreviousVersion: previousVersion, Version: response.Version, CLIUpdated: completed, RuntimeUpdated: runtimeUpdated, ActivationPending: response.Pending}
}

var updateProgressInterval = 5 * time.Second

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
	interval := updateProgressInterval
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
			finished := <-resultCh
			return finished.response, errors.Join(finished.err, ctx.Err())
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
		fmt.Fprintf(command.OutOrStdout(), "Paperboat %s is staged. Activation is in progress; run `pb update status` to confirm completion.\n", version)
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

func newUpdateControlClient(timeout time.Duration) (*updated.Client, error) {
	socket, err := updateControlSocketForCommand()
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
