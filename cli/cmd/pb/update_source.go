package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/diskspace"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/cli/internal/liveconnections"
	"github.com/spf13/cobra"
)

// The protected installer authenticates bundles against its own retained TUF
// state. Staging here grants neither official provenance nor rollback permission.
func actionUpdateFrom(command *cobra.Command, input string, force bool) error {
	approval, _ := command.Flags().GetString("approve")
	if approval != "" {
		return commandRejection{reason: commandRejectUpdateFlags}
	}
	ctx, cancel := context.WithTimeout(command.Context(), 15*time.Minute)
	defer cancel()
	directory, err := os.MkdirTemp("", "paperboat-update-")
	if err != nil {
		return stagingFailure(err, err)
	}
	defer os.RemoveAll(directory)
	staged, err := installsource.StageInput(ctx, input, directory, nil)
	if err != nil {
		return stagingFailure(err, commandRejection{reason: commandRejectUpdateSource, detail: err.Error()})
	}
	return installStagedUpdateSource(command, ctx, staged.Path, force)
}

// stagingFailure says "disk full" when the copy of the update into the temporary directory (or creating that directory)
// ran out of space (F14-03), instead of telling the user the file cannot be read; any other failure is otherwise.
func stagingFailure(err, otherwise error) error {
	if diskspace.IsFull(err) {
		return hostinstall.ErrInsufficientSpace
	}
	return otherwise
}

// updateOpenConnectionsFromFile keeps an installation from a file from silently ending open SSH
// connections and access forwards, as the signed updater does: replacing the daemon closes them.
// A daemon that cannot answer never blocks the install (the updater must work when the app is broken); the failed probe
// is recorded (liveconnections.RecordUnconfirmed) so a session it cut can be explained.
func updateOpenConnectionsFromFile(ctx context.Context) error {
	open, err := updateDependenciesOf(ctx).openConnections(ctx)
	if err != nil {
		liveconnections.RecordUnconfirmed("pb update", err)
		return nil
	}
	if open.Total() == 0 {
		return nil
	}
	return updateOpenConnectionsRefusal{message: (&autoupdate.ActiveTerminalSessionsError{SSH: open.SSH, Forwards: open.Forwards}).Error()}
}

// updateCompletionFailure reports a step that failed after the update was installed.
type updateCompletionFailure struct {
	stage string
	cause error
}

func (e updateCompletionFailure) Error() string {
	return "Paperboat was updated, but the CLI could not " + e.stage + ". The new version is installed; run `pb doctor` to confirm it is healthy before retrying."
}
func (e updateCompletionFailure) Unwrap() error { return e.cause }

func installStagedUpdateSource(command *cobra.Command, ctx context.Context, stagedPath string, force bool) error {
	var source installsource.Source
	var err error
	executable, bundle := stagedPath, ""
	if strings.EqualFold(filepath.Ext(stagedPath), ".pb-update") {
		executable, bundle = "", stagedPath
	} else {
		if err = os.Chmod(executable, 0700); err != nil {
			return err
		}
		source, err = updateDependenciesOf(ctx).inspect(ctx, executable)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			// A file that does not run as a Paperboat build (a text file, another program, a wrong platform) is the user's
			// input, not an internal failure; the cause stays out of the message (it can quote the program's output).
			// A local failure (permission, disk) keeps its cause: retrying can fix it.
			if !errors.Is(err, installsource.ErrInput) {
				return err
			}
			return commandRejection{reason: commandRejectUpdateSource, detail: "the file is not a Paperboat executable, package or .pb-update bundle for this machine"}
		}
	}
	if bundle != "" {
		if made, wrong := bundleMadeForOtherOS(bundle, runtime.GOOS); wrong {
			return commandRejection{reason: commandRejectUpdateSource, detail: "this .pb-update bundle was made on " + made + " and this machine is " + runtime.GOOS + "; run `pb update bundle` on a " + runtime.GOOS + " machine"}
		}
	}
	if !force {
		if refusal := updateOpenConnectionsFromFile(ctx); refusal != nil {
			return refusal
		}
	}
	installed, err := updateDependenciesOf(ctx).install(ctx, executable, source, bundle, force, "", selectedInstallServer(command))
	if err != nil {
		if errors.Is(err, hostdproto.ErrMaintenanceBusy) {
			return finishingUpdateRefusal(ctx, err)
		}
		return err
	}
	// The installation is committed here. A failure below must say so, because
	// the generic failure text invites a retry of an update that already landed.
	canonical, err := filepath.EvalSymlinks(installed)
	if err != nil {
		return updateCompletionFailure{stage: "locate the installed executable", cause: err}
	}
	actual, err := updateDependenciesOf(ctx).inspect(ctx, canonical)
	if err != nil {
		return updateCompletionFailure{stage: "verify the installed executable", cause: err}
	}
	if bundle != "" {
		// The bundle keeps the machine's own automatic-update setting, which this process cannot see: not reported.
		actual.Distribution = installsource.Official
	}
	return writeUpdateFromResult(command, buildinfo.Version, installed, actual)
}

// writeUpdateFromResult says what the install did. Installing the build that is already
// running replaces its files with identical ones, which is not an update.
func writeUpdateFromResult(command *cobra.Command, previousVersion, installed string, actual installsource.Source) error {
	changed := actual.Version != previousVersion
	if updateJSON(command) {
		result := map[string]any{"executable": installed, "version": actual.Version, "previous_version": previousVersion, "changed": changed, "distribution": actual.Distribution, "sha256": actual.SHA256, "enrollment": "unchanged"}
		if actual.Distribution != installsource.Official {
			result["automatic_update_checks"] = actual.AutomaticUpdates
		}
		return writeCLIJSON(command.OutOrStdout(), result)
	}
	if !changed {
		_, err := fmt.Fprintf(command.OutOrStdout(), "Paperboat is already on %s; reinstalled it from the supplied file.\n", actual.Version)
		return err
	}
	_, err := fmt.Fprintf(command.OutOrStdout(), "Updated Paperboat from %s to %s.\n", previousVersion, actual.Version)
	return err
}

func updateBundleCommand() *cobra.Command {
	command := &cobra.Command{Use: "bundle --output <file.pb-update>", Short: "Save an official update and its signed proof for offline installation", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		output, _ := command.Flags().GetString("output")
		if output == "" {
			return localArgumentError("--output is required: name the new file, for example `pb update bundle --output paperboat.pb-update`")
		}
		if !strings.EqualFold(filepath.Ext(output), ".pb-update") {
			return localArgumentError("--output must name a .pb-update file")
		}
		output, err := filepath.Abs(output)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			return localArgumentError("the bundle output already exists; choose a new file name")
		}
		ctx, cancel := context.WithTimeout(command.Context(), 15*time.Minute)
		defer cancel()
		// Each export owns isolated metadata; it never mutates an installation's
		// protected verifier or races its updater. The receiver enforces its floors.
		state, err := os.MkdirTemp("", "paperboat-update-proof-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(state)
		repository, err := buildinfo.ReleaseRepository()
		if err != nil {
			return err
		}
		index, err := bootstrap.ExportUpdateBundle(ctx, repository, state, output, nil, time.Now())
		if err != nil {
			return bundleExportError(err)
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"output": output, "version": index.Version, "platform": index.Platform, "architecture": index.Architecture, "install_with": "pb update --from " + output})
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "Saved Paperboat %s (%s/%s) to %s.\nInstall offline with: pb update --from %s\n", index.Version, index.Platform, index.Architecture, output, output)
		return err
	}}
	command.Flags().String("output", "", "new .pb-update file for this platform")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func bundleExportError(err error) error {
	if errors.Is(err, bootstrap.ErrReleasePlatformUnpublished) {
		return commandRejection{reason: commandRejectUpdatePlatform, detail: runtime.GOOS + "/" + runtime.GOARCH}
	}
	return err
}

// installOfficialBundle installs the signed official release as a bundle, verified like `pb update --from <bundle>`.
// force moves a custom installation to official bytes; a custom runtime still cannot enable automatic updates or
// select arbitrary release URLs. A non-empty checkedVersion binds the install to the release that was checked: a bundle
// of another version (the release moved on, or a different one resolved) installs nothing.
func installOfficialBundle(command *cobra.Command, ctx context.Context, force bool, checkedVersion string) error {
	directory, err := os.MkdirTemp("", "paperboat-official-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	bundle := filepath.Join(directory, "release.pb-update")
	exported, err := updateDependenciesOf(ctx).exportBundle(ctx, filepath.Join(directory, "proof"), bundle)
	if err != nil {
		return bundleExportError(err)
	}
	if checkedVersion != "" && exported != checkedVersion {
		return commandRejection{reason: commandRejectUpdateSource, detail: "the signed release changed while updating (checked " + checkedVersion + ", fetched " + exported + "); nothing was installed, run `pb update` again"}
	}
	return installStagedUpdateSource(command, ctx, bundle, force)
}

// exportOfficialBundle saves the signed release as a bundle and returns its version.
func exportOfficialBundle(ctx context.Context, proofDirectory, output string) (string, error) {
	repository, err := buildinfo.ReleaseRepository()
	if err != nil {
		return "", err
	}
	index, err := bootstrap.ExportUpdateBundle(ctx, repository, proofDirectory, output, nil, time.Now())
	return index.Version, err
}

// finishingUpdateRefusal refuses an install that hostd's maintenance lock turned away, naming the activation that
// holds it when the updater says which one.
func finishingUpdateRefusal(ctx context.Context, cause error) error {
	response, err := updaterActivation(ctx)
	if err != nil || response.Transaction.CandidateVersion == "" {
		return commandRejection{reason: commandRejectUpdateBusy, cause: cause}
	}
	return activationRefusal(response, cause)
}

// activationRefusal says an update is being activated (the JSON code stays update_busy).
func activationRefusal(response updated.ControlResponse, cause error) error {
	update := "an update"
	if response.Transaction.CandidateVersion != "" {
		update += " to " + response.Transaction.CandidateVersion
	}
	state := response.Transaction
	if activationOverdue(state, time.Now()) {
		return commandRejection{reason: commandRejectUpdateActivating, cause: cause, detail: "A" + update[1:] + " has not finished its health check after 10 minutes. Run `pb doctor`, and `pb update status` to see whether it recovered."}
	}
	finishing := strings.TrimPrefix(activationFinishingText(state.HealthDeadline, time.Now()), "installed, ")
	return commandRejection{reason: commandRejectUpdateActivating, cause: cause, detail: "A" + update[1:] + " is in progress: " + finishing + ". Run `pb update status` to follow it."}
}

// activationWindow is the signed stability window (releasepolicy StabilityWindowSeconds) that bounds an activation;
// activationMargin is the most the updater's health watch runs past it (workerupdate.stabilityCompletionMargin).
const (
	activationWindow = 10 * time.Minute
	activationMargin = 30 * time.Second
)

// activationOverdue reports an activation past the end of its health watch: the recorded deadline, or without one
// the signed window from the activation's start, plus the watch's margin.
func activationOverdue(state workerupdate.TransactionState, now time.Time) bool {
	end := state.HealthDeadline
	if end.IsZero() {
		if state.UpdatedAt.IsZero() {
			return false
		}
		end = state.UpdatedAt.Add(activationWindow)
	}
	return now.After(end.Add(activationMargin))
}

// updaterActivation reads the updater's status within a few seconds: status answers without the control lock an
// activation holds, where a check or install waits behind it.
func updaterActivation(ctx context.Context) (updated.ControlResponse, error) {
	client, err := newUpdateControlClient(ctx, 5*time.Second)
	if err != nil {
		return updated.ControlResponse{}, err
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return client.Status(probe)
}

// refuseUpdateWhileActivating refuses at once while an activation is finishing; a status that cannot be read proceeds.
func refuseUpdateWhileActivating(ctx context.Context) error {
	response, err := updaterActivation(ctx)
	if err != nil || !response.Activating {
		return nil
	}
	return activationRefusal(response, hostdproto.ErrMaintenanceBusy)
}

// fetchSignedReleaseVersion reads the version of the signed official release for this platform.
func fetchSignedReleaseVersion(ctx context.Context) (string, error) {
	repository, err := buildinfo.ReleaseRepository()
	if err != nil {
		return "", err
	}
	directory, err := os.MkdirTemp("", "paperboat-update-check-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(directory)
	index, err := bootstrap.FetchVerifiedReleaseIndex(ctx, repository, directory, nil, time.Now())
	return index.Version, err
}

// bundleMadeForOtherOS tells from the file magic of the executable inside a .pb-update bundle (which records no platform) that it was made
// on another OS than `current`. A bundle it cannot read, or an executable of an unknown kind, is left to the signature checks.
func bundleMadeForOtherOS(path, current string) (made string, wrong bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	archive := tar.NewReader(file)
	for {
		header, err := archive.Next()
		if err != nil {
			return "", false
		}
		if strings.HasPrefix(header.Name, "metadata/") {
			continue
		}
		magic := make([]byte, 4)
		if _, err := io.ReadFull(archive, magic); err != nil {
			return "", false
		}
		switch {
		case bytes.Equal(magic, []byte("\x7fELF")):
			made = "linux"
		case bytes.Equal(magic, []byte{0xcf, 0xfa, 0xed, 0xfe}), bytes.Equal(magic, []byte{0xca, 0xfe, 0xba, 0xbe}):
			made = "darwin"
		case bytes.Equal(magic[:2], []byte("MZ")):
			made = "windows"
		default:
			return "", false
		}
		return made, made != current
	}
}
