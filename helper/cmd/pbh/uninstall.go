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
	removeData := fs.Bool("remove-data", false, "also delete protected identity, credentials and configuration")
	confirm := fs.Bool("confirm-remove-data", false, "explicitly confirm permanent data deletion")
	if done, err := parseFlags(fs, args, out); done {
		return err
	}
	if fs.NArg() != 0 {
		return usage("unexpected uninstall arguments")
	}
	if *removeData != *confirm {
		return usage("data deletion requires both --remove-data and --confirm-remove-data; omit both to retain state")
	}
	if !isRoot() {
		return elevate(ctx, append([]string{"uninstall"}, args...), out)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return runUninstall(ctx, uninstallEnvFor(), *removeData, out)
}

// isRoot and uninstallEnvFor are what the command line wires to the real host; tests replace them.
var (
	isRoot          = func() bool { return os.Geteuid() == 0 }
	elevate         = elevated
	uninstallEnvFor = defaultUninstallEnv
)

// uninstallEnv is everything uninstall touches outside its own logic, so a test points it at a temp tree and fakes
// the commands.
type uninstallEnv struct {
	systemctl, run      commander
	unitDir, runtimeDir string
	paths               hostPaths
	binaries, binary    string
	teardown            func(context.Context) error
	removeLocal         func() error
}

func defaultUninstallEnv() uninstallEnv {
	return uninstallEnv{
		systemctl: systemctlCommander, run: execCommander,
		unitDir: systemUnitDir, runtimeDir: systemdRuntimeDir,
		paths: defaultHostPaths(defaultState), binaries: defaultBinaries, binary: "/usr/local/bin/pbh",
		teardown:    func(ctx context.Context) error { return containers.Teardown(ctx, defaultState) },
		removeLocal: func() error { return selfhost.RemoveLocal(defaultState) },
	}
}

// runUninstall is safe to rerun: a step that already happened is skipped, a failed one leaves what a rerun needs.
func runUninstall(ctx context.Context, env uninstallEnv, removeData bool, out io.Writer) error {
	touched, err := stopServices(ctx, env.systemctl, env.unitDir)
	if err != nil {
		return err
	}
	if err := removeHostNetwork(ctx, env.run, env.paths, out); err != nil {
		return err
	}
	if err := env.teardown(ctx); err != nil {
		return err
	}
	removed, err := removeMainUnit(ctx, env.systemctl, env.unitDir)
	if err != nil {
		return err
	}
	if err := reloadSystemd(ctx, env.systemctl, touched || removed, env.runtimeDir); err != nil {
		return err
	}
	if err := os.RemoveAll(env.binaries); err != nil {
		return err
	}
	if err := os.Remove(env.binary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if removeData {
		if err := env.removeLocal(); err != nil {
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
	systemUnitDir    = "/etc/systemd/system"
	selfhostUnitName = "paperboat-selfhost.service"
)

// stopServices stops the service so nothing starts the node again. An absent unit is an expected state on a rerun;
// touched reports it existed.
func stopServices(ctx context.Context, run commander, unitDir string) (touched bool, err error) {
	exists, err := unitExists(filepath.Join(unitDir, selfhostUnitName))
	if err != nil || !exists {
		return false, err
	}
	if _, err := run(ctx, "systemctl", "stop", selfhostUnitName); err != nil {
		return true, fmt.Errorf("%w: %w", commandError("could not stop self-host service; rerun pbh uninstall after fixing the cause"), err)
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
		return true, fmt.Errorf("%w: %w", commandError("could not disable the self-host service; containers and firewall rules are already removed, rerun pbh uninstall"), err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	return true, nil
}

// systemctlCommander runs systemctl under the uninstall deadline itself (execCommander caps a command at 20 s,
// which is for host-network commands).
func systemctlCommander(ctx context.Context, name string, args ...string) (string, error) {
	out, err := localeC(exec.CommandContext(ctx, name, args...)).CombinedOutput()
	return string(out), err
}

// systemdRuntimeDir exists while systemd is the running init.
const systemdRuntimeDir = "/run/systemd/system"

// reloadSystemd reloads unit files after units were removed, and also on a rerun where an earlier reload failed
// after the files were gone; a host without systemd (--no-service) has nothing to reload.
func reloadSystemd(ctx context.Context, run commander, unitsRemoved bool, runtimeDir string) error {
	if !unitsRemoved {
		if _, err := os.Stat(runtimeDir); err != nil {
			return nil
		}
	}
	if _, err := run(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("%w: %w", commandError("could not reload systemd; rerun pbh uninstall"), err)
	}
	return nil
}

// unitExists is false only when the file is absent; any other stat failure is an error, not "nothing to do".
func unitExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
