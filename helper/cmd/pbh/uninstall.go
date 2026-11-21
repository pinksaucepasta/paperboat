package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
	touched, err := stopServices(ctx, execCommander, systemUnitDir, certificateEnvFile, *removeData)
	if err != nil {
		return err
	}
	if err := removeHostNetwork(ctx, execCommander, defaultHostPaths(defaultState), out); err != nil {
		return err
	}
	if err := containers.Teardown(ctx, defaultState); err != nil {
		return err
	}
	removed, err := removeMainUnit(ctx, execCommander, systemUnitDir)
	if err != nil {
		return err
	}
	if touched || removed { // a host without systemd units (--no-service) has nothing to reload
		if _, err := execCommander(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
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
	selfhostUnitName   = "paperboat-selfhost.service"
	certificateEnvFile = "/etc/paperboat-edge-certificate.env"
)

// certificateUnits run edge-ip-certificate.py from the directory uninstall deletes (deploy/README.md), so they
// are disabled and removed with it; the env file is configuration and goes only with --remove-data.
var certificateUnits = []string{"paperboat-edge-certificate.timer", "paperboat-edge-certificate.service"}

// stopServices stops everything that could start the node again, certificate renewal first (it restarts the
// service), then the service. An absent unit is an expected state on a rerun; touched reports a unit existed.
func stopServices(ctx context.Context, run commander, unitDir, envFile string, removeData bool) (touched bool, err error) {
	touched, err = removeCertificateUnits(ctx, run, unitDir, envFile, removeData)
	if err != nil {
		return touched, err
	}
	exists, err := unitExists(filepath.Join(unitDir, selfhostUnitName))
	if err != nil || !exists {
		return touched, err
	}
	if _, err := run(ctx, "systemctl", "stop", selfhostUnitName); err != nil {
		return true, fmt.Errorf("%w: %w", commandError("could not stop self-host service; nothing removed"), err)
	}
	return true, nil
}

// removeMainUnit disables and deletes the service unit if it is still there.
func removeMainUnit(ctx context.Context, run commander, unitDir string) (bool, error) {
	path := filepath.Join(unitDir, selfhostUnitName)
	exists, err := unitExists(path)
	if err != nil || !exists {
		return false, err
	}
	if _, err := run(ctx, "systemctl", "disable", selfhostUnitName); err != nil {
		return true, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	return true, nil
}

// unitExists is false only when the file is absent; any other stat failure is an error, not "nothing to do".
func unitExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func removeCertificateUnits(ctx context.Context, run commander, unitDir, envFile string, removeData bool) (touched bool, err error) {
	var errs []error
	for _, unit := range certificateUnits {
		path := filepath.Join(unitDir, unit)
		exists, statErr := unitExists(path)
		if statErr != nil {
			errs = append(errs, statErr)
			continue
		}
		if !exists {
			continue
		}
		touched = true
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
		return touched, fmt.Errorf("%w: %w", commandError("could not remove the edge certificate timer and service; fix the cause and rerun pbh uninstall"), errors.Join(errs...))
	}
	return touched, nil
}
