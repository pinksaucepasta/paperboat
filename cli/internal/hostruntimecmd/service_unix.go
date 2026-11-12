//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	if len(args) >= 1 && args[0] == "inventory" {
		if os.Geteuid() != 0 {
			return hostinstall.ErrNotPrivileged
		}
		if len(args) > 2 || len(args) == 2 && args[1] != "--check" {
			return serviceUsageError{"inventory accepts only --check"}
		}
		return writePurgeResult(ctx, stdout, runInstallInventory(ctx, invokingServiceUID(), len(args) == 2))
	}
	preserveCLI := len(args) == 2 && args[0] == "purge" && args[1] == "--preserve-cli"
	if (!preserveCLI && len(args) != 1) || args[0] != "install" && args[0] != "commit" && args[0] != "repair" && args[0] != "repair-persisted" && args[0] != "stop" && args[0] != "uninstall" && args[0] != "uninstall-persisted" && args[0] != "purge" && args[0] != "install-supplied" && args[0] != "commit-supplied" && args[0] != "rollback-supplied" {
		return serviceUsageError{"service requires install, commit, repair, repair-persisted, stop, uninstall, purge or inventory"}
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
		return withInstallInventory(ctx, hostinstall.RepairPersisted(ctx))
	}
	if args[0] == "purge" {
		removed, warnings, err := purgeSystemInstallationReport(ctx, preserveCLI)
		return writePurgeReport(ctx, stdout, err, removed, warnings)
	}
	request, err := hostinstall.Decode(stdin)
	if err != nil {
		return err
	}
	if args[0] == "install-supplied" || args[0] == "commit-supplied" || args[0] == "rollback-supplied" {
		var recover func(context.Context) error
		hooks := hostinstall.PreparedUpdateHooks{}
		if args[0] == "install-supplied" {
			// Bound to the request the installation runs with: an offline bundle's version is known only
			// once the helper has verified the bundle.
			hooks.Bind = func(resolved hostinstall.Request) (stage, validate func(context.Context) error) {
				stage, validate, recover = suppliedUpdateValidator(ctx, resolved)
				return stage, validate
			}
		}
		err := hostinstall.SuppliedBinary(ctx, request, args[0], hooks)
		if err != nil && recover != nil {
			err = errors.Join(err, recover(context.WithoutCancel(ctx)))
		}
		return err
	}
	if args[0] == "uninstall" {
		return hostinstall.Uninstall(ctx, request)
	}
	if args[0] == "commit" {
		return withInstallInventory(ctx, hostinstall.Commit(ctx, request))
	}
	if args[0] == "repair" {
		return withInstallInventory(ctx, hostinstall.Repair(ctx, request))
	}
	if args[0] == "stop" {
		return hostinstall.Stop(ctx, request)
	}
	// A successful install is finished by commit; a failed one ends here.
	if err := hostinstall.Install(ctx, request); err != nil {
		return withInstallInventory(ctx, err)
	}
	return nil
}

// withInstallInventory ends a root install operation, failed or not, with the
// install inventory; the operation's own failure stays first.
func withInstallInventory(ctx context.Context, err error) error {
	return errors.Join(err, runInstallInventory(context.WithoutCancel(ctx), invokingServiceUID(), false))
}

func authorizePersistedUninstall(ctx context.Context) error {
	executable := systemWorkerExecutable()
	command := sudoCommand(ctx, "--", "/usr/bin/env",
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
	uid, _ := machineguard.InvokingUID()
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
