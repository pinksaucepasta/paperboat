//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"time"

	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

// A standard user needs the Windows administrator prompt, which a non-interactive logon (scheduled task, service) cannot show.
const nonInteractiveInstallRefusal = "installing the supplied Paperboat executable needs administrator access, and this session cannot show the Windows approval prompt; run `pb install` from a signed-in desktop session and approve it"

var consentPromptAvailable = func() bool { return elevation.IsCurrentProcessElevated() || elevation.ConsentPromptAvailable() }

// InstallRunningBinary stages caller-supplied bytes into Windows' fixed,
// per-user machine-wide slot. Account enrollment remains a separate action.
func InstallRunningBinary(ctx context.Context, sourcePath string, source installsource.Source, installDir, controlURL string) (string, error) {
	return installUpdateSource(ctx, sourcePath, source, "", false, installDir, false, controlURL)
}
func InstallUpdateSource(ctx context.Context, sourcePath string, source installsource.Source, bundlePath string, force bool, installDir, controlURL string) (string, error) {
	return installUpdateSource(ctx, sourcePath, source, bundlePath, force, installDir, true, controlURL)
}
func installUpdateSource(ctx context.Context, sourcePath string, source installsource.Source, bundlePath string, force bool, installDir string, explicitUpdate bool, controlURL string) (installedPath string, resultErr error) {
	if !consentPromptAvailable() {
		return "", runtimeFailure(nonInteractiveInstallRefusal)
	}
	if ctx == nil || bundlePath == "" && (!filepath.IsAbs(sourcePath) || filepath.Clean(sourcePath) != sourcePath) || installDir != "" {
		return "", errors.New("invalid Windows installation source")
	}
	if bundlePath == "" {
		if explicitUpdate {
			source.Distribution = installsource.Custom
			source.AutomaticUpdates = false
		}
		if err := source.Validate(); err != nil || source.Platform != "windows" || source.Architecture != runtime.GOARCH {
			return "", runtimeFailure("invalid Windows installation source identity", err)
		}
		if err := source.Verify(sourcePath); err != nil {
			return "", err
		}
	}
	account, err := user.Current()
	if err != nil || account.Username == "" {
		return "", runtimeFailure("could not resolve Windows installation owner", err)
	}
	sid, err := currentBootstrapSID()
	if err != nil {
		return "", err
	}
	instance, err := hostinstall.WindowsInstanceForSID(sid)
	if err != nil {
		return "", err
	}
	layout, err := hostinstall.WindowsLayoutForInstance(instance)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	stateRoot, err := helperconfig.DefaultStateRoot(os.Getenv)
	if err != nil {
		return "", err
	}
	client, err := clientconfig.Load("")
	if err != nil {
		return "", err
	}
	request := hostinstall.Request{Schema: hostinstall.SchemaV1, Platform: "windows", User: windowsAccountName(account.Username), Group: "Paperboat", OwnerSID: sid, Executable: sourcePath, Source: source, StateRoot: stateRoot, WorkspaceRoot: home, ControlURL: firstNonEmpty(controlURL, client.ServerURL), EnrollmentPending: true, UpdateBundle: bundlePath, Force: force, SuppliedUpdate: explicitUpdate}
	action := elevation.ActionInstall
	if existing, loadErr := hostinstall.LoadWindowsRuntimeConfigForInstance(instance); loadErr == nil {
		if !existing.EnrollmentPending {
			request.Artifact = existing.Artifact
			request.Artifact.Version = source.Version
			request.Home = home
			request.Path = os.Getenv("PATH")
			request.StateRoot = existing.StateRoot
			request.WorkspaceRoot = existing.Workspace
			request.ControlURL = firstNonEmpty(controlURL, existing.ControlURL)
			request.UserMachineID = existing.MachineID
			request.Shell = filepath.Join(os.Getenv("WINDIR"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
			request.HelperListenAddress = existing.ListenAddress
			request.EnrollmentPending = existing.EnrollmentPending
			action = elevation.ActionInstallCommit
		}
	} else if !runtimeCausesOnly(loadErr, os.ErrNotExist, hostinstall.ErrNotInstalled) {
		return "", loadErr
	}
	installerExecutable, err := os.Executable()
	if err != nil {
		return "", err
	}
	elevatedPath, cleanup, err := stageWindowsElevationExecutable(installerExecutable)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := cleanup(); err != nil {
			resultErr = withCleanup("installation finished or failed, but removing its temporary executable failed", resultErr, err)
		}
	}()
	if err := elevation.RunRuntimeService(ctx, elevatedPath, action, request); err != nil {
		if remote, ok := err.(*elevation.RemoteError); ok {
			switch remote.Code {
			case "maintenance_busy":
				return "", hostdproto.ErrMaintenanceBusy
			case "update_verification_failed":
				return "", hostinstall.ErrUpdateVerificationFailed
			case "legacy_carrier_running":
				return "", hostinstall.ErrLegacyCarrierRunning
			}
		}
		return "", err
	}
	return layout.Binary, nil
}

const (
	elevationExecutablePattern = ".pb-elevated-*.exe"
	// A copy older than this belongs to an installer that was killed (or whose helper outlived it): no live
	// installation takes this long to start its helper.
	staleElevationExecutableAge = time.Hour
	// A helper that was canceled or timed out is still exiting when the installer returns, and Windows refuses to
	// delete an image that is running: the removal waits for it, within this bound.
	elevationExecutableRemoveAttempts = 20
	elevationExecutableRemoveWait     = 500 * time.Millisecond
)

// sweepStaleElevationExecutables deletes the elevation-helper copies an earlier installer left in dir. A copy that
// is still running, or is too young to be stale, stays (the removal of a running image fails and is ignored).
func sweepStaleElevationExecutables(dir string, now time.Time) {
	matches, err := filepath.Glob(filepath.Join(dir, elevationExecutablePattern))
	if err != nil {
		return
	}
	for _, path := range matches {
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > staleElevationExecutableAge {
			_ = os.Remove(path)
		}
	}
}

// removeElevationExecutable deletes path, retrying while its helper is still exiting. A copy that stays is swept by
// the next installation.
func removeElevationExecutable(path string) error {
	var err error
	for attempt := 0; attempt < elevationExecutableRemoveAttempts; attempt++ {
		if err = os.Remove(path); err == nil || runtimeCausesOnly(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(elevationExecutableRemoveWait)
	}
	return err
}

func stageWindowsElevationExecutable(sourcePath string) (string, func() error, error) {
	sweepStaleElevationExecutables(os.TempDir(), time.Now())
	temporary, err := os.CreateTemp("", elevationExecutablePattern)
	if err != nil {
		return "", func() error { return nil }, runtimeFailure("creating the elevation executable failed", err)
	}
	path := temporary.Name()
	cleanup := func() error { return removeElevationExecutable(path) }
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", func() error { return nil }, runtimeFailure("opening the elevation source failed", err, temporary.Close(), cleanup())
	}
	_, copyErr := io.Copy(temporary, source)
	resultErr := runtimeFailureIfAny("copying the elevation executable failed", copyErr, source.Close(), temporary.Close())
	if resultErr != nil {
		return "", func() error { return nil }, withCleanup("copying or cleaning the elevation executable failed", resultErr, cleanup())
	}
	return path, cleanup, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
