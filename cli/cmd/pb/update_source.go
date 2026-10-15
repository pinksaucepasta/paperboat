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
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
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
		source, err = installsource.InspectCandidate(ctx, executable)
		if err != nil {
			return err
		}
	}
	installed, err := hostruntimecmd.InstallUpdateSource(ctx, executable, source, bundle, force, "")
	if err != nil {
		if errors.Is(err, hostdproto.ErrMaintenanceBusy) {
			return commandRejection{reason: commandRejectUpdateBusy, cause: err}
		}
		return err
	}
	canonical, err := filepath.EvalSymlinks(installed)
	if err != nil {
		return err
	}
	actual, err := installsource.InspectCandidate(ctx, canonical)
	if err != nil {
		return fmt.Errorf("installation completed but its executable could not be verified: %w", err)
	}
	if bundle != "" {
		actual.Distribution, actual.AutomaticUpdates = installsource.Official, true
	}
	if updateJSON(command) {
		return writeCLIJSON(command.OutOrStdout(), map[string]any{"executable": installed, "version": actual.Version, "distribution": actual.Distribution, "automatic_update_checks": actual.AutomaticUpdates, "sha256": actual.SHA256, "enrollment": "unchanged"})
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Updated Paperboat to %s.\n", actual.Version)
	return err
}

func updateBundleCommand() *cobra.Command {
	command := &cobra.Command{Use: "bundle --output <file.pb-update>", Short: "Save an official update and its signed proof for offline installation", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		output, _ := command.Flags().GetString("output")
		if !strings.EqualFold(filepath.Ext(output), ".pb-update") {
			return errors.New("--output must name a .pb-update file")
		}
		output, err := filepath.Abs(output)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			return errors.New("the bundle output must not already exist")
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
		_, err = fmt.Fprintf(command.OutOrStdout(), "Saved Paperboat %s (%s/%s) to %s.\nInstall offline with: pb update --from %s\n", index.Version, index.Platform, index.Architecture, output, output)
		return err
	}}
	command.Flags().String("output", "", "new .pb-update file for this platform")
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
