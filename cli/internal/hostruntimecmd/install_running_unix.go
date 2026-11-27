//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	documentation "github.com/pinksaucepasta/paperboat/cli/docs"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/cli/internal/localdaemon"
)

// InstallRunningBinary installs this executable. controlURL is the selected server (pb --server); empty
// uses the configured one.
func InstallRunningBinary(ctx context.Context, executable string, source installsource.Source, directory, controlURL string) (string, error) {
	if NoSystemd() {
		if err := source.Verify(executable); err != nil {
			return "", err
		}
		return installBinaryOnly(executable, directory, os.Stderr)
	}
	return installUpdateSource(ctx, executable, source, "", false, directory, false, controlURL)
}
func InstallUpdateSource(ctx context.Context, executable string, source installsource.Source, bundlePath string, force bool, directory, controlURL string) (string, error) {
	return installUpdateSource(ctx, executable, source, bundlePath, force, directory, true, controlURL)
}

// supplyInstallRequest is the request every supplied install or update sends to the
// privileged service. ControlURL lets a fresh machine guard learn the deployment's
// local access domain from the server.
func supplyInstallRequest(account *user.User, uid, gid int, group, executable string, source installsource.Source, bundlePath string, force, explicitUpdate bool, controlURL string) hostinstall.Request {
	return hostinstall.Request{Schema: hostinstall.SchemaV1, Platform: runtime.GOOS, User: account.Username, UID: uid, GID: gid, Group: group, Home: account.HomeDir, Executable: executable, Source: source, EnrollmentPending: true, UpdateBundle: bundlePath, Force: force, SuppliedUpdate: explicitUpdate, ControlURL: controlURL}
}

func installUpdateSource(ctx context.Context, executable string, source installsource.Source, bundlePath string, force bool, directory string, explicitUpdate bool, controlURL string) (installedPath string, resultErr error) {
	if ctx == nil {
		return "", hostinstall.ErrInvalidRequest
	}
	if explicitUpdate || bundlePath != "" {
		ctx = context.WithValue(ctx, yieldInstallIO{}, true)
	}
	deadline, bounded := ctx.Deadline()
	budget := time.Until(deadline) // the caller's whole budget, given again to the steps after the privileged one
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
	var busy *hostinstall.MaintenanceBusyTimeoutError
	if errors.As(err, &busy) {
		return "", runtimeFailure("another Paperboat install or update is running on this computer; wait for it to finish, then run the command again", err)
	}
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
	// The daemon keeps the server it was paired with: --server, else the installed declaration's, else the configured one.
	declaredServer, _ := localdaemon.DeclaredServer(executable) // an unreadable declaration is rewritten by this install
	server := installControlURL(controlURL, installControlURL(declaredServer, cfg.ServerURL))
	if err := config.CheckDaemonSignIn(cfg, server); err != nil {
		if errors.Is(err, config.ErrCredentialStoreUnavailable) {
			return "", runtimeFailure(config.DaemonSignInHelp, err)
		}
		return "", err
	}
	request := supplyInstallRequest(account, uid, gid, group.Name, executable, source, bundlePath, force, explicitUpdate, server)
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
		// Its reason is also kept (bounded) in the error: scrollback was the only copy, and the local failure record
		// held just "exit status 1" (macOS in-place upgrade, L337).
		reason := &tailBuffer{limit: 2048}
		command.Stdout, command.Stderr = os.Stderr, io.MultiWriter(os.Stderr, reason)
		// The root child outlives a killed sudo and keeps the stderr pipe open; Run must still return at the bound.
		command.WaitDelay = privilegedPipeWaitDelay
		if err = command.Run(); errors.Is(err, exec.ErrWaitDelay) {
			err = nil // exited 0; a service it started kept the pipe
		}
		if err != nil && detachedLauncher([]string{"__runtime-service", operation}, account.Uid) != nil && detachedStepRunning(detachedUnit(operation, account.Uid)) {
			return errors.Join(errStepStillRunning, err)
		}
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				switch exitErr.ExitCode() {
				case MaintenanceBusyExitCode:
					return hostdproto.ErrMaintenanceBusy
				case UpdateVerificationExitCode:
					return hostinstall.ErrUpdateVerificationFailed
				case LegacyCarrierRunningExitCode:
					return hostinstall.ErrLegacyCarrierRunning
				case InsufficientSpaceExitCode:
					return hostinstall.ErrInsufficientSpace
				case UpdateNotReadyExitCode:
					return &hostinstall.UpdateNotReadyError{Step: notReadyStep(reason.String())}
				case NoPendingInstallationExitCode:
					return hostinstall.ErrNoPendingInstallation
				case RollbackIncompleteExitCode:
					return runtimeFailure(rollbackIncompleteMessage, service.ErrLifecycleRollbackIncomplete, stderrReason(reason))
				}
			}
			if refused := sudoRefusal("Installing the supplied Paperboat executable", err, reason.String()); refused != nil {
				return refused
			}
			// The privileged step prints its own reason first; administrator approval is only one cause.
			// --json shows nothing printed above, so a named step is carried in the error itself.
			if step := privilegedFailedStep(reason.String()); step != "" {
				failure := runtimeFailure("installing the supplied Paperboat executable failed at step: "+step+"; if administrator access was declined, approve it and rerun `pb install`", err, stderrReason(reason), runtimeContextFailure(callCtx)).(*CommandFailure)
				failure.step = step
				return failure
			}
			printed := ""
			if reason.String() != "" { // a step that wrote nothing left no reason above
				printed = " (its reason is printed above)"
			}
			return runtimeFailure("installing the supplied Paperboat executable failed"+printed+"; if administrator access was declined, approve it and rerun `pb install`", err, stderrReason(reason), runtimeContextFailure(callCtx))
		}
		return nil
	}
	// Every outcome ends with the install inventory: nothing of another version may stay on the host.
	// It has its own bound, so a caller without a deadline (or a recovery context) never waits on it unbounded.
	inventory := func(callCtx context.Context) error {
		callCtx, cancel := context.WithTimeout(callCtx, installInventoryTimeout)
		defer cancel()
		command := privilegedInstallCommand(callCtx, os.Geteuid(), rootHome, account.Uid, installerExecutable, "__runtime-service", "inventory")
		return runInventoryCommand(callCtx, command, installerExecutable)
	}
	if ctx.Err() != nil {
		// Cancelled before the privileged step: start nothing, with the same authored failure and cause a
		// cancelled start of it gave.
		return "", runtimeFailure("installing the supplied Paperboat executable failed; if administrator access was declined, approve it and rerun `pb install`", runtimeContextFailure(ctx))
	}
	// The privileged step is not held to the caller's deadline, like a package manager install (Tailscale
	// clientupdate/clientupdate.go:511 and :642 run apt-get and dnf without one): killing sudo at that deadline left
	// the root step finishing the install behind a reported failure (dadape, 2026-11-26: 3 min deadline, 4 min
	// install). It has its own generous bound; Ctrl-C still reaches it through the terminal's process group. The
	// steps after it get the caller's budget again.
	fmt.Fprintln(os.Stderr, "Installing Paperboat's system services; on a busy disk this can take a few minutes...")
	stepCtx, cancelStep := context.WithTimeout(context.WithoutCancel(ctx), suppliedInstallBound)
	err = invoke(stepCtx, "install-supplied")
	stepEnded := stepCtx.Err() != nil
	cancelStep()
	// As root without a transient service the kill ended the step itself: nothing finishes.
	if err != nil && (errors.Is(err, errStepStillRunning) || stepEnded && os.Geteuid() != 0 && detachedLauncher([]string{"__runtime-service", "install-supplied"}, account.Uid) == nil) {
		// The root child outlives its sudo and holds its own locks until it exits; it is not a failure yet.
		return "", StillFinishingFailure("The Paperboat install is still finishing in the background. Run `pb status` to see when it is done; run `pb install` again only if it reports no installation.", err)
	}
	if bounded {
		parent := ctx
		rest, cancelRest := context.WithTimeout(context.WithoutCancel(parent), budget)
		defer cancelRest()
		// A real cancellation (Ctrl-C) still ends the rest; the deadline the privileged step outlasted does not.
		defer context.AfterFunc(parent, func() {
			if !errors.Is(parent.Err(), context.DeadlineExceeded) {
				cancelRest()
			}
		})()
		ctx = rest
	}
	if err != nil {
		if ctx.Err() != nil {
			// Cancelled (Ctrl-C, or a caller that ended): start no new privileged child; the next pb install
			// runs the inventory.
			return "", err
		}
		if errors.Is(err, service.ErrLifecycleRollbackIncomplete) {
			// The previous version's files are still needed to finish the restore: no inventory may prune them.
			return "", err
		}
		return "", KeepInstallFailureOwner(err, inventory(ctx))
	}
	link, err := installWorkerCommand(directory, layout.Binary)
	var restoreService func(context.Context) error
	rollback := func(cause error) (string, error) {
		// rollback-supplied is never cut off where pb's kill would end it (macOS, no systemd-run). Killing its sudo at a bound either left the root child restoring
		// while pb restored over it, or, from a terminal, made sudo hang up the child mid-restore (device check
		// 11-27: sudo sends SIGHUP/SIGTERM to a child on its terminal), leaving the failed build and its journal.
		// It is waited for like a package manager's own rollback; past suppliedRollbackNotice pb says so once.
		// Ctrl-C still reaches it through the terminal's process group.
		notice := time.AfterFunc(suppliedRollbackNotice, func() {
			fmt.Fprintln(os.Stderr, "Still restoring the previous Paperboat version; waiting for it to finish...")
		})
		rollbackCtx, cancelRollback := context.WithoutCancel(ctx), context.CancelFunc(func() {})
		if detachedLauncher([]string{"__runtime-service", "rollback-supplied"}, account.Uid) != nil {
			// As a service it outlives pb, so a generous bound is safe here and frees unattended callers.
			rollbackCtx, cancelRollback = context.WithTimeout(rollbackCtx, suppliedRollbackBound)
		}
		binaryErr := invoke(rollbackCtx, "rollback-supplied")
		cancelRollback()
		notice.Stop()
		if errors.Is(binaryErr, errStepStillRunning) {
			// Nothing may restore over it: only the user's own command link is put back.
			var linkErr error
			if link != nil {
				linkErr = link.Rollback()
			}
			return "", StillFinishingFailure(stillRestoringMessage, cause, binaryErr, linkErr)
		}
		// The steps after it have their own budget: sharing one 45 s with it left them an expired context.
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), suppliedRecoveryBound)
		defer cancel()
		if errors.Is(binaryErr, hostinstall.ErrNoPendingInstallation) {
			// A bound update commits inside install-supplied, so there is no journal to undo and the new
			// version stays installed: keep its command link and service, and report the step that failed
			// instead of a rollback that never ran.
			var linkErr error
			if link != nil {
				linkErr = link.Commit()
			}
			failure := runtimeFailure("the new Paperboat version is installed, but a later step failed; run pb doctor to see why, then rerun pb install", cause, linkErr)
			// One owner: a bare join with the inventory's own error rendered as the generic "could not finish".
			return "", KeepInstallFailureOwner(failure, inventory(recovery))
		}
		var linkErr error
		if link != nil {
			linkErr = link.Rollback()
		}
		var serviceErr error
		if restoreService != nil {
			serviceErr = restoreService(recovery)
		}
		return "", KeepInstallFailureOwner(cause, errors.Join(binaryErr, serviceErr, linkErr, inventory(recovery)))
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
	if restoreService, err = localdaemon.ReplaceCurrentUserService(ctx, layout.Binary, cfg.Path(), server, source.Version); err != nil {
		return rollback(err)
	}
	// The same bound as the privileged validator: a new daemon may report starting while its first local-access
	// setup finishes (up to localdaemon's 45 s start-up grace); 30 s here rolled back a slow but healthy start.
	readyCtx, cancel := context.WithTimeout(ctx, suppliedUpdateReadyTimeout)
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
			return restoreAfterNotReady(os.Stderr, rollback, runtimeContextFailure(readyCtx), lastProbeErr)
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err = invoke(ctx, "commit-supplied"); errors.Is(err, errStepStillRunning) {
		return "", StillFinishingFailure("The Paperboat install is still being committed in the background. Run `pb install` again once it finishes to confirm it.", err)
	} else if err != nil {
		return "", runtimeFailure("installation commit could not be confirmed; keep the installed files and run pb doctor before retrying", err)
	}
	if err = link.Commit(); err != nil {
		return "", runtimeFailure("the new Paperboat version is installed, but its pb command link could not be finished; rerun pb install", err)
	}
	if err = retryInventory(ctx, inventory); err != nil {
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
// installInventoryTimeout bounds one privileged install inventory: stopping leftovers takes at most a few
// unit stops and a TERM/KILL wait, far below it.
const installInventoryTimeout = 2 * time.Minute

// suppliedRollbackNotice is when pb says it is still waiting for rollback-supplied (an activation lock wait, then
// stopping and restarting the prior services); suppliedRecoveryBound bounds the user service restore after it.
// Variables so tests can shorten them.
var (
	suppliedRollbackNotice = 2 * time.Minute
	suppliedRollbackBound  = 30 * time.Minute
	suppliedRecoveryBound  = 45 * time.Second
)

// systemdRun is the transient-unit launcher; tests replace it.
var systemdRun = "/usr/bin/systemd-run"

// detachedLauncher runs a Linux install, commit or rollback step as a transient systemd service (Tailscale's
// clientupdate does the same with systemd-run --wait --pipe --collect, feature/clientupdate/clientupdate.go:381). Under sudo the step was
// in sudo's terminal session: closing the terminal made sudo-rs kill it about 3 s later even with every signal
// ignored, and killing sudo at a bound hung it up (device checks 11-27, Ubuntu sudo-rs 0.2.13). As a service of PID 1
// it finishes whatever happens to pb, sudo or the terminal; --wait and --pipe keep its output, input and exit status
// for pb, and KillMode=process leaves anything it started running. macOS needs no launcher: its sudo never kills a
// child that ignores the hangup (service_unix.go ignores it), so the step stays a plain child there.
func detachedLauncher(arguments []string, invokingUID string) []string {
	if runtime.GOOS != "linux" || len(arguments) != 2 || arguments[0] != "__runtime-service" {
		return nil
	}
	switch arguments[1] {
	case "install-supplied", "commit-supplied", "rollback-supplied":
	default:
		return nil
	}
	if info, err := os.Stat(systemdRun); err != nil || !info.Mode().IsRegular() || systemctlPath() == "" || !systemdRunSupported() {
		return nil
	}
	// One fixed name per step and account: a second concurrent step is refused by systemd, and pb can ask whether
	// the step is still running after systemd-run itself ended (a Ctrl-C, or the bound killed it).
	return []string{systemdRun, "--quiet", "--wait", "--pipe", "--collect", "--property=KillMode=process", "--unit=" + detachedUnit(arguments[1], invokingUID)}
}

func detachedUnit(operation, invokingUID string) string {
	return "paperboat-" + operation + "-u" + invokingUID + ".service"
}

// systemdRunSupported reports systemd 236 or newer (--collect; --pipe is 235, --wait 232), as Tailscale checks
// (feature/clientupdate/clientupdate.go:402). Older hosts keep the plain sudo child. Tests replace it.
var systemdRunSupported = sync.OnceValue(func() bool {
	out, err := exec.Command(systemdRun, "--version").Output()
	fields := strings.Fields(string(out))
	if err != nil || len(fields) < 2 || fields[0] != "systemd" {
		return false
	}
	version, err := strconv.Atoi(fields[1])
	return err == nil && version >= 236
})

// detachedStepRunning reports whether a step's transient service is still running; tests replace it.
var detachedStepRunning = func(unit string) bool {
	systemctl := systemctlPath()
	if systemctl == "" {
		return true // cannot tell: never restore over a step that may still run (detachedLauncher requires it)
	}
	return exec.Command(systemctl, "is-active", "--quiet", unit).Run() == nil
}

// systemctlPath is where systemctl is (merged /usr or not), or empty: without it pb could not tell a running step,
// so the step is not detached at all. Tests replace it.
var systemctlPath = func() string {
	for _, systemctl := range []string{"/usr/bin/systemctl", "/bin/systemctl"} {
		if info, err := os.Stat(systemctl); err == nil && info.Mode().IsRegular() {
			return systemctl
		}
	}
	return ""
}

// stillRestoringMessage is what pb says when it stops waiting for a background restore (Ctrl-C, or its bound). The
// restore finishes on its own; `pb install` from this machine's ~/.local/bin/pb (already put back to the previous
// version) repairs anything it could not, which `pb doctor` reports.
const stillRestoringMessage = "The update failed and Paperboat is still restoring the previous version in the background. Run `pb status` to see when it is done, then `pb doctor`; if it reports a problem, run `pb install` to repair this computer's installation."

// restoreAfterNotReady says the restore starts before running it: the failure printed after it named an "attempt"
// that had already happened (L46).
func restoreAfterNotReady(out io.Writer, rollback func(error) (string, error), causes ...error) (string, error) {
	fmt.Fprintln(out, "The new Paperboat service did not become ready; restoring the previous installation...")
	return rollback(runtimeFailure("installed Paperboat service did not become ready", causes...))
}

// errStepStillRunning: pb stopped waiting (its bound, or Ctrl-C ended systemd-run) while the step's service runs on.
var errStepStillRunning = errors.New("the privileged step is still running in the background")

// yieldInstallIO marks an update's context: its privileged children run at idle I/O and nice 10 (e4586fcee).
type yieldInstallIO struct{}

// suppliedInstallBound is the outer bound on the privileged install step (Tailscale runs it with none); past it
// pb stops waiting and says the step is still finishing, because killing sudo cannot stop the root child.
var suppliedInstallBound = 15 * time.Minute

// privilegedInstallCommand is a variable so a workspace test can record the privileged children it would start.
var privilegedInstallCommand = func(ctx context.Context, effectiveUID int, rootHome, invokingUID, executable string, arguments ...string) *exec.Cmd {
	args := []string{"PAPERBOAT_INVOKING_UID=" + invokingUID}
	if diagnostics.TerminalHopsEnabled() {
		args = append(args, "PAPERBOAT_TERMINAL_HOPS=1")
	}
	if runtime.GOOS == "linux" && ctx.Value(yieldInstallIO{}) != nil {
		// Update preparation yields to live terminals; set priorities before Go starts so every installer thread
		// and child inherits them. A fresh pb install has no sessions to protect and runs at normal priority.
		args = append(args, "/usr/bin/ionice", "-c", service.UpdateIOClass, "/usr/bin/nice", "-n", service.UpdateNice)
	}
	args = append(args, executable)
	args = append(args, arguments...)
	launcher := []string{"/usr/bin/env"}
	if transient := detachedLauncher(arguments, invokingUID); transient != nil {
		// The service manager runs the step, not sudo's session: see detachedLauncher.
		// A service has no sudo -H: name root's home itself (the caller passes it only when pb runs as root).
		home := rootHome
		if home == "" {
			home = "/root"
			if account, err := user.LookupId("0"); err == nil && account.HomeDir != "" {
				home = account.HomeDir
			}
		}
		launcher = append(transient, "/usr/bin/env", "HOME="+home, "USER=root", "LOGNAME=root")
	}
	if effectiveUID == 0 {
		args = append([]string{"HOME=" + rootHome, "USER=root", "LOGNAME=root"}, args...)
		return exec.CommandContext(ctx, launcher[0], append(launcher[1:], args...)...)
	}
	sudo := append([]string{"-H", "--"}, launcher...)
	if os.Getenv("PB_RECIPE") != "" {
		// A recipe install never asks for a password (PLAN F26): without passwordless sudo it fails at once.
		sudo = append([]string{"-n"}, sudo...)
	}
	return exec.CommandContext(ctx, "/usr/bin/sudo", append(sudo, args...)...)
}

// installControlURL is the server the privileged install asks for the local access domain: the one
// selected with --server, else the configured one.
func installControlURL(selected, configured string) string {
	if selected != "" {
		return selected
	}
	return configured
}

// KeepInstallFailureOwner adds the inventory's failure to a failed install without making it a second owner: the
// inventory is privileged too, so it fails for the same reason (a non-admin user has no sudo) and a plain join of
// the two left no sole owner, which turned the authored refusal (F1-07) into "could not finish the command". The
// authored failure keeps the prose, extended by the inventory's own authored sentence when it has one (an older
// service that cannot run the inventory names its version); the inventory failure stays among the inspectable causes.
func KeepInstallFailureOwner(installErr, inventoryErr error) error {
	if inventoryErr == nil {
		return installErr
	}
	if failure, ok := installErr.(*CommandFailure); ok {
		message := failure.message
		var inventoryRefused, installRefused *AdministratorRequiredError
		bothRefused := errors.As(inventoryErr, &inventoryRefused) && errors.As(installErr, &installRefused)
		if authored, ok := inventoryErr.(*CommandFailure); ok && authored.message != "" && !bothRefused {
			message += "\n" + authored.message
		}
		return &CommandFailure{message: message, causes: append(append([]error(nil), failure.causes...), inventoryErr)}
	}
	return errors.Join(installErr, inventoryErr)
}

// stderrReason is what the privileged step printed last, as a cause (nil when it printed nothing).
// notReadyStep is the failed step the privileged step printed last ("pb: cause: <step>"); empty when it printed none.
func notReadyStep(stderr string) string {
	step := ""
	for _, line := range strings.Split(stderr, "\n") {
		if cause, ok := strings.CutPrefix(strings.TrimSpace(line), "pb: cause: "); ok {
			step = cause
		}
	}
	return step
}

// privilegedFailedStep is the step the privileged child named: its last "pb: cause:" line, else its last
// "pb: Paperboat installation failed: <step>. Run pb doctor, then retry." line; printable and at most 200 bytes.
func privilegedFailedStep(stderr string) string {
	step := notReadyStep(stderr)
	if step == "" {
		for _, line := range strings.Split(stderr, "\n") {
			if text, ok := strings.CutPrefix(strings.TrimSpace(line), "pb: Paperboat installation failed: "); ok {
				step = strings.TrimSuffix(strings.TrimSuffix(text, " Run pb doctor, then retry."), ".")
			}
		}
	}
	step = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, step)
	if len(step) > 200 {
		step = strings.ToValidUTF8(step[:200], "")
	}
	return step
}

func stderrReason(reason *tailBuffer) error {
	if text := reason.String(); text != "" {
		return errors.New("privileged step: " + text)
	}
	return nil
}
