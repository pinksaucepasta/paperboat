//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/spf13/cobra"
)

func removePlatformProductInstallation(ctx context.Context, _ *cobra.Command, _ []string, _ io.Writer) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return "", managedssh.CleanupUnusedLocks(ctx, home, uint32(os.Geteuid()))
}

func platformUninstallSuccessMessage() string {
	return "Paperboat was completely removed. The Paperboat Inbox was preserved."
}

func platformProductHandoffRequired() bool { return false }

func platformRequiresConfirmedDaemonStop() bool { return true }

func unsafeCleanupPathTraversal(_ string, info os.FileInfo) (bool, error) {
	return info.Mode()&os.ModeSymlink != 0, nil
}

func purgePlatformRuntime(command *cobra.Command) error {
	return hostruntimecmd.ExecutePurge(command.Context(), command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr())
}

func platformUninstallHelperCommand() *cobra.Command {
	return &cobra.Command{Use: "__complete-uninstall", Hidden: true, RunE: func(*cobra.Command, []string) error {
		return errors.New("deferred uninstall is only available on Windows")
	}}
}
