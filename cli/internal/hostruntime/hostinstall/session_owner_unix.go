//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/updated"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func sessionOwnerCatalog(request Request) (sessionowner.Catalog, string, error) {
	if request.UID < 0 || request.GID < 0 || request.User == "" || request.Group == "" {
		return sessionowner.Catalog{}, "", ErrInvalidRequest
	}
	paths := platformPaths(request.UID)
	return sessionOwnerCatalogAt(paths, request), paths.instance, nil
}

func sessionOwnerCatalogAt(paths installPaths, request Request) sessionowner.Catalog {
	return sessionowner.Catalog{Root: filepath.Join(paths.root, "session-owners"), ConfigRoot: "/", User: request.User, Group: request.Group, Maximum: config.DefaultResources.MaxSessions + 1, Runner: service.ExecRunner{}}
}

func NativeSessionOwnerConfig(ctx context.Context, request Request) (sessionowner.Catalog, sessionowner.Descriptor, error) {
	if request.UID < 0 || request.GID < 0 || request.User == "" || request.Group == "" {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, ErrInvalidRequest
	}
	return nativeSessionOwnerConfigAt(ctx, request, platformPaths(request.UID))
}

// ForegroundSessionOwner is NativeSessionOwnerConfig for a host with no service manager (a container): the
// catalog and the pinned executable live under root, a protected directory of the caller's choosing, instead of
// the platform's install layout. It publishes nothing; the caller starts the owner.
func ForegroundSessionOwner(ctx context.Context, request Request, root string) (sessionowner.Catalog, sessionowner.Descriptor, error) {
	if request.UID < 0 || request.GID < 0 || request.User == "" || request.Group == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, ErrInvalidRequest
	}
	return nativeSessionOwnerConfigAt(ctx, request, foregroundPaths(request, root))
}

func foregroundPaths(request Request, root string) installPaths {
	return installPaths{instance: "u" + strconv.Itoa(request.UID), root: root, workerRollback: filepath.Join(root, "releases", "pb.rollback")}
}

// PruneForegroundSessionOwnerPins removes the pinned executables of replaced image versions: every pin that
// neither the running version nor a catalog descriptor names (the same sweep an install runs).
func PruneForegroundSessionOwnerPins(ctx context.Context, catalog sessionowner.Catalog, request Request, root string) error {
	return pruneSessionOwnerPinsAt(ctx, catalog, foregroundPaths(request, root), request.UID, request.StateRoot, request.Source.SHA256)
}

func nativeSessionOwnerConfigAt(ctx context.Context, request Request, paths installPaths) (sessionowner.Catalog, sessionowner.Descriptor, error) {
	if ctx == nil {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, ErrInvalidRequest
	}
	if os.Geteuid() != 0 {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, ErrNotPrivileged
	}
	if err := ctx.Err(); err != nil {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, err
	}
	if request.Source.Validate() != nil || request.Source.Platform != runtime.GOOS || request.Source.Architecture != runtime.GOARCH {
		return sessionowner.Catalog{}, sessionowner.Descriptor{}, ErrInvalidRequest
	}
	c, instance := sessionOwnerCatalogAt(paths, request), paths.instance
	pin := filepath.Join(filepath.Dir(paths.workerRollback), "pins", request.Source.SHA256, "pb")
	if _, err := os.Lstat(pin); err == nil {
		if err := request.Source.Verify(pin); err != nil {
			return c, sessionowner.Descriptor{}, err
		}
	} else if !installNotExistOnly(err) {
		return c, sessionowner.Descriptor{}, err
	} else if err := stageBinary(request.Executable, pin, request.Source); err != nil {
		return c, sessionowner.Descriptor{}, err
	}
	generation, err := sourceGeneration(request.Source)
	if err != nil {
		return c, sessionowner.Descriptor{}, err
	}
	endpointRoot := filepath.Join("/run", "paperboat-session-owner-"+instance+"-"+generation)
	if runtime.GOOS == "darwin" {
		ipcParent := filepath.Join("/Library/Application Support/Paperboat", "ipc")
		if err := secureRootDirectory(ipcParent, 0755); err != nil {
			return c, sessionowner.Descriptor{}, err
		}
		endpointRoot = filepath.Join(ipcParent, instance+"-"+generation)
	}

	d := sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: generation, Instance: instance, Source: request.Source, Executable: pin, RuntimeRoot: request.StateRoot, Endpoint: filepath.Join(endpointRoot, "control.sock"), UID: request.UID, GID: request.GID, Workspace: request.WorkspaceRoot, Shell: request.Shell}
	d, err = bindInitialSessionStore(ctx, c, d, request.SessionOwnerGeneration)
	if err != nil {
		return c, d, err
	}
	return c, d, sessionowner.PrepareNativeEndpoint(d)
}
func PrepareSessionOwner(ctx context.Context, request Request) error {
	c, d, err := NativeSessionOwnerConfig(ctx, request)
	if err != nil {
		return err
	}
	return startPreparedSessionOwner(ctx, c, d)
}

func InstalledSessionOwnerCoordinator(ctx context.Context, uid int) (sessionowner.Coordinator, error) {
	request, err := LoadEnrolledOwner(uid)
	if err != nil {
		return sessionowner.Coordinator{}, err
	}
	recorded, err := SessionOwnerRecorded(ctx, request)
	if err != nil {
		return sessionowner.Coordinator{}, err
	}
	if !recorded {
		return sessionowner.Coordinator{}, sessionowner.ErrInvalid
	}
	request.Executable = platformPaths(uid).worker
	if err := request.Source.Verify(request.Executable); err != nil {
		return sessionowner.Coordinator{}, err
	}
	c, d, err := NativeSessionOwnerConfig(ctx, request)
	paths := platformPaths(uid)
	installing := func() bool { return installationMayOwnCandidates(paths, uid, request.Source) }
	return sessionowner.Coordinator{Catalog: c, Active: d, Installing: installing}, err
}

// installationMayOwnCandidates reports an installation that may own a session owner newer than source: one holding
// the activation lock now (an install holds it for its whole run, however long), or one that committed since source
// was read. An unknown answer keeps the candidate.
func installationMayOwnCandidates(paths installPaths, uid int, source installsource.Source) bool {
	lock, err := updated.TryLockUnixNativeInstall(paths.updateState)
	if err != nil {
		return true
	}
	defer lock.Close()
	installed, err := loadInstallMetadata(paths.metadata, uid)
	return err != nil || installed.Source.SHA256 != source.SHA256
}

func InstalledSessionOwnerRecorded(ctx context.Context, uid int, source installsource.Source) (bool, error) {
	request, err := LoadEnrolledOwner(uid)
	if err != nil {
		return false, err
	}
	if request.Source.SHA256 != source.SHA256 || request.Source.Length != source.Length || request.Source.Version != source.Version {
		return false, ErrInvalidRequest
	}
	return SessionOwnerRecorded(ctx, request)
}

// carrierNotServingError reports a retained, verified carrier that did not become ready: it serves no
// streams (for example its pinned binary exits on current state), so an install may replace it after
// the drain instead of refusing; see replaceUnservingUnixCarrier.
type carrierNotServingError struct{ cause error }

func (e *carrierNotServingError) Error() string {
	return "the installed carrier is not serving; it will be replaced by the new version"
}
func (e *carrierNotServingError) Unwrap() error { return e.cause }

// preflightUnixCarrier refuses a running pre-carrier authority before mutation. A retained carrier that
// verifies but does not become ready returns *carrierNotServingError.
func preflightUnixCarrier(ctx context.Context, request Request, paths installPaths) (bool, error) {
	c, err := readCarrierConfigFile(ctx, paths.instance)
	if err == nil {
		installer, installErr := carrierInstaller(c)
		if installErr != nil {
			return false, installErr
		}
		native, ok := installerController(c).(service.NativeLifecycleController)
		if !ok {
			return false, service.ErrInvalidDefinition
		}
		status, inspectErr := native.Inspect(ctx, installer.DefinitionPath())
		if inspectErr != nil {
			return false, inspectErr
		}
		if status.Registered || status.Running {
			if err := c.Source.Verify(c.Executable); err != nil {
				return false, err
			}
			if err := verifyCarrierNativeCustody(c, installer); err != nil {
				return false, err
			}
			return false, carrierReadiness(ctx, waitCarrierReady(ctx, c))
		}
	}
	if err != nil && !installNotExistOnly(err) {
		return false, err
	}
	hostd, _, err := pendingInstallers(request, paths)
	if err != nil {
		return false, err
	}
	worker, err := legacyWorkerInstaller(request, paths)
	if err != nil {
		return false, err
	}
	checks := []struct {
		kind      string
		installer *service.Installer
	}{{service.HostdKind, hostd}, {service.WorkerKind, worker}}
	for _, check := range checks {
		var controller service.Controller
		if check.kind == service.HostdKind {
			generation, generationErr := sourceGeneration(request.Source)
			if generationErr != nil {
				return false, generationErr
			}
			controller, err = service.HostdController(request.Platform, generation, request.UID, service.ExecRunner{})
		} else if runtime.GOOS == "linux" {
			controller = service.SystemdController{Runner: service.ExecRunner{}, Unit: filepath.Base(check.installer.DefinitionPath())}
		} else {
			label := filepath.Base(check.installer.DefinitionPath())
			label = strings.TrimSuffix(label, ".plist")
			controller = service.LaunchdController{Runner: service.ExecRunner{}, UID: request.UID, Label: label}
		}
		if err != nil {
			return false, err
		}
		native, ok := controller.(service.NativeLifecycleController)
		if !ok {
			return false, service.ErrInvalidDefinition
		}
		status, err := native.Inspect(ctx, check.installer.DefinitionPath())
		if err != nil {
			return false, err
		}
		if status.Running {
			return false, ErrLegacyCarrierRunning
		}
	}
	return true, nil
}

func cleanupPreparedUnixCarrier(ctx context.Context, paths installPaths, cause error) error {
	c, err := readCarrierConfigFile(context.WithoutCancel(ctx), paths.instance)
	if err != nil {
		return errors.Join(cause, err)
	}
	installer, err := carrierInstaller(c)
	if err != nil {
		return errors.Join(cause, err)
	}
	return cleanupCarrierStartup(ctx, installer, cause)
}

// carrierReadiness turns "the retained carrier never answered" into *carrierNotServingError: its own
// readiness wait ran out while the control endpoint stayed unavailable and the install itself is still
// live. Any other failure (unreadable capability, a wrong pin or capability answering, the install's
// own deadline or cancellation) stays a hard failure, so a carrier that may be healthy is never replaced.
func carrierReadiness(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, carriercontrol.ErrUnavailable) {
		return err
	}
	return &carrierNotServingError{cause: err}
}

// replaceUnservingUnixCarrier swaps a retained carrier that did not become ready for the candidate's
// pin. Update would otherwise keep that carrier and wait for it forever, so a carrier broken by its own
// binary could never be updated (the test host 2026-10-22: an old pin that refused its trust file). It runs
// inside the lifecycle transaction after the drain; if the candidate carrier does not become ready the
// transaction fails and Restore puts the captured carrier back.
func replaceUnservingUnixCarrier(ctx context.Context, request Request, paths installPaths, carrierUnit []byte) error {
	retained, err := readCarrierConfigFile(ctx, paths.instance)
	if err != nil {
		return err
	}
	if err := removeCarrierGeneration(ctx, retained); err != nil {
		return err
	}
	if err := os.Remove(carrierConfigPath(paths.instance)); err != nil && !installNotExistOnly(err) {
		return err
	}
	return prepareCarrierLocked(ctx, request, carrierUnit)
}

// carrierUnit is the new carrier's unit as the build being installed rendered it (nil: this build's).
func prepareColdUnixCarrier(ctx context.Context, request Request, paths installPaths, carrierUnit []byte) error {
	retained, err := readCarrierConfigFile(ctx, paths.instance)
	if err == nil {
		return EnsureExistingCarrier(ctx, retained)
	}
	if !installNotExistOnly(err) {
		return err
	}
	return prepareCarrierLocked(ctx, request, carrierUnit)
}
