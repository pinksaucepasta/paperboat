package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/pinksaucepasta/paperboat-helper/internal/containers"
	"github.com/pinksaucepasta/paperboat-relay/selfhost"
)

func uninstall(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	removeData := fs.Bool("remove-data", false, "also delete protected identity, credentials and configuration")
	confirm := fs.Bool("confirm-remove-data", false, "explicitly confirm permanent data deletion")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return commandError("unexpected uninstall arguments")
	}
	if *removeData != *confirm {
		return commandError("data deletion requires both --remove-data and --confirm-remove-data; omit both to retain state")
	}
	if os.Geteuid() != 0 {
		return elevated(ctx, append([]string{"uninstall"}, args...), out)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "stop", "paperboat-selfhost.service").Run(); err != nil {
		return fmt.Errorf("%w: %w", commandError("could not stop self-host service; nothing removed"), err)
	}
	if err := containers.Teardown(ctx, defaultState); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "disable", "paperboat-selfhost.service").Run(); err != nil {
		return err
	}
	if err := os.Remove("/etc/systemd/system/paperboat-selfhost.service"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "daemon-reload").Run(); err != nil {
		return err
	}
	if err := os.RemoveAll(defaultBinaries); err != nil {
		return err
	}
	if err := os.Remove("/usr/local/bin/pbh"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if *removeData {
		if err := selfhost.RemoveLocal(defaultState); err != nil {
			return err
		}
		fmt.Fprintln(out, "Self-host containers, service and explicitly confirmed local data removed.")
	} else {
		fmt.Fprintln(out, "Self-host containers and service removed; protected identity, credentials and configuration retained for reinstall.")
	}
	fmt.Fprintln(out, "Remove the node from Dashboard → Network to revoke its registry entry.")
	return nil
}
