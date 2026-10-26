package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/selfupdate"
	"github.com/spf13/cobra"
)

// The protected installer authenticates bundles against its own retained TUF
// state. Staging here grants neither official provenance nor rollback permission.
func actionUpdateFrom(command *cobra.Command, input string, force bool) error {
	approval, _ := command.Flags().GetString("approve")
	if approval != "" {
		return errors.New("--from cannot be combined with --approve")
	}
	ctx, cancel := context.WithTimeout(command.Context(), 15*time.Minute)
	defer cancel()
	directory, err := os.MkdirTemp("", "paperboat-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	staged, err := installsource.StageInput(ctx, input, directory, nil)
	if err != nil {
		return err
	}
	return installStagedUpdateSource(command, ctx, staged.Path, force)
}

var installUpdateSourceForCommand = hostruntimecmd.InstallUpdateSource

// inspectUpdateCandidate reads what a staged executable is by running it, before anything is installed.
var inspectUpdateCandidate = installsource.InspectCandidate

var openConnectionsFromDaemon = openConnectionsForUpdate

// updateOpenConnectionsFromFile keeps an installation from a file from silently ending open SSH
// connections and access forwards, as the signed updater does: replacing the daemon closes them.
// A daemon that cannot answer never blocks the install (the updater must work when the app is broken).
func updateOpenConnectionsFromFile(ctx context.Context) error {
	open, err := openConnectionsFromDaemon(ctx)
	if err != nil || open.Total() == 0 {
		return nil
	}
	return updateOpenConnectionsRefusal{message: (&autoupdate.ActiveTerminalSessionsError{SSH: open.SSH, Forwards: open.Forwards}).Error()}
}

// updateVersionUnsupportedError names an official build older than the oldest release this service
// supports. Installed over a newer one, it fails at startup: the service keeps sending a machine what it
// reported it could take (peer endpoints), and the older build rejects the signed network configuration
// that carries it, so the native carrier restarts until the newer version returns.
type updateVersionUnsupportedError struct{ version, oldest string }

func (e updateVersionUnsupportedError) Error() string {
	return "Paperboat " + e.version + " is older than " + e.oldest + ", the oldest release this service supports; installed here it would stop this computer's private connections. Nothing was changed. Install " + e.oldest + " or newer. To use " + e.version + " anyway, run `pb uninstall`, then install it again and pair this computer as new."
}

// refuseUnsupportedUpdateTarget refuses an official build older than the oldest supported release before
// anything is installed. Builds without a release version (development and custom builds) cannot be
// compared and are not refused.
func refuseUnsupportedUpdateTarget(version string) error {
	comparison, err := selfupdate.CompareVersions(version, buildinfo.OldestSupportedOfficial)
	if err != nil || comparison >= 0 {
		return nil
	}
	return commandRejection{reason: commandRejectUpdateUnsupported, cause: updateVersionUnsupportedError{version: version, oldest: buildinfo.OldestSupportedOfficial}}
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
		source, err = inspectUpdateCandidate(ctx, executable)
		if err != nil {
			return err
		}
		if err = refuseUnsupportedUpdateTarget(source.Version); err != nil {
			return err
		}
	}
	if !force {
		if refusal := updateOpenConnectionsFromFile(ctx); refusal != nil {
			return refusal
		}
	}
	installed, err := installUpdateSourceForCommand(ctx, executable, source, bundle, force, "", selectedInstallServer(command))
	if err != nil {
		if errors.Is(err, hostdproto.ErrMaintenanceBusy) {
			return commandRejection{reason: commandRejectUpdateBusy, cause: err}
		}
		return err
	}
	// The installation is committed here. A failure below must say so, because
	// the generic failure text invites a retry of an update that already landed.
	canonical, err := filepath.EvalSymlinks(installed)
	if err != nil {
		return updateCompletionFailure{stage: "locate the installed executable", cause: err}
	}
	actual, err := installsource.InspectCandidate(ctx, canonical)
	if err != nil {
		return updateCompletionFailure{stage: "verify the installed executable", cause: err}
	}
	if bundle != "" {
		actual.Distribution, actual.AutomaticUpdates = installsource.Official, true
	}
	return writeUpdateFromResult(command, buildinfo.Version, installed, actual)
}

// writeUpdateFromResult says what the install did. Installing the build that is already
// running replaces its files with identical ones, which is not an update.
func writeUpdateFromResult(command *cobra.Command, previousVersion, installed string, actual installsource.Source) error {
	changed := actual.Version != previousVersion
	if updateJSON(command) {
		return writeCLIJSON(command.OutOrStdout(), map[string]any{"executable": installed, "version": actual.Version, "previous_version": previousVersion, "changed": changed, "distribution": actual.Distribution, "automatic_update_checks": actual.AutomaticUpdates, "sha256": actual.SHA256, "enrollment": "unchanged"})
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
			return err
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

// Explicit force may move a custom installation to official bytes. A custom
// runtime still cannot enable automatic updates or select arbitrary release URLs.
func actionForceOfficial(command *cobra.Command, ctx context.Context) error {
	directory, err := os.MkdirTemp("", "paperboat-official-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	bundle := filepath.Join(directory, "release.pb-update")
	repository, err := buildinfo.ReleaseRepository()
	if err != nil {
		return err
	}
	_, err = bootstrap.ExportUpdateBundle(ctx, repository, filepath.Join(directory, "proof"), bundle, nil, time.Now())
	if err != nil {
		return err
	}
	return installStagedUpdateSource(command, ctx, bundle, true)
}
