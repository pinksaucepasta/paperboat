//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

func runServiceCommand(ctx context.Context, args []string, stdin io.Reader, stdout, _ io.Writer) error {
	// Privileged service operations create shared directories with explicit
	// modes. sudo preserves the caller's umask, so fix it for this process to
	// make installed parents independent of whoever invoked the operation.
	if os.Geteuid() == 0 {
		syscall.Umask(0o022)
	}
	if len(args) == 1 && args[0] == "browser-domain" {
		if os.Geteuid() != 0 {
			return hostinstall.ErrNotPrivileged
		}
		request, err := decodeBrowserDomain(stdin)
		if err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		_, err = machineguard.InstallBrowserDomain(ctx, executable, request.Owner, request.Domain)
		return err
	}
	preserveCLI := len(args) == 2 && args[0] == "purge" && args[1] == "--preserve-cli"
	if (!preserveCLI && len(args) != 1) || args[0] != "install" && args[0] != "commit" && args[0] != "repair" && args[0] != "repair-persisted" && args[0] != "stop" && args[0] != "uninstall" && args[0] != "uninstall-persisted" && args[0] != "purge" && args[0] != "install-supplied" && args[0] != "commit-supplied" && args[0] != "rollback-supplied" {
		return errors.New("service requires install, commit, repair, repair-persisted, stop, or uninstall")
	}
	if args[0] == "uninstall" && os.Geteuid() != 0 {
		if _, err := os.Stat(systemWorkerExecutable()); runtimeCausesOnly(err, os.ErrNotExist) {
			if err := machineguard.CleanupUserTrust(ctx); err != nil {
				return err
			}
			return removeSystemWorkerCommand()
		} else if err != nil {
			return err
		}
		if err := authorizePersistedUninstall(ctx); err != nil {
			return err
		}
		if err := machineguard.CleanupUserTrust(ctx); err != nil {
			return fmt.Errorf("remove browser certificate trust: %w", err)
		}
		return removeSystemWorkerCommand()
	}
	if os.Geteuid() != 0 {
		return hostinstall.ErrNotPrivileged
	}
	if args[0] == "uninstall-persisted" {
		return hostinstall.UninstallPersisted(ctx)
	}
	if args[0] == "repair-persisted" {
		return hostinstall.RepairPersisted(ctx)
	}
	if args[0] == "purge" {
		return writePurgeResult(ctx, stdout, purgeSystemInstallationScoped(ctx, preserveCLI))
	}
	request, err := hostinstall.Decode(stdin)
	if err != nil {
		return err
	}
	if args[0] == "install-supplied" || args[0] == "commit-supplied" || args[0] == "rollback-supplied" {
		var validate, recover func(context.Context) error
		if args[0] == "install-supplied" {
			validate, recover = suppliedUpdateValidator(ctx, request)
		}
		err := hostinstall.SuppliedBinary(ctx, request, args[0], validate)
		if err != nil && recover != nil {
			err = errors.Join(err, recover(context.WithoutCancel(ctx)))
		}
		return err
	}
	if args[0] == "uninstall" {
		return hostinstall.Uninstall(ctx, request)
	}
	if args[0] == "commit" {
		return hostinstall.Commit(ctx, request)
	}
	if args[0] == "repair" {
		return hostinstall.Repair(ctx, request)
	}
	if args[0] == "stop" {
		return hostinstall.Stop(ctx, request)
	}
	return hostinstall.Install(ctx, request)
}

func authorizePersistedUninstall(ctx context.Context) error {
	executable := systemWorkerExecutable()
	command := exec.CommandContext(ctx, "/usr/bin/sudo", "--", "/usr/bin/env",
		"PAPERBOAT_INVOKING_UID="+strconv.Itoa(os.Getuid()), executable, "__runtime-service", "uninstall-persisted")
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return runtimeFailure("administrator approval or service removal failed", err, runtimeContextFailure(ctx))
	}
	return nil
}

func systemWorkerExecutable() string {
	uid := os.Getuid()
	if os.Geteuid() == 0 {
		uid = invokingServiceUID()
	}
	layout, _ := service.UserLayout(runtime.GOOS, uid)
	return layout.Binary
}

func invokingServiceUID() int {
	value := os.Getenv("PAPERBOAT_INVOKING_UID")
	if value == "" {
		value = os.Getenv("SUDO_UID")
	}
	uid, err := strconv.Atoi(value)
	if err != nil || uid < 0 {
		return 0
	}
	return uid
}

func removeSystemWorkerCommand() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	commandPath := filepath.Join(home, ".local", "bin", "pb")
	info, err := os.Lstat(commandPath)
	if runtimeCausesOnly(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, err := os.Readlink(commandPath)
	if err != nil {
		return err
	}
	if target != systemWorkerExecutable() {
		return nil
	}
	return os.Remove(commandPath)
}
