//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	documentation "github.com/pinksaucepasta/paperboat/docs"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

func InstallRunningBinary(ctx context.Context, executable string, source installsource.Source, directory string) (string, error) {
	return installUpdateSource(ctx, executable, source, "", false, directory, false)
}
func InstallUpdateSource(ctx context.Context, executable string, source installsource.Source, bundlePath string, force bool, directory string) (string, error) {
	return installUpdateSource(ctx, executable, source, bundlePath, force, directory, true)
}
// supplyInstallRequest is the request every supplied install or update sends to the
// privileged service. ControlURL lets a fresh machine guard learn the deployment's
// local access domain from the server.
func supplyInstallRequest(account *user.User, uid, gid int, group, executable string, source installsource.Source, bundlePath string, force, explicitUpdate bool, controlURL string) hostinstall.Request {
	return hostinstall.Request{Schema: hostinstall.SchemaV1, Platform: runtime.GOOS, User: account.Username, UID: uid, GID: gid, Group: group, Home: account.HomeDir, Executable: executable, Source: source, EnrollmentPending: true, UpdateBundle: bundlePath, Force: force, SuppliedUpdate: explicitUpdate, ControlURL: controlURL}
}

func installUpdateSource(ctx context.Context, executable string, source installsource.Source, bundlePath string, force bool, directory string, explicitUpdate bool) (installedPath string, resultErr error) {
	if ctx == nil {
		return "", hostinstall.ErrInvalidRequest
	}
	if bundlePath == "" {
		if explicitUpdate {
			source.Distribution = installsource.Custom
			source.AutomaticUpdates = false
		}
		if err := source.Verify(executable); err != nil {
			return "", err
		}
	}
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return "", err
	}
	if bundlePath == "" {
		if err := hostinstall.CheckSuppliedExecutable(executable, uid); err != nil {
			return "", err
		}
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return "", err
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		return "", err
	}
	rootHome := ""
	if os.Geteuid() == 0 {
		rootAccount, lookupErr := user.LookupId("0")
		if lookupErr != nil {
			return "", lookupErr
		}
		rootHome = rootAccount.HomeDir
	}
	if directory == "" {
		directory = filepath.Join(account.HomeDir, ".local", "bin")
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", errors.New("install directory must be an absolute canonical path")
	}
	layout, err := service.UserLayout(runtime.GOOS, uid)
	if err != nil {
		return "", err
	}
	paths, err := localdaemon.CurrentUserPaths()
	if err != nil {
		return "", err
	}
	lock, err := hostinstall.LockSuppliedOperation(filepath.Join(paths.StateRoot, "install.lock"), uid)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := lock.Close(); err != nil {
			resultErr = withCleanup("installation finished or failed, but releasing its operation lock failed", resultErr, err)
		}
	}()
	cfg, err := config.Load("")
	if err != nil {
		return "", err
	}
	request := supplyInstallRequest(account, uid, gid, group.Name, executable, source, bundlePath, force, explicitUpdate, cfg.ServerURL)
	installerExecutable, err := os.Executable()
	if err != nil {
		return "", err
	}
	invoke := func(callCtx context.Context, operation string) error {
		body, err := json.Marshal(request)
		if err != nil {
			return err
		}
		command := privilegedInstallCommand(callCtx, os.Geteuid(), rootHome, account.Uid, installerExecutable, "__runtime-service", operation)
		command.Stdin = bytes.NewReader(body)
		command.Stdout, command.Stderr = os.Stderr, os.Stderr
		if err = command.Run(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				switch exitErr.ExitCode() {
				case MaintenanceBusyExitCode:
					return hostdproto.ErrMaintenanceBusy
				case UpdateVerificationExitCode:
					return hostinstall.ErrUpdateVerificationFailed
				case LegacyCarrierRunningExitCode:
					return hostinstall.ErrLegacyCarrierRunning
				}
			}
			return runtimeFailure("installing the supplied Paperboat executable failed; approve administrator access when prompted, then rerun `pb install`", err, runtimeContextFailure(callCtx))
		}
		return nil
	}
	if err = invoke(ctx, "install-supplied"); err != nil {
		return "", err
	}
	link, err := installWorkerCommand(directory, layout.Binary)
	var restoreService func(context.Context) error
	rollback := func(cause error) (string, error) {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		var linkErr error
		if link != nil {
			linkErr = link.Rollback()
		}
		binaryErr := invoke(recovery, "rollback-supplied")
		var serviceErr error
		if restoreService != nil {
			serviceErr = restoreService(recovery)
		}
		return "", errors.Join(cause, binaryErr, serviceErr, linkErr)
	}
	if err != nil {
		return rollback(err)
	}
	if bundlePath != "" {
		source, err = installsource.InspectCandidate(ctx, layout.Binary)
		if err != nil {
			return rollback(fmt.Errorf("installed bundle identity could not be read: %w", err))
		}
		// The request sent so far named no source (a bundle's source is only known once
		// install-supplied has unpacked it); commit-supplied must verify the installed one.
		request = suppliedCommitRequest(request, source)
	}
	if restoreService, err = localdaemon.ReplaceCurrentUserService(ctx, layout.Binary, cfg.Path(), cfg.ServerURL); err != nil {
		return rollback(err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var lastProbeErr error
	for {
		probe, probeErr := localdaemon.ProbeCurrentUserForUpdate(readyCtx)
		if probeErr == nil && probe.Running && probe.Version == source.Version && probe.State != "starting" && probe.State != "stopping" {
			break
		}
		if readyCtx.Err() == nil {
			lastProbeErr = probeErr
		}
		select {
		case <-readyCtx.Done():
			return rollback(runtimeFailure("installed Paperboat service did not become ready; attempting to restore the previous installation", runtimeContextFailure(readyCtx), lastProbeErr))
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err = invoke(ctx, "commit-supplied"); err != nil {
		return "", runtimeFailure("installation commit could not be confirmed; keep the installed files and run pb doctor before retrying", err)
	}
	if err = link.Commit(); err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		manuals := privilegedInstallCommand(ctx, os.Geteuid(), rootHome, account.Uid, layout.Binary, "--no-customization", "__man-pages", "--directory", "/usr/local/share/man")
		manuals.Stdin, manuals.Stdout, manuals.Stderr = os.Stdin, os.Stderr, os.Stderr
		err = manuals.Run()
	} else {
		err = documentation.Install(filepath.Join(filepath.Dir(directory), "share", "man"))
	}
	if err != nil {
		return "", runtimeFailure("Paperboat is installed, but its manuals could not be installed; retry pb install to repair them", err)
	}
	installed := filepath.Join(directory, "pb")
	return installed, nil
}

// suppliedCommitRequest is the request for commit-supplied: the installation request with the
// identity of the executable that was actually installed. The privileged helper verifies that
// identity against the installed binary and its install record, so an empty or stale source
// fails an installation that already succeeded (F14-08). A bundle is official however its
// executable inspects: the inspection cannot know the signed provenance, and a source that
// differs from the one the install step journaled makes the commit refuse.
func suppliedCommitRequest(request hostinstall.Request, installed installsource.Source) hostinstall.Request {
	if request.UpdateBundle != "" {
		installed.Distribution, installed.AutomaticUpdates = installsource.Official, true
	}
	request.Source = installed
	return request
}

// Diagnostic storage belongs to the child's effective identity. Keep the
// enrolled identity in the supplied request and invoking UID, never in HOME.
func privilegedInstallCommand(ctx context.Context, effectiveUID int, rootHome, invokingUID, executable string, arguments ...string) *exec.Cmd {
	args := []string{"PAPERBOAT_INVOKING_UID=" + invokingUID, executable}
	args = append(args, arguments...)
	if effectiveUID == 0 {
		args = append([]string{"HOME=" + rootHome, "USER=root", "LOGNAME=root"}, args...)
		return exec.CommandContext(ctx, "/usr/bin/env", args...)
	}
	args = append([]string{"-H", "--", "/usr/bin/env"}, args...)
	return exec.CommandContext(ctx, "/usr/bin/sudo", args...)
}
