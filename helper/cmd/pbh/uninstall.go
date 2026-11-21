package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/helper/internal/containers"
	"github.com/pinksaucepasta/paperboat/relay/selfhost"
)

func uninstall(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	removeData := fs.Bool("remove-data", false, "also delete protected identity, credentials and configuration")
	confirm := fs.Bool("confirm-remove-data", false, "explicitly confirm permanent data deletion")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(out, "uninstall", fs)
			return nil
		}
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
	// An absent main unit is an expected state on a rerun, so a failure after it was removed can be retried.
	if _, err := os.Stat(selfhostUnit); err == nil {
		if err := exec.CommandContext(ctx, "systemctl", "stop", "paperboat-selfhost.service").Run(); err != nil {
			return fmt.Errorf("%w: %w", commandError("could not stop self-host service; nothing removed"), err)
		}
	}
	if err := removeCertificateUnits(ctx, execCommander, systemUnitDir, certificateEnvFile, *removeData); err != nil {
		return err
	}
	if err := removeHostNetwork(ctx, execCommander, defaultHostPaths(defaultState), out); err != nil {
		return err
	}
	if err := containers.Teardown(ctx, defaultState); err != nil {
		return err
	}
	if _, err := os.Stat(selfhostUnit); err == nil {
		if err := exec.CommandContext(ctx, "systemctl", "disable", "paperboat-selfhost.service").Run(); err != nil {
			return err
		}
		if err := os.Remove(selfhostUnit); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
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

const (
	systemUnitDir      = "/etc/systemd/system"
	selfhostUnit       = systemUnitDir + "/paperboat-selfhost.service"
	certificateEnvFile = "/etc/paperboat-edge-certificate.env"
)

// certificateUnits run edge-ip-certificate.py from the directory uninstall deletes (deploy/README.md), so they
// are disabled and removed with it; the env file is configuration and goes only with --remove-data.
var certificateUnits = []string{"paperboat-edge-certificate.timer", "paperboat-edge-certificate.service"}

func removeCertificateUnits(ctx context.Context, run commander, unitDir, envFile string, removeData bool) error {
	var errs []error
	for _, unit := range certificateUnits {
		path := filepath.Join(unitDir, unit)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, err := run(ctx, "systemctl", "disable", "--now", unit); err != nil {
			errs = append(errs, fmt.Errorf("disable %s: %w", unit, err))
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if removeData && len(errs) == 0 { // the env file the units read stays until both are gone
		if err := os.Remove(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", commandError("could not remove the edge certificate timer and service; fix the cause and rerun pbh uninstall"), errors.Join(errs...))
	}
	return nil
}
