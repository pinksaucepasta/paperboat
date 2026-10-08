//go:build windows

package updated

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/binarytarget"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesignature"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsActivatorService = "PaperboatUpdateActivator"
	windowsHostdService     = "PaperboatHostd"
	windowsUpdaterService   = "PaperboatUpdated"
	windowsSSHService       = "PaperboatSshd"
)

func windowsInstanceServiceNames(ownerSID string) (hostd, updater string, err error) {
	instance, err := service.WindowsUserInstance(ownerSID)
	if err != nil {
		return "", "", err
	}
	return windowsHostdService + "-" + instance, windowsUpdaterService + "-" + instance, nil
}

func windowsInstanceNames(ownerSID string) (instance, hostd, updater, ssh, activator string, err error) {
	instance, err = service.WindowsUserInstance(ownerSID)
	if err != nil {
		return "", "", "", "", "", err
	}
	return instance, windowsHostdService + "-" + instance, windowsUpdaterService + "-" + instance, windowsSSHService + "-" + instance, windowsActivatorService + "-" + instance, nil
}

// windowsReleasePaths is retained only as an internal transaction view while
// the Windows service controller is being collapsed onto the canonical pb
// slot. It is never exposed by service.Layout or written into service
// definitions.
type windowsReleasePaths struct {
	Root, Runtime, CLI, Hostd, Updater string
}

func canonicalWindowsRelease(layout service.Layout, version string) (windowsReleasePaths, error) {
	if version == "" || filepath.Base(version) != version || strings.ContainsAny(version, "/\\\x00\r\n") || version == "." || version == ".." {
		return windowsReleasePaths{}, errInvalidWindowsActivation
	}
	root := filepath.Join(layout.ReleasesRoot, "versions", version)
	return windowsReleasePaths{
		Root: root, Runtime: filepath.Join(root, "pb.exe"), CLI: filepath.Join(root, "pb.exe"),
		Hostd: filepath.Join(root, "pb.exe"), Updater: filepath.Join(root, "pb.exe"),
	}, nil
}

func windowsActivationJournalPath(stateRoot string) string {
	return filepath.Join(stateRoot, "activation", "journal.json")
}

func stageWindowsActivation(ctx context.Context, config WindowsConfig, release workerupdate.Release) (windowsActivationJournal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !exactReleasePattern.MatchString(release.Version) || release.Platform != "windows" || release.Architecture != config.Architecture {
		return windowsActivationJournal{}, workerupdate.ErrInvalidRelease
	}
	// The signed deployment policy is part of the crash journal. Validate it
	// before creating any immutable release files so a malformed canary/drain
	// policy cannot strand a partially staged transaction.
	if err := workerupdate.ValidateActivationRelease(release); err != nil {
		return windowsActivationJournal{}, err
	}
	candidate, err := workerupdate.PreparedCandidateForRelease(release)
	if err != nil {
		return windowsActivationJournal{}, err
	}
	if prior, loadErr := loadWindowsActivationJournal(config); loadErr == nil && prior.Stage == windowsActivationAwaitingApproval && prior.Candidate.ID == candidate.ID {
		if err := verifyWindowsPreparedCandidate(ctx, prior); err != nil {
			return windowsActivationJournal{}, err
		}
		return prior, nil
	} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return windowsActivationJournal{}, loadErr
	}
	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil {
		return windowsActivationJournal{}, err
	}
	paths, err := canonicalWindowsRelease(layout, release.Version)
	if err != nil {
		return windowsActivationJournal{}, err
	}
	if _, err := os.Lstat(filepath.Join(paths.Root, ".quarantined")); err == nil {
		return windowsActivationJournal{}, workerupdate.ErrQuarantined
	} else if !errors.Is(err, os.ErrNotExist) {
		return windowsActivationJournal{}, err
	}
	// Resolve and validate every mutable SCM dependency before downloading any
	// release bytes. An inconsistent installation fails cheaply and unchanged.
	hostdName, updaterName, err := windowsInstanceServiceNames(config.OwnerSID)
	if err != nil {
		return windowsActivationJournal{}, err
	}
	oldHostd, err := queryWindowsServiceTarget(hostdName, "__runtime-hostd")
	if err != nil {
		return windowsActivationJournal{}, err
	}
	oldUpdater, err := queryWindowsServiceTarget(updaterName, "__runtime-updated")
	if err != nil {
		return windowsActivationJournal{}, err
	}
	instance, _, _, sshName, _, err := windowsInstanceNames(config.OwnerSID)
	if err != nil {
		return windowsActivationJournal{}, err
	}
	for _, role := range []struct {
		kind   string
		target *windowsServiceTarget
	}{{service.HostdKind, &oldHostd}, {service.UpdaterKind, &oldUpdater}} {
		identity, identityErr := service.VerifyOwnedWindowsRoleExecutable(role.kind, instance, role.target.Executable)
		if identityErr != nil {
			return windowsActivationJournal{}, fmt.Errorf("native runtime declaration requires initial maintenance installation: %w", identityErr)
		}
		role.target.SHA256, role.target.Length = identity.SHA256, identity.Length
	}

	oldSSH := windowsServiceTarget{}
	{
		oldSSH, err = queryOptionalWindowsServiceTarget(sshName)
		if err != nil {
			return windowsActivationJournal{}, err
		}
		if !validWindowsSSHTarget(oldSSH) {
			return windowsActivationJournal{}, errInvalidWindowsActivation
		}
		if !validWindowsSSHArguments(oldSSH.Arguments) {
			return windowsActivationJournal{}, errInvalidWindowsActivation
		}
	}
	if !activeWindowsServiceTargetsMatch(layout, config.ActiveVersion, oldHostd, oldUpdater, oldSSH, config.Source) {
		return windowsActivationJournal{}, errInvalidWindowsActivation
	}
	if !release.SupervisorMaintenance && (strings.EqualFold(oldHostd.Executable, layout.Binary) || strings.EqualFold(oldUpdater.Executable, layout.Binary) || strings.EqualFold(oldSSH.Executable, layout.Binary)) {
		return windowsActivationJournal{}, errors.New("Windows terminal-preserving updates require the installed native owners to be pinned during maintenance")
	}
	localDaemonLock, err := windowsLocalDaemonLockPath(config.RuntimeStateRoot)
	if err != nil {
		return windowsActivationJournal{}, err
	}
	localDaemonWasRunning, err := localdaemon.WindowsOwnerServiceRunning(localDaemonLock, config.OwnerSID)
	if err != nil {
		return windowsActivationJournal{}, err
	}
	localDaemonServiceRunning, err := localdaemon.WindowsLocalDaemonServiceRunning(config.OwnerSID)
	if err != nil {
		return windowsActivationJournal{}, err
	}
	localDaemonWasRunning = localDaemonWasRunning || localDaemonServiceRunning
	previousBinary, err := describeWindowsActivationComponent(layout.Binary, config.Architecture)
	if err != nil {
		return windowsActivationJournal{}, err
	}
	verifyCtx, cancelVerify := context.WithTimeout(ctx, 30*time.Second)
	if err := nativesignature.New(nil).Verify(verifyCtx, layout.Binary, "windows", config.Architecture); err != nil {
		cancelVerify()
		return windowsActivationJournal{}, err
	}
	cancelVerify()
	if err := secureWindowsTransactionDirectory(filepath.Dir(windowsActivationJournalPath(config.StateRoot))); err != nil {
		return windowsActivationJournal{}, err
	}
	if err := secureWindowsReleaseDirectory(filepath.Dir(paths.Root)); err != nil {
		return windowsActivationJournal{}, err
	}
	if err := secureWindowsReleaseDirectory(paths.Root); err != nil {
		return windowsActivationJournal{}, err
	}
	source, err := newWindowsTUFSource(config)
	if err != nil {
		return windowsActivationJournal{}, err
	}
	// Every role executes the same authenticated pb target. Stage it once;
	// downloading aliases independently can leave different snapshots of the
	// same signed release in one transaction if metadata changes mid-stage.
	target := workerupdate.ComponentTarget{SHA256: release.SHA256, Length: release.Length, Platform: release.Platform, Architecture: release.Architecture}
	if target.Platform != "windows" || target.Architecture != config.Architecture || target.Length <= 0 || target.Length > maxWindowsComponentSize || len(target.SHA256) != 64 || !lowerHex(target.SHA256) {
		return windowsActivationJournal{}, workerupdate.ErrInvalidRelease
	}
	if err := stageWindowsComponent(ctx, source, release, "pb", paths.Runtime, target, config.OwnerSID); err != nil {
		return windowsActivationJournal{}, err
	}
	staged := windowsActivationComponent{Path: paths.Runtime, SHA256: target.SHA256, Length: target.Length}
	newSSH := windowsServiceTarget{}
	if oldSSH.Executable != "" {
		newSSH = windowsServiceTarget{Executable: paths.Runtime, Arguments: append([]string(nil), oldSSH.Arguments...), WasRunning: oldSSH.WasRunning}
	}
	var transaction [16]byte
	if _, err := rand.Read(transaction[:]); err != nil {
		return windowsActivationJournal{}, err
	}
	journal := windowsActivationJournal{
		Release: release, Schema: windowsActivationJournalSchema, TransactionID: hex.EncodeToString(transaction[:]), PreviousVersion: config.ActiveVersion, Version: release.Version, Architecture: config.Architecture, Stage: windowsActivationAwaitingApproval,
		Runtime: staged, CLI: staged, Hostd: staged, Updater: staged, PreviousBinary: previousBinary,
		OldHostd: oldHostd, OldUpdater: oldUpdater, OldSSH: oldSSH, NewSSH: newSSH,
		LocalDaemonWasRunning: localDaemonWasRunning,
		NewHostd:              windowsServiceTarget{Executable: paths.Runtime, SHA256: staged.SHA256, Length: staged.Length, Arguments: []string{"daemon", "__runtime-hostd", "--instance", instance}, WasRunning: oldHostd.WasRunning},
		NewUpdater:            windowsServiceTarget{Executable: paths.Runtime, SHA256: staged.SHA256, Length: staged.Length, Arguments: []string{"daemon", "__runtime-updated", "--instance", instance}, WasRunning: oldUpdater.WasRunning},
	}
	if !release.SupervisorMaintenance {
		previousPaths, pathErr := canonicalWindowsRelease(layout, config.ActiveVersion)
		if pathErr != nil {
			return windowsActivationJournal{}, pathErr
		}
		journal.PreviousRuntime = previousBinary
		journal.PreviousRuntime.Path = previousPaths.Runtime
		if !matchesWindowsComponent(journal.PreviousRuntime.Path, windowsActivationComponentTarget(previousBinary, config.Architecture)) {
			return windowsActivationJournal{}, errInvalidWindowsActivation
		}
		journal.NewHostd = oldHostd
		journal.NewUpdater = oldUpdater
		journal.NewSSH = oldSSH
	}
	if config.Source.Validate() == nil && config.Source.Version == config.ActiveVersion {
		local := config.Source
		journal.PreviousSource = &local
	}
	journal.Candidate = candidate
	backend := newWindowsSCMActivationBackend(config)
	if err := backend.AuthorizeRecovery(ctx, journal); err != nil {
		return windowsActivationJournal{}, err
	}
	if err := backend.WriteJournal(journal); err != nil {
		return windowsActivationJournal{}, err
	}
	return journal, nil
}

func activeWindowsServiceTargetsMatch(layout service.Layout, version string, hostd, updater, ssh windowsServiceTarget, sources ...installsource.Source) bool {
	local := len(sources) == 1 && sources[0].Validate() == nil && sources[0].Version == version
	if !(exactReleasePattern.MatchString(version) || local) || !windowsOwnedServiceExecutable(layout, hostd.Executable) || !windowsOwnedServiceExecutable(layout, updater.Executable) {
		return false
	}
	return ssh.Executable == "" || windowsOwnedServiceExecutable(layout, ssh.Executable)
}

func windowsUpdaterExecutableMatches(layout service.Layout, executable string) bool {
	return windowsOwnedServiceExecutable(layout, executable)
}

func describeWindowsActivationComponent(path, architecture string) (windowsActivationComponent, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxWindowsComponentSize {
		if err == nil {
			err = errInvalidWindowsActivation
		}
		return windowsActivationComponent{}, err
	}
	if err := binarytarget.Validate(path, "windows", architecture); err != nil {
		return windowsActivationComponent{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return windowsActivationComponent{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxWindowsComponentSize+1)); err != nil {
		return windowsActivationComponent{}, err
	}
	return windowsActivationComponent{Path: filepath.Clean(path), SHA256: hex.EncodeToString(hash.Sum(nil)), Length: info.Size()}, nil
}

func windowsActivationComponentTarget(component windowsActivationComponent, architecture string) workerupdate.ComponentTarget {
	return workerupdate.ComponentTarget{SHA256: component.SHA256, Length: component.Length, Platform: "windows", Architecture: architecture}
}

func stageWindowsComponent(ctx context.Context, source workerupdate.TUFSource, release workerupdate.Release, name, destination string, target workerupdate.ComponentTarget, ownerSID string) error {
	if matchesWindowsComponent(destination, target) {
		if err := secureWindowsReleaseFile(destination); err != nil {
			return err
		}
		verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return nativesignature.New(nil).Verify(verifyCtx, destination, "windows", target.Architecture)
	}
	if _, err := os.Lstat(destination); err == nil {
		return errInvalidWindowsActivation
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stream, err := source.FetchComponent(ctx, release, name)
	if err != nil {
		return err
	}
	defer stream.Close()
	temporary := destination + ".staged"
	_ = os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(stream, target.Length+1))
	syncErr, closeErr := file.Sync(), file.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if written != target.Length || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), target.SHA256) {
		_ = os.Remove(temporary)
		return workerupdate.ErrInvalidRelease
	}
	if err := binarytarget.Validate(temporary, "windows", target.Architecture); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := nativesignature.New(nil).Verify(verifyCtx, temporary, "windows", target.Architecture); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := secureWindowsReleaseFile(temporary); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	// Destination cannot exist: immutable version paths are never overwritten.
	//paperboat:allow-source-policy atomic-replacement owner=windows-updater reason=verified-immutable-component-publication
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func secureWindowsReleaseDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errInvalidWindowsActivation
	}
	return applyWindowsReleaseACL(path, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)")
}

func secureWindowsTransactionDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errInvalidWindowsActivation
	}
	return applyWindowsReleaseACL(path, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
}

func secureWindowsReleaseFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errInvalidWindowsActivation
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errInvalidWindowsActivation
	}
	return applyWindowsReleaseACL(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)")
}

func applyWindowsReleaseACL(path, sddl string) error {
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, system, nil, dacl, nil)
	}); err != nil {
		return err
	}
	if !windowssecurity.OwnerMatchesSID(path, system) || !windowssecurity.ProtectedDACLMatches(path, sddl) {
		return errInvalidWindowsActivation
	}
	return nil
}

func matchesWindowsComponent(path string, target workerupdate.ComponentTarget) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != target.Length {
		return false
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), target.SHA256) && binarytarget.Validate(path, "windows", target.Architecture) == nil
}

func readOptionalWindowsCLIRecord(path string) (string, error) {
	if _, err := os.Lstat(path); err == nil {
		if !windowsMachineFileSecurityMatches(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)") {
			return "", errInvalidWindowsActivation
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || len(body) > 256 || strings.Count(string(body), "\n") != 1 {
		return "", errInvalidWindowsActivation
	}
	return string(body), nil
}

func windowsMachineFileSecurityMatches(path, dacl string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	return err == nil && windowssecurity.OwnerMatchesSID(path, system) && windowssecurity.ProtectedDACLMatches(path, dacl)
}

func queryWindowsServiceTarget(name, expectedArgument string) (windowsServiceTarget, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return windowsServiceTarget{}, err
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(name)
	if err != nil {
		return windowsServiceTarget{}, err
	}
	defer item.Close()
	config, err := item.Config()
	if err != nil {
		return windowsServiceTarget{}, err
	}
	target, err := parseWindowsRuntimeServiceTarget(name, expectedArgument, config.BinaryPathName)
	if err != nil || !validPrivilegedWindowsServiceConfig(config, mgr.StartAutomatic, mgr.ErrorNormal) {
		return windowsServiceTarget{}, errInvalidWindowsActivation
	}
	if err := validateWindowsRecovery(item); err != nil {
		return windowsServiceTarget{}, err
	}
	status, err := item.Query()
	if err != nil {
		return windowsServiceTarget{}, err
	}
	target.WasRunning = status.State != svc.Stopped
	return target, nil
}

func parseWindowsRuntimeServiceTarget(name, role, command string) (windowsServiceTarget, error) {
	base := ""
	switch role {
	case "__runtime-hostd":
		base = windowsHostdService
	case "__runtime-updated":
		base = windowsUpdaterService
	default:
		return windowsServiceTarget{}, errInvalidWindowsActivation
	}
	args, err := windows.DecomposeCommandLine(command)
	if err != nil || len(args) != 5 || !filepath.IsAbs(args[0]) || args[1] != "daemon" || args[2] != role || args[3] != "--instance" || len(args[4]) != 25 || args[4][0] != 'u' || !lowerHex(args[4][1:]) || name != base+"-"+args[4] {
		return windowsServiceTarget{}, errInvalidWindowsActivation
	}
	return windowsServiceTarget{Executable: filepath.Clean(args[0]), Arguments: append([]string(nil), args[1:]...)}, nil
}

func queryOptionalWindowsServiceTarget(name string) (windowsServiceTarget, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return windowsServiceTarget{}, err
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return windowsServiceTarget{}, nil
	}
	if err != nil {
		return windowsServiceTarget{}, err
	}
	defer item.Close()
	config, err := item.Config()
	if err != nil {
		return windowsServiceTarget{}, err
	}
	args, err := windows.DecomposeCommandLine(config.BinaryPathName)
	if err != nil || len(args) < 2 || !filepath.IsAbs(args[0]) || !validPrivilegedWindowsServiceConfig(config, mgr.StartAutomatic, mgr.ErrorNormal) {
		return windowsServiceTarget{}, errInvalidWindowsActivation
	}
	status, err := item.Query()
	if err != nil {
		return windowsServiceTarget{}, err
	}
	return windowsServiceTarget{Executable: filepath.Clean(args[0]), Arguments: append([]string(nil), args[1:]...), WasRunning: status.State != svc.Stopped}, nil
}

func validPrivilegedWindowsServiceConfig(config mgr.Config, startType, errorControl uint32) bool {
	return strings.EqualFold(config.ServiceStartName, "LocalSystem") && config.StartType == startType && config.ErrorControl == errorControl && config.SidType == windows.SERVICE_SID_TYPE_UNRESTRICTED && !config.DelayedAutoStart
}

func installWindowsActivatorService(executable, ownerSID string) error {
	instance, _, _, _, name, err := windowsInstanceNames(ownerSID)
	if err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	if current, openErr := manager.OpenService(name); openErr == nil {
		defer current.Close()
		config, e := current.Config()
		if e != nil {
			return e
		}
		config.BinaryPathName = windows.ComposeCommandLine([]string{executable, "daemon", "__runtime-activate", "--instance", instance})
		config.StartType = mgr.StartAutomatic
		config.ErrorControl = mgr.ErrorSevere
		config.ServiceStartName = "LocalSystem"
		config.SidType = windows.SERVICE_SID_TYPE_UNRESTRICTED
		config.DelayedAutoStart = false
		if err := current.UpdateConfig(config); err != nil {
			return err
		}
		if err := configureWindowsRecovery(current); err != nil {
			return err
		}
		updated, err := current.Config()
		if err != nil || !strings.EqualFold(updated.BinaryPathName, config.BinaryPathName) || !validPrivilegedWindowsServiceConfig(updated, mgr.StartAutomatic, mgr.ErrorSevere) {
			return errors.Join(errInvalidWindowsActivation, err)
		}
		return validateWindowsRecovery(current)
	} else if !errors.Is(openErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return openErr
	}
	item, err := manager.CreateService(name, executable, mgr.Config{DisplayName: "Paperboat Update Activator", Description: "Paperboat one-shot verified update activation", StartType: mgr.StartAutomatic, ErrorControl: mgr.ErrorSevere, ServiceStartName: "LocalSystem", SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED}, "daemon", "__runtime-activate", "--instance", instance)
	if err != nil {
		return err
	}
	defer item.Close()
	if err := configureWindowsRecovery(item); err != nil {
		return err
	}
	return validateWindowsRecovery(item)
}

func configureWindowsRecovery(item *mgr.Service) error {
	if err := item.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 15 * time.Second}, {Type: mgr.ServiceRestart, Delay: time.Minute}}, 24*60*60); err != nil {
		return err
	}
	return item.SetRecoveryActionsOnNonCrashFailures(true)
}

func validateWindowsRecovery(item *mgr.Service) error {
	return validateWindowsRecoveryActions(item, standardWindowsRecoveryActions())
}

func standardWindowsRecoveryActions() []mgr.RecoveryAction {
	return []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 15 * time.Second}, {Type: mgr.ServiceRestart, Delay: time.Minute}}
}

func windowsRecoveryActionsForService(name string) []mgr.RecoveryAction {
	if isWindowsSSHInstanceService(name) {
		return windowsopenssh.ServiceRecoveryActions()
	}
	return standardWindowsRecoveryActions()
}

func validateWindowsRecoveryActions(item *mgr.Service, expected []mgr.RecoveryAction) error {
	actions, err := item.RecoveryActions()
	if err != nil || !windowsRecoveryActionsMatch(actions, expected) {
		return errors.Join(errInvalidWindowsActivation, err)
	}
	nonCrash, err := item.RecoveryActionsOnNonCrashFailures()
	if err != nil || !nonCrash {
		return errors.Join(errInvalidWindowsActivation, err)
	}
	return nil
}

func windowsRecoveryActionsMatch(actual, expected []mgr.RecoveryAction) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func startWindowsActivatorService(ownerSID string) error {
	_, _, _, _, name, err := windowsInstanceNames(ownerSID)
	if err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(name)
	if err != nil {
		return err
	}
	defer item.Close()
	return item.Start()
}

type windowsSCMActivationBackend struct {
	config WindowsConfig
}

func newWindowsSCMActivationBackend(config WindowsConfig) *windowsSCMActivationBackend {
	return &windowsSCMActivationBackend{config: config}
}

func (b *windowsSCMActivationBackend) WriteJournal(j windowsActivationJournal) error {
	body, err := json.Marshal(j)
	if err != nil {
		return err
	}
	path := windowsActivationJournalPath(b.config.StateRoot)
	if err := atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}); err != nil {
		return err
	}
	return applyWindowsReleaseACL(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)")
}

func windowsLocalDaemonLockPath(runtimeStateRoot string) (string, error) {
	if !filepath.IsAbs(runtimeStateRoot) || filepath.Clean(runtimeStateRoot) != runtimeStateRoot {
		return "", errInvalidWindowsActivation
	}
	return filepath.Join(filepath.Dir(runtimeStateRoot), "state", "daemon.lock"), nil
}

func (b *windowsSCMActivationBackend) StopServices(ctx context.Context, localDaemonWasRunning bool) error {
	instance, err := service.WindowsUserInstance(b.config.OwnerSID)
	if err != nil {
		return err
	}
	serviceErr := stopNamedWindowsServices(ctx, windowsActivationServiceNames(instance)...)
	lockPath, err := windowsLocalDaemonLockPath(b.config.RuntimeStateRoot)
	if err != nil {
		return errors.Join(serviceErr, err)
	}
	stopErr := localdaemon.StopWindowsOwnerService(ctx, lockPath, b.config.OwnerSID)
	stateErr := hostinstall.PrepareWindowsLocalDaemonState(b.config.RuntimeStateRoot, b.config.OwnerSID)
	// A stale session-scoped lock DACL can prevent the first owner-process
	// cleanup after SCM has stopped the service. Once the privileged state
	// repair succeeds, retry that exact idempotent stop before slot rotation.
	if stopErr != nil && stateErr == nil {
		stopErr = localdaemon.StopWindowsOwnerService(ctx, lockPath, b.config.OwnerSID)
	}
	if !localDaemonWasRunning && errors.Is(stopErr, os.ErrNotExist) {
		stopErr = nil
	}
	return errors.Join(serviceErr, stopErr, stateErr)
}

func windowsStableBinaryDACL(ownerSID string) string {
	return "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;" + ownerSID + ")"
}

func windowsStableBinarySecurityDescriptor(ownerSID string) string {
	return "O:SY" + windowsStableBinaryDACL(ownerSID)
}

// AuthorizeRecovery checks the fresh signed version policy and the exact
// protected previous bytes before any destructive service or slot change.
func (b *windowsSCMActivationBackend) AuthorizeRecovery(ctx context.Context, journal windowsActivationJournal) error {
	source, err := newWindowsTUFSource(b.config)
	if err != nil {
		return err
	}
	if journal.PreviousSource == nil {
		if err := source.AuthorizeRecovery(ctx, journal.PreviousVersion, "windows", journal.Architecture); err != nil {
			return err
		}
	} else if !validWindowsLocalPrevious(journal) {
		return errInvalidWindowsActivation
	}
	target := windowsActivationComponentTarget(journal.PreviousBinary, journal.Architecture)
	for _, path := range []string{b.config.Binary, b.config.BinaryRollback} {
		if matchesWindowsComponent(path, target) {
			return verifyWindowsStableBinary(ctx, path, target, b.config.OwnerSID)
		}
	}
	return errInvalidWindowsActivation
}

func (b *windowsSCMActivationBackend) ActivateBinary(ctx context.Context, journal windowsActivationJournal) error {
	layout, err := service.WindowsUserLayout(b.config.OwnerSID)
	if err != nil || b.config.Binary != layout.Binary || b.config.BinaryRollback != layout.BinaryRollback || b.config.BinaryStaged != layout.BinaryStaged {
		return fmt.Errorf("canonical Windows slot identity: %w", errInvalidWindowsActivation)
	}
	candidateTarget := windowsActivationComponentTarget(journal.Runtime, journal.Architecture)
	if err := verifyWindowsActivationComponent(ctx, journal.Runtime.Path, candidateTarget); err != nil {
		return err
	}
	previousTarget := windowsActivationComponentTarget(journal.PreviousBinary, journal.Architecture)
	if !matchesWindowsComponent(b.config.Binary, previousTarget) {
		return fmt.Errorf("previous Windows binary identity: %w", errInvalidWindowsActivation)
	}
	body, err := readWindowsActivationBinary(journal.Runtime.Path)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(b.config.BinaryStaged, body, atomicfile.Options{Mode: 0o755, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: windowsStableBinarySecurityDescriptor(b.config.OwnerSID)}); err != nil {
		return err
	}
	if err := verifyWindowsStableBinary(ctx, b.config.BinaryStaged, candidateTarget, b.config.OwnerSID); err != nil {
		_ = removeWindowsActivationFile(b.config.BinaryStaged)
		return fmt.Errorf("verify staged Windows binary: %w", err)
	}
	if err := removeWindowsActivationFile(b.config.BinaryRollback); err != nil {
		_ = removeWindowsActivationFile(b.config.BinaryStaged)
		return err
	}
	if err := moveWindowsActivationFile(ctx, b.config.Binary, b.config.BinaryRollback); err != nil {
		_ = removeWindowsActivationFile(b.config.BinaryStaged)
		return err
	}
	if err := moveWindowsActivationFile(ctx, b.config.BinaryStaged, b.config.Binary); err != nil {
		_ = moveWindowsActivationFile(ctx, b.config.BinaryRollback, b.config.Binary)
		return err
	}
	if err := verifyWindowsStableBinary(ctx, b.config.Binary, candidateTarget, b.config.OwnerSID); err != nil {
		_ = removeWindowsActivationFile(b.config.Binary)
		_ = moveWindowsActivationFile(ctx, b.config.BinaryRollback, b.config.Binary)
		return err
	}
	return nil
}

func (b *windowsSCMActivationBackend) RestoreBinary(ctx context.Context, journal windowsActivationJournal) error {
	layout, err := service.WindowsUserLayout(b.config.OwnerSID)
	if err != nil || b.config.Binary != layout.Binary || b.config.BinaryRollback != layout.BinaryRollback || b.config.BinaryStaged != layout.BinaryStaged {
		return errInvalidWindowsActivation
	}
	candidateTarget := windowsActivationComponentTarget(journal.Runtime, journal.Architecture)
	previousTarget := windowsActivationComponentTarget(journal.PreviousBinary, journal.Architecture)
	if _, err := os.Lstat(b.config.BinaryStaged); err == nil {
		if !matchesWindowsComponent(b.config.BinaryStaged, candidateTarget) {
			return errInvalidWindowsActivation
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	currentExists := false
	if _, err := os.Lstat(b.config.Binary); err == nil {
		currentExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rollbackExists := false
	if _, err := os.Lstat(b.config.BinaryRollback); err == nil {
		rollbackExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if currentExists && matchesWindowsComponent(b.config.Binary, previousTarget) {
		return removeWindowsActivationFile(b.config.BinaryStaged)
	}
	if !rollbackExists || !matchesWindowsComponent(b.config.BinaryRollback, previousTarget) {
		return errInvalidWindowsActivation
	}
	if currentExists {
		if !matchesWindowsComponent(b.config.Binary, candidateTarget) {
			return errInvalidWindowsActivation
		}
		if err := removeWindowsActivationFile(b.config.Binary); err != nil {
			return err
		}
	}
	if err := removeWindowsActivationFile(b.config.BinaryStaged); err != nil {
		return err
	}
	if err := moveWindowsActivationFile(ctx, b.config.BinaryRollback, b.config.Binary); err != nil {
		return err
	}
	return verifyWindowsStableBinary(ctx, b.config.Binary, previousTarget, b.config.OwnerSID)
}

func readWindowsActivationBinary(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxWindowsComponentSize+1))
	if err != nil || len(body) == 0 || int64(len(body)) > maxWindowsComponentSize {
		return nil, errInvalidWindowsActivation
	}
	return body, nil
}

func verifyWindowsActivationComponent(ctx context.Context, path string, target workerupdate.ComponentTarget) error {
	if !matchesWindowsComponent(path, target) {
		return errInvalidWindowsActivation
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return nativesignature.New(nil).Verify(verifyCtx, path, "windows", target.Architecture)
}

func verifyWindowsStableBinary(ctx context.Context, path string, target workerupdate.ComponentTarget, ownerSID string) error {
	if err := verifyWindowsActivationComponent(ctx, path, target); err != nil {
		return err
	}
	if !windowsMachineFileSecurityMatches(path, windowsStableBinaryDACL(ownerSID)) {
		return fmt.Errorf("protected Windows binary ACL: %w", errInvalidWindowsActivation)
	}
	return nil
}

func moveWindowsActivationFile(ctx context.Context, from, to string) error {
	fromPointer, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPointer, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return retryWindowsFileOperation(ctx, func() error {
		return windows.MoveFileEx(fromPointer, toPointer, windows.MOVEFILE_WRITE_THROUGH)
	})
}

func removeWindowsActivationFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errInvalidWindowsActivation
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errInvalidWindowsActivation
	}
	return os.Remove(path)
}

func retryWindowsFileOperation(ctx context.Context, operation func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var lastErr error
	for {
		if err := operation(); err == nil {
			return nil
		} else {
			lastErr = err
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(lastErr, ctx.Err())
		case <-deadline.C:
			return lastErr
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (b *windowsSCMActivationBackend) SetServiceTargets(ctx context.Context, hostd, updater, ssh windowsServiceTarget) error {
	layout, err := service.WindowsUserLayout(b.config.OwnerSID)
	if err != nil {
		return err
	}
	hostd, updater, ssh, err = normalizeWindowsRollbackTargets(layout, hostd, updater, ssh)
	if err != nil {
		return err
	}

	hostdName, updaterName, err := windowsInstanceServiceNames(b.config.OwnerSID)
	if err != nil {
		return err
	}
	if err := setWindowsServiceTarget(hostdName, hostd); err != nil {
		return err
	}
	if err := setWindowsServiceTarget(updaterName, updater); err != nil {
		return err
	}
	for _, role := range []struct {
		kind   string
		target windowsServiceTarget
	}{{service.HostdKind, hostd}, {service.UpdaterKind, updater}, {service.DaemonKind, hostd}} {
		if err := hostinstall.PublishWindowsRoleDefinition(ctx, b.config.OwnerSID, role.kind, role.target.Executable, role.target.SHA256, role.target.Length); err != nil {
			return err
		}
	}
	instance, _, _, _, _, err := windowsInstanceNames(b.config.OwnerSID)
	if err != nil {
		return err
	}
	if err = setWindowsServiceTarget("PaperboatLocalDaemon-"+instance, windowsServiceTarget{Executable: hostd.Executable, Arguments: []string{"daemon", "__runtime-local-daemon", "--instance", instance}}); err != nil {
		return err
	}
	if ssh.Executable != "" {
		_, _, _, sshName, _, err := windowsInstanceNames(b.config.OwnerSID)
		if err != nil {
			return err
		}
		return setWindowsServiceTarget(sshName, ssh)
	}
	return nil
}

// normalizeWindowsRollbackTargets returns service targets that are valid after
// the rollback slot has been moved back into the canonical binary location.
// A previous interrupted activation may have recorded PaperboatUpdated on the
// rollback path. Once RestoreBinary succeeds that path no longer exists, so
// restarting it verbatim leaves every service stopped and strands recovery.
func normalizeWindowsRollbackTargets(layout service.Layout, hostd, updater, ssh windowsServiceTarget) (windowsServiceTarget, windowsServiceTarget, windowsServiceTarget, error) {
	if !windowsOwnedServiceExecutable(layout, hostd.Executable) || !windowsOwnedServiceExecutable(layout, updater.Executable) || (ssh.Executable != "" && !windowsOwnedServiceExecutable(layout, ssh.Executable)) {
		return windowsServiceTarget{}, windowsServiceTarget{}, windowsServiceTarget{}, errInvalidWindowsActivation
	}
	if strings.EqualFold(updater.Executable, layout.BinaryRollback) {
		updater.Executable = layout.Binary
	}
	return hostd, updater, ssh, nil
}

func (b *windowsSCMActivationBackend) StartServices(ctx context.Context, hostd, updater, ssh, localDaemon bool) error {
	if localDaemon {
		if err := hostinstall.PrepareWindowsLocalDaemonState(b.config.RuntimeStateRoot, b.config.OwnerSID); err != nil {
			return err
		}
	}
	// Hostd validates its managed SSH loopback target during startup. Start SSH
	// first so both activation and rollback can bring a host runtime up from a
	// fully stopped service set without a dependency deadlock.
	instance, err := service.WindowsUserInstance(b.config.OwnerSID)
	if err != nil {
		return err
	}
	for _, name := range windowsActivationServiceStartNames(instance, hostd, updater, ssh) {
		if err := startNamedWindowsService(ctx, name); err != nil {
			return err
		}
	}
	if localDaemon {
		return localdaemon.StartWindowsOwnerService(ctx, b.config.OwnerSID)
	}
	return nil
}

func (b *windowsSCMActivationBackend) VerifyCommitted(ctx context.Context, journal windowsActivationJournal) error {
	source, err := newWindowsTUFSource(b.config)
	if err != nil {
		return err
	}
	if err = source.AuthorizeRecovery(ctx, journal.Version, "windows", journal.Architecture); err != nil {
		return err
	}
	if err = verifyWindowsStableBinary(ctx, b.config.Binary, windowsActivationComponentTarget(journal.Runtime, journal.Architecture), b.config.OwnerSID); err != nil {
		return err
	}

	if journal.Release.SupervisorMaintenance {
		if err := b.verifyWindowsRuntimeVersions(ctx, journal.Version); err != nil {
			return err
		}
	}
	status, err := b.activeHostdStatus(ctx)
	if err == nil && !validWindowsRuntimeStatus(status, journal.Version) {
		return errInvalidWindowsActivation
	}
	return err
}

func (b *windowsSCMActivationBackend) FinalizeServices(ctx context.Context, journal windowsActivationJournal) error {
	if journal.Stage != windowsActivationCommitted {
		return errInvalidWindowsActivation
	}
	if !journal.Release.SupervisorMaintenance {
		return nil
	}
	if err := hostinstall.EnsureWindowsLocalDaemonService(ctx, b.config.OwnerSID); err != nil {
		return err
	}
	return b.clearCommittedOwnerMaintenance(journal)
}

func (b *windowsSCMActivationBackend) VerifyRollback(ctx context.Context, journal windowsActivationJournal) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if journal.Release.SupervisorMaintenance {
		if err := b.verifyWindowsRuntimeVersions(ctx, journal.PreviousVersion, windowsPinnedTargetVersion(journal.OldHostd, journal.PreviousVersion)); err != nil {
			return err
		}
	}
	status, err := b.activeHostdStatus(ctx)
	if err == nil && !validWindowsRuntimeStatus(status, journal.PreviousVersion) {
		return errInvalidWindowsActivation
	}
	return err
}

func (b *windowsSCMActivationBackend) activeHostdStatus(ctx context.Context) (hostdproto.Status, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	token, err := os.ReadFile(b.config.TokenFile)
	if err != nil {
		return hostdproto.Status{}, err
	}
	defer clear(token)
	hostd, err := hostdproto.NewClient(b.config.HostdSocket, token, 5*time.Second)
	if err != nil {
		return hostdproto.Status{}, err
	}
	return hostd.Active(ctx)
}

func (b *windowsSCMActivationBackend) VerifyHealth(ctx context.Context, journal windowsActivationJournal) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if b == nil {
		return errInvalidWindowsActivation
	}
	hostdName, updaterName, err := windowsInstanceServiceNames(b.config.OwnerSID)
	if err != nil {
		return err
	}
	if err := requireNamedWindowsServicesRunning(hostdName, updaterName); err != nil {
		return fmt.Errorf("verify Windows runtime services: %w", err)
	}
	token, err := os.ReadFile(b.config.TokenFile)
	if err != nil {
		return err
	}
	defer clear(token)
	hostd, err := hostdproto.NewClient(b.config.HostdSocket, token, 5*time.Second)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(90 * time.Second)
	var status hostdproto.Status
	var activeErr error
	for {
		status, activeErr = hostd.Active(ctx)
		if activeErr == nil && validWindowsRuntimeStatus(status, journal.Version) && status.APIVersion >= journal.Release.HostdAPIMin && status.APIVersion <= journal.Release.HostdAPIMax {
			break
		}
		if time.Now().After(deadline) {
			return errors.Join(errors.New("verify PaperboatHostd active heartbeat"), errInvalidWindowsActivation, activeErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if b.config.HealthURL == "" {
		return errors.Join(errors.New("verify runtime health URL"), errInvalidWindowsActivation)
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.config.HealthURL, nil)
	response, err := (&http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var health struct {
		Live bool `json:"live"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 8<<10)).Decode(&health) != nil || !health.Live {
		return fmt.Errorf("runtime health returned HTTP %d", response.StatusCode)
	}
	if err := b.verifyWindowsRuntimeVersions(ctx, journal.Version); err != nil {
		return err
	}
	if !matchesWindowsComponent(journal.CLI.Path, workerupdate.ComponentTarget{SHA256: journal.CLI.SHA256, Length: journal.CLI.Length, Platform: "windows", Architecture: journal.Architecture}) {
		return errors.Join(errors.New("verify staged Windows CLI component"), errInvalidWindowsActivation)
	}
	if journal.NewSSH.WasRunning {
		_, _, _, sshName, _, err := windowsInstanceNames(b.config.OwnerSID)
		if err != nil {
			return err
		}
		if err := requireNamedWindowsServicesRunning(sshName); err != nil {
			return err
		}
	}
	// Observe the approved replacement without a worker admission transaction.
	bounded, cancel := context.WithTimeout(ctx, windowsStabilityCallTimeout(journal.Release.StabilityWindow, journal.Release.StabilityInterval))
	defer cancel()
	timer := time.NewTimer(journal.Release.StabilityWindow)
	defer timer.Stop()
	ticker := time.NewTicker(journal.Release.StabilityInterval)
	defer ticker.Stop()
	probe := func() error {
		current, err := hostd.Active(bounded)
		if err != nil {
			return err
		}
		if current.State != hostdproto.StateActive || current.WorkerID != status.WorkerID || current.Epoch != status.Epoch || current.LastHeartbeatUnixMilli <= 0 || time.Since(time.UnixMilli(current.LastHeartbeatUnixMilli)) > 15*time.Second {
			return errInvalidWindowsActivation
		}
		return b.verifyWindowsRuntimeVersions(bounded, journal.Version)
	}
	for {
		select {
		case <-bounded.Done():
			return bounded.Err()
		case <-ticker.C:
			if err := probe(); err != nil {
				return err
			}
		case <-timer.C:
			return probe()
		}
	}

}

func (b *windowsSCMActivationBackend) verifyWindowsRuntimeVersions(ctx context.Context, version string, daemonVersions ...string) error {
	daemonVersion := version
	if len(daemonVersions) != 0 {
		daemonVersion = daemonVersions[0]
	}
	hostdName, updaterName, err := windowsInstanceServiceNames(b.config.OwnerSID)
	if err != nil {
		return err
	}
	if err := requireNamedWindowsServicesRunning(hostdName, updaterName); err != nil {
		return fmt.Errorf("verify Windows runtime services: %w", err)
	}
	updater, err := NewClient(b.config.ControlSocket, 2*time.Second)
	if err != nil {
		return fmt.Errorf("verify PaperboatUpdated control: %w", err)
	}
	if err := waitForWindowsUpdaterVersion(ctx, version, 30*time.Second, 250*time.Millisecond, updater.Status); err != nil {
		return err
	}
	lockPath, err := windowsLocalDaemonLockPath(b.config.RuntimeStateRoot)
	if err != nil {
		return err
	}
	paths, err := localapi.WindowsPaths(b.config.RuntimeStateRoot, b.config.OwnerSID)
	if err != nil {
		return fmt.Errorf("resolve Paperboat local daemon endpoint: %w", err)
	}
	return waitForWindowsDaemonVersion(ctx, daemonVersion, 30*time.Second, 250*time.Millisecond, func(probeCtx context.Context) (localapi.Snapshot, error) {
		running, probeErr := localdaemon.WindowsOwnerServiceRunning(lockPath, b.config.OwnerSID)
		if probeErr != nil || !running {
			if probeErr == nil {
				probeErr = errors.New("enrolled-owner daemon process is not running")
			}
			return localapi.Snapshot{}, probeErr
		}
		return localapi.ReadSystemOwnerSnapshot(probeCtx, paths.SocketPath, b.config.OwnerSID, 2*time.Second)
	})
}

func windowsStabilityCallTimeout(window, interval time.Duration) time.Duration {
	const maximumCompletionMargin = 30 * time.Second
	margin := interval
	if margin < time.Second {
		margin = time.Second
	}
	if margin > maximumCompletionMargin {
		margin = maximumCompletionMargin
	}
	return window + margin
}

func waitForWindowsUpdaterVersion(ctx context.Context, want string, timeout, retryDelay time.Duration, status func(context.Context) (ControlResponse, error)) error {
	if ctx == nil || !validObservedRuntimeVersion(want) || timeout <= 0 || retryDelay <= 0 || status == nil {
		return errors.Join(errors.New("verify PaperboatUpdated control version"), errInvalidWindowsActivation)
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastVersion string
	var lastErr error
	for {
		response, err := status(bounded)
		if err == nil && response.Version == want {
			return nil
		}
		lastVersion, lastErr = response.Version, err
		timer := time.NewTimer(retryDelay)
		select {
		case <-bounded.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			message := fmt.Errorf("verify PaperboatUpdated control version: got %q, want %q", lastVersion, want)
			return errors.Join(message, errInvalidWindowsActivation, lastErr, bounded.Err())
		case <-timer.C:
		}
	}
}

func waitForWindowsDaemonVersion(ctx context.Context, want string, timeout, retryDelay time.Duration, snapshot func(context.Context) (localapi.Snapshot, error)) error {
	if ctx == nil || !validObservedRuntimeVersion(want) || timeout <= 0 || retryDelay <= 0 || snapshot == nil {
		return errors.Join(errors.New("verify Paperboat local daemon version"), errInvalidWindowsActivation)
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastVersion string
	var lastErr error
	for {
		state, err := snapshot(bounded)
		if err == nil && state.DaemonVersion == want {
			return nil
		}
		lastVersion, lastErr = state.DaemonVersion, err
		timer := time.NewTimer(retryDelay)
		select {
		case <-bounded.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			message := fmt.Errorf("verify Paperboat local daemon version: got %q, want %q", lastVersion, want)
			return errors.Join(message, errInvalidWindowsActivation, lastErr, bounded.Err())
		case <-timer.C:
		}
	}
}

func requireNamedWindowsServicesRunning(names ...string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	for _, name := range names {
		item, err := manager.OpenService(name)
		if err != nil {
			return fmt.Errorf("open Windows service %s: %w", name, err)
		}
		status, queryErr := item.Query()
		closeErr := item.Close()
		if queryErr != nil || closeErr != nil {
			return errors.Join(fmt.Errorf("query Windows service %s", name), errInvalidWindowsActivation, queryErr, closeErr)
		}
		if status.State != svc.Running {
			return errors.Join(fmt.Errorf("Windows service %s state is %d, want %d", name, status.State, svc.Running), errInvalidWindowsActivation)
		}
	}
	return nil
}
func (b *windowsSCMActivationBackend) CommitCLI(ctx context.Context, journal windowsActivationJournal) error {
	if err := commitWindowsInstallIdentity(ctx, b.config, journal); err != nil {
		return err
	}
	recordPath := filepath.Join(filepath.Dir(b.config.Binary), "pb.active")
	newRecord := journal.NewCLIRecord
	cli := journal.CLI
	if windowsJournalRestoresPrevious(journal) {
		newRecord = journal.PreviousCLIRecord
		cli = windowsActivationComponent{}
	}
	if newRecord == "" {
		if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	name := strings.TrimSuffix(newRecord, "\n")
	if strings.ContainsAny(name, `/\:*?"<>|`) || !strings.HasPrefix(name, "pb.slot-") || !strings.HasSuffix(name, ".exe") {
		return errInvalidWindowsActivation
	}
	destination := filepath.Join(filepath.Dir(b.config.Binary), name)
	if cli.Path != "" && !matchesWindowsComponent(destination, workerupdate.ComponentTarget{SHA256: cli.SHA256, Length: cli.Length, Platform: "windows", Architecture: journal.Architecture}) {
		body, err := os.ReadFile(cli.Path)
		if err != nil {
			return err
		}
		if err := windowssecurity.WithRestorePrivilege(func() error {
			return atomicfile.Write(destination, body, atomicfile.Options{Mode: 0o755, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)"})
		}); err != nil {
			return err
		}
	}
	if !windowsMachineFileSecurityMatches(destination, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)") {
		return errInvalidWindowsActivation
	}
	if cli.Path != "" {
		target := workerupdate.ComponentTarget{SHA256: cli.SHA256, Length: cli.Length, Platform: "windows", Architecture: journal.Architecture}
		if !matchesWindowsComponent(destination, target) {
			return errInvalidWindowsActivation
		}
		verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := nativesignature.New(nil).Verify(verifyCtx, destination, "windows", journal.Architecture); err != nil {
			return err
		}
	} else if newRecord != "" {
		if err := binarytarget.Validate(destination, "windows", b.config.Architecture); err != nil {
			return err
		}
		verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := nativesignature.New(nil).Verify(verifyCtx, destination, "windows", b.config.Architecture); err != nil {
			return err
		}
	}
	return windowssecurity.WithRestorePrivilege(func() error {
		return atomicfile.Write(recordPath, []byte(newRecord), atomicfile.Options{Mode: 0o644, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)"})
	})
}

func windowsJournalRestoresPrevious(j windowsActivationJournal) bool {
	return j.Stage == windowsActivationRollingBack || j.Stage == windowsActivationRollbackReady || j.Stage == windowsActivationRolledBack
}

func windowsJournalInstallSource(j windowsActivationJournal, automatic bool) (installsource.Source, error) {
	if !validWindowsActivationJournal(j) {
		return installsource.Source{}, errInvalidWindowsActivation
	}
	version, component := j.Version, j.Runtime
	if windowsJournalRestoresPrevious(j) {
		if j.PreviousSource != nil {
			return *j.PreviousSource, nil
		}
		version, component = j.PreviousVersion, j.PreviousBinary
	} else if j.Stage != windowsActivationServicesLive && j.Stage != windowsActivationCommitReady && j.Stage != windowsActivationCommitted {
		return installsource.Source{}, errInvalidWindowsActivation
	}
	source := installsource.Source{Version: version, Platform: "windows", Architecture: j.Architecture, SHA256: component.SHA256, Length: component.Length, Distribution: installsource.Official, AutomaticUpdates: automatic}
	return source, source.Validate()
}

func commitWindowsInstallIdentity(ctx context.Context, config WindowsConfig, journal windowsActivationJournal) error {
	if !validWindowsActivationPaths(config, journal) {
		return errInvalidWindowsActivation
	}
	if err := validateWindowsReadOnlyOwnerFile(config.InstallState, config.OwnerSID); err != nil {
		return err
	}
	body, err := os.ReadFile(config.InstallState)
	if err != nil || len(body) == 0 || len(body) > 128<<10 {
		return errInvalidWindowsActivation
	}
	var document hostinstall.WindowsRuntimeConfig
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || document.OwnerSID != config.OwnerSID || document.Source.Validate() != nil || document.Source.Platform != "windows" || document.Source.Architecture != journal.Architecture || bootstrap.VerifyArtifactTarget(document.Artifact) != nil || document.Artifact.RepositoryURL != config.RepositoryURL || document.Artifact.Architecture != journal.Architecture {
		return errInvalidWindowsActivation
	}
	current := document.Source
	if !((current.Version == journal.PreviousVersion && current.SHA256 == journal.PreviousBinary.SHA256 && current.Length == journal.PreviousBinary.Length) || (current.Version == journal.Version && current.SHA256 == journal.Runtime.SHA256 && current.Length == journal.Runtime.Length)) {
		return errInvalidWindowsActivation
	}
	if current.Version == journal.PreviousVersion && journal.PreviousSource != nil && current != *journal.PreviousSource || current.Version == journal.Version && current.Distribution != installsource.Official {
		return errInvalidWindowsActivation
	}
	source, err := windowsJournalInstallSource(journal, current.AutomaticUpdates)
	if err != nil {
		return err
	}
	target := workerupdate.ComponentTarget{SHA256: source.SHA256, Length: source.Length, Platform: source.Platform, Architecture: source.Architecture}
	if err := verifyWindowsStableBinary(ctx, config.Binary, target, config.OwnerSID); err != nil {
		return err
	}
	document.Source = source
	document.Artifact.Version, document.Artifact.Platform, document.Artifact.Architecture = source.Version, source.Platform, source.Architecture
	updated, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return atomicfile.Write(config.InstallState, updated, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + config.OwnerSID + ")"})
	}); err != nil {
		return err
	}
	return applyWindowsReleaseACL(config.InstallState, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;"+config.OwnerSID+")")
}

func reconcileWindowsInstallVersion(ctx context.Context, config WindowsConfig) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil {
		return err
	}
	if !windowsPinnedServiceExecutable(layout, executable) {
		return errInvalidWindowsActivation
	}
	if _, err := service.VerifyOwnedWindowsRoleExecutable(service.UpdaterKind, layout.Instance, executable); err != nil {
		return err
	}
	version, err := WindowsFeatureVersion(ctx, config)
	if err != nil {
		return err
	}
	if version != config.ActiveVersion {
		return errInvalidWindowsActivation
	}
	return nil
}

// WindowsFeatureVersion separates the current feature bytes from the immutable
// native owner image. Only the existing protected transaction may select
// uncommitted cutover/rollback bytes instead of the installed Source.
func WindowsFeatureVersion(ctx context.Context, config WindowsConfig) (string, error) {
	journal, err := loadWindowsActivationJournal(config)
	if err == nil && journal.Stage != windowsActivationAwaitingApproval && journal.Stage != windowsActivationStaged {
		if err := secureWindowsFileShape(windowsActivationJournalPath(config.StateRoot)); err != nil {
			return "", err
		}
		if !windowsMachineFileSecurityMatches(windowsActivationJournalPath(config.StateRoot), "D:P(A;;FA;;;SY)(A;;FA;;;BA)") {
			return "", errInvalidWindowsActivation
		}
		candidate := windowsActivationComponentTarget(journal.Runtime, config.Architecture)
		previous := windowsActivationComponentTarget(journal.PreviousBinary, config.Architecture)
		switch journal.Stage {
		case windowsActivationSwitching, windowsActivationServicesLive, windowsActivationCommitReady, windowsActivationCommitted:
			if matchesWindowsComponent(config.Binary, candidate) {
				if err := verifyWindowsStableBinary(ctx, config.Binary, candidate, config.OwnerSID); err != nil {
					return "", err
				}
				return journal.Version, nil
			}
		case windowsActivationRollingBack, windowsActivationRollbackReady, windowsActivationRolledBack:
			if matchesWindowsComponent(config.Binary, previous) {
				if err := verifyWindowsStableBinary(ctx, config.Binary, previous, config.OwnerSID); err != nil {
					return "", err
				}
				return journal.PreviousVersion, nil
			}
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if config.Source.Validate() != nil {
		return "", errInvalidWindowsActivation
	}
	if err := config.Source.Verify(config.Binary); err != nil {
		return "", err
	}
	return config.Source.Version, nil
}
func windowsPinnedTargetVersion(target windowsServiceTarget, featureVersion string) string {
	if strings.EqualFold(filepath.Base(target.Executable), "pb.exe") && strings.EqualFold(filepath.Base(filepath.Dir(filepath.Dir(target.Executable))), "versions") {
		return filepath.Base(filepath.Dir(target.Executable))
	}
	return featureVersion
}

func (b *windowsSCMActivationBackend) Quarantine(_ context.Context, journal windowsActivationJournal) error {
	path := filepath.Join(filepath.Dir(journal.Runtime.Path), ".quarantined")
	if err := atomicfile.Write(path, []byte(boundedWindowsActivationFailure(errors.New(journal.Failure))+"\n"), atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}); err != nil {
		return err
	}
	return applyWindowsReleaseACL(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)")
}

func setWindowsServiceTarget(name string, target windowsServiceTarget) error {
	if !filepath.IsAbs(target.Executable) || len(target.Arguments) == 0 || len(target.Arguments) > 16 {
		return errInvalidWindowsActivation
	}
	if strings.HasPrefix(name, windowsHostdService+"-") && (len(target.Arguments) != 4 || target.Arguments[0] != "daemon" || target.Arguments[1] != "__runtime-hostd" || target.Arguments[2] != "--instance" || name != windowsHostdService+"-"+target.Arguments[3]) || strings.HasPrefix(name, windowsUpdaterService+"-") && (len(target.Arguments) != 4 || target.Arguments[0] != "daemon" || target.Arguments[1] != "__runtime-updated" || target.Arguments[2] != "--instance" || name != windowsUpdaterService+"-"+target.Arguments[3]) {
		return errInvalidWindowsActivation
	}
	if strings.HasPrefix(name, windowsSSHService+"-") && !validWindowsSSHArguments(target.Arguments) {
		return errInvalidWindowsActivation
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(name)
	if err != nil {
		return err
	}
	defer item.Close()
	config, err := item.Config()
	if err != nil {
		return err
	}
	config.BinaryPathName = windows.ComposeCommandLine(append([]string{target.Executable}, target.Arguments...))
	config.StartType = mgr.StartAutomatic
	config.ErrorControl = mgr.ErrorNormal
	config.ServiceStartName = "LocalSystem"
	config.SidType = windows.SERVICE_SID_TYPE_UNRESTRICTED
	config.DelayedAutoStart = false
	if err := item.UpdateConfig(config); err != nil {
		return err
	}
	updated, err := item.Config()
	if err != nil || !strings.EqualFold(updated.BinaryPathName, config.BinaryPathName) || !validPrivilegedWindowsServiceConfig(updated, mgr.StartAutomatic, mgr.ErrorNormal) {
		return errors.Join(errInvalidWindowsActivation, err)
	}
	return validateWindowsRecoveryActions(item, windowsRecoveryActionsForService(name))
}
func stopNamedWindowsServices(ctx context.Context, names ...string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	for _, name := range names {
		item, err := manager.OpenService(name)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			continue
		}
		if err != nil {
			return err
		}
		_, controlErr := item.Control(svc.Stop)
		if controlErr != nil && !errors.Is(controlErr, windows.ERROR_SERVICE_NOT_ACTIVE) {
			item.Close()
			return controlErr
		}
		for {
			status, queryErr := item.Query()
			if queryErr != nil {
				item.Close()
				return queryErr
			}
			if status.State == svc.Stopped {
				break
			}
			select {
			case <-ctx.Done():
				item.Close()
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		item.Close()
	}
	return nil
}
func startNamedWindowsService(ctx context.Context, name string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("start Windows service %s: %w", name, err)
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(name)
	if err != nil {
		return fmt.Errorf("start Windows service %s: %w", name, err)
	}
	defer item.Close()
	if err := item.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start Windows service %s: %w", name, err)
	}
	for {
		status, err := item.Query()
		if err != nil {
			return fmt.Errorf("start Windows service %s: %w", name, err)
		}
		if status.State == svc.Running {
			return nil
		}
		if status.State == svc.Stopped {
			return fmt.Errorf("start Windows service %s: stopped (Win32 exit %d, service exit %d): %w", name, status.Win32ExitCode, status.ServiceSpecificExitCode, errInvalidWindowsActivation)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("start Windows service %s: %w", name, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func loadWindowsActivationJournal(config WindowsConfig) (windowsActivationJournal, error) {
	file, err := os.Open(windowsActivationJournalPath(config.StateRoot))
	if err != nil {
		return windowsActivationJournal{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(bufio.NewReader(io.LimitReader(file, 128<<10)))
	decoder.DisallowUnknownFields()
	var journal windowsActivationJournal
	var extra any
	if decoder.Decode(&journal) != nil || decoder.Decode(&extra) != io.EOF {
		return windowsActivationJournal{}, fmt.Errorf("%w: activation journal cannot be decoded", errInvalidWindowsActivation)
	}
	if !validWindowsActivationJournal(journal) {
		return windowsActivationJournal{}, fmt.Errorf("%w: activation journal identity or policy is invalid", errInvalidWindowsActivation)
	}
	if !validWindowsActivationPaths(config, journal) {
		return windowsActivationJournal{}, fmt.Errorf("%w: activation journal paths do not match the installed owner", errInvalidWindowsActivation)
	}
	return journal, nil
}

func resumeWindowsActivation(ctx context.Context, config WindowsConfig) (bool, error) {
	journal, err := loadWindowsActivationJournal(config)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !windowsActivationNeedsResume(journal, config.ActiveVersion, false) {
		return false, nil
	}
	activatorOwnsTransaction, err := windowsActivatorOwnsTransaction(config.OwnerSID)
	if err != nil {
		return false, err
	}
	if !windowsActivationNeedsResume(journal, config.ActiveVersion, activatorOwnsTransaction) {
		return false, nil
	}
	target := workerupdate.ComponentTarget{SHA256: journal.Updater.SHA256, Length: journal.Updater.Length, Platform: "windows", Architecture: journal.Architecture}
	if !matchesWindowsComponent(journal.Updater.Path, target) {
		return false, errInvalidWindowsActivation
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := nativesignature.New(nil).Verify(verifyCtx, journal.Updater.Path, "windows", journal.Architecture); err != nil {
		return false, err
	}
	// Signature verification can take long enough for an activator that was
	// already starting to become visible in SCM. Re-check ownership immediately
	// before mutating the activator service so this updater never steals a live
	// transaction during that handoff window.
	activatorOwnsTransaction, err = windowsActivatorOwnsTransaction(config.OwnerSID)
	if err != nil {
		return false, err
	}
	if activatorOwnsTransaction {
		return false, nil
	}
	if err := installWindowsActivatorService(journal.Updater.Path, config.OwnerSID); err != nil {
		return false, err
	}
	if err := startWindowsActivatorService(config.OwnerSID); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return false, err
	}
	return true, nil
}

func windowsActivatorOwnsTransaction(ownerSID string) (bool, error) {
	_, _, _, _, name, err := windowsInstanceNames(ownerSID)
	if err != nil {
		return false, err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return false, err
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer item.Close()
	status, err := item.Query()
	if err != nil {
		return false, err
	}
	return status.State != svc.Stopped, nil
}

func validWindowsActivationPaths(config WindowsConfig, journal windowsActivationJournal) bool {

	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil {
		return false
	}
	paths, err := canonicalWindowsRelease(layout, journal.Version)
	if err != nil || !strings.EqualFold(journal.Runtime.Path, paths.Runtime) || !strings.EqualFold(journal.CLI.Path, paths.CLI) || !strings.EqualFold(journal.Hostd.Path, paths.Hostd) || !strings.EqualFold(journal.Updater.Path, paths.Updater) {
		return false
	}
	if !strings.EqualFold(journal.PreviousBinary.Path, layout.Binary) {
		return false
	}
	if !journal.Release.SupervisorMaintenance {
		previousPaths, err := canonicalWindowsRelease(layout, journal.PreviousVersion)
		if err != nil || !strings.EqualFold(journal.PreviousRuntime.Path, previousPaths.Runtime) {
			return false
		}
		for _, pair := range [][2]windowsServiceTarget{{journal.OldHostd, journal.NewHostd}, {journal.OldUpdater, journal.NewUpdater}, {journal.OldSSH, journal.NewSSH}} {
			if pair[0].Executable != pair[1].Executable || !slices.Equal(pair[0].Arguments, pair[1].Arguments) || pair[0].Executable != "" && !windowsPinnedServiceExecutable(layout, pair[0].Executable) {
				return false
			}
		}
	} else {
		if !strings.EqualFold(journal.NewHostd.Executable, paths.Runtime) || !strings.EqualFold(journal.NewUpdater.Executable, paths.Runtime) || journal.NewSSH.Executable != "" && !strings.EqualFold(journal.NewSSH.Executable, paths.Runtime) {
			return false
		}
		if !windowsOwnedServiceExecutable(layout, journal.OldHostd.Executable) || !windowsOwnedServiceExecutable(layout, journal.OldUpdater.Executable) || journal.OldSSH.Executable != "" && !windowsOwnedServiceExecutable(layout, journal.OldSSH.Executable) {
			return false
		}
	}
	for _, target := range []windowsServiceTarget{journal.OldHostd, journal.NewHostd, journal.OldUpdater, journal.NewUpdater, journal.OldSSH, journal.NewSSH} {
		if target.Executable == "" {
			continue
		}
		if !filepath.IsAbs(target.Executable) || filepath.Clean(target.Executable) != target.Executable || len(target.Arguments) != 4 || target.Arguments[2] != "--instance" || target.Arguments[3] != layout.Instance {
			return false
		}
		for _, argument := range target.Arguments {
			if strings.ContainsAny(argument, "\x00\r\n") || len(argument) > 4096 {
				return false
			}
		}
	}
	return true
}

// RunWindowsActivator is the fixed one-shot SCM entry. Its only input is the
// protected journal created by the signed updater.
func RunWindowsActivator(ctx context.Context, config WindowsConfig) error {
	journal, err := loadWindowsActivationJournal(config)
	if err != nil {
		return err
	}
	backend := newWindowsSCMActivationBackend(config)
	result, activationErr := executeWindowsActivation(ctx, backend, journal)
	return finishWindowsActivatorResult(ctx, result, activationErr, backend.restoreFeatureUpdater, func() error {
		manager, err := mgr.Connect()
		if err != nil {
			return err
		}
		defer manager.Disconnect()
		_, _, _, _, name, err := windowsInstanceNames(config.OwnerSID)
		if err != nil {
			return err
		}
		item, err := manager.OpenService(name)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		if err != nil {
			return err
		}
		defer item.Close()
		if err := item.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return err
		}
		return nil
	})
}

// Keep the durable activator until its terminal feature installation has a
// usable updater again. Retrying this terminal step never repeats cutover.
func finishWindowsActivatorResult(ctx context.Context, journal windowsActivationJournal, activationErr error, restoreUpdater func(context.Context, windowsActivationJournal) error, retire func() error) error {
	terminal := journal.Stage == windowsActivationCommitted && activationErr == nil || journal.Stage == windowsActivationRolledBack
	if !terminal {
		return activationErr
	}
	if !journal.Release.SupervisorMaintenance {
		if err := restoreUpdater(ctx, journal); err != nil {
			return errors.Join(activationErr, err)
		}
	}
	// Rolled-back transactions intentionally retain the original failure in
	// their journal. It is no longer a service failure after usable recovery.
	return retire()
}

func (b *windowsSCMActivationBackend) restoreFeatureUpdater(ctx context.Context, journal windowsActivationJournal) error {
	_, updaterName, err := windowsInstanceServiceNames(b.config.OwnerSID)
	if err != nil {
		return err
	}
	client, err := NewClient(b.config.ControlSocket, 2*time.Second)
	if err != nil {
		return err
	}
	return restoreWindowsFeatureUpdater(ctx, journal, func(ctx context.Context, j windowsActivationJournal) error {
		if err := b.verifyFeatureNativePins(ctx, j); err != nil {
			return err
		}
		// Terminal recovery also repairs a commit interrupted before installer
		// metadata was persisted, before starting the trusted native updater.
		return b.CommitCLI(ctx, j)
	}, func(ctx context.Context) error {
		return startNamedWindowsService(ctx, updaterName)
	}, client.Status)
}

// Native executable identity is independent of the feature version reported by
// the updater. Starting the installed old pin is safe only after its SCM target,
// protected declaration and executable bytes still match the approved journal.
func (b *windowsSCMActivationBackend) verifyFeatureNativePins(ctx context.Context, journal windowsActivationJournal) error {
	layout, err := service.WindowsUserLayout(b.config.OwnerSID)
	if err != nil {
		return err
	}
	hostdName, updaterName, err := windowsInstanceServiceNames(b.config.OwnerSID)
	if err != nil {
		return err
	}
	for _, role := range []struct {
		name, kind, argument string
		expected             windowsServiceTarget
	}{
		{hostdName, service.HostdKind, "__runtime-hostd", journal.OldHostd},
		{updaterName, service.UpdaterKind, "__runtime-updated", journal.OldUpdater},
	} {
		actual, err := queryWindowsServiceTarget(role.name, role.argument)
		if err != nil {
			return err
		}
		identity, err := service.VerifyOwnedWindowsRoleExecutable(role.kind, layout.Instance, actual.Executable)
		if err != nil {
			return err
		}
		if err := validateWindowsFeatureNativePin(layout, role.expected, actual, identity); err != nil {
			return err
		}
		if err := verifyWindowsStableBinary(ctx, actual.Executable, workerupdate.ComponentTarget{SHA256: identity.SHA256, Length: identity.Length, Platform: "windows", Architecture: journal.Architecture}, b.config.OwnerSID); err != nil {
			return err
		}
	}
	return nil
}

func validateWindowsFeatureNativePin(layout service.Layout, expected, actual windowsServiceTarget, identity service.WindowsExecutableIdentity) error {
	if !windowsPinnedServiceExecutable(layout, actual.Executable) || !strings.EqualFold(expected.Executable, actual.Executable) || !strings.EqualFold(identity.Executable, actual.Executable) || !slices.Equal(expected.Arguments, actual.Arguments) || expected.Length <= 0 || expected.Length != identity.Length || len(expected.SHA256) != 64 || expected.SHA256 != identity.SHA256 {
		return errInvalidWindowsActivation
	}
	return nil
}

func restoreWindowsFeatureUpdater(ctx context.Context, journal windowsActivationJournal, verify func(context.Context, windowsActivationJournal) error, start func(context.Context) error, status func(context.Context) (ControlResponse, error)) error {
	if ctx == nil || journal.Release.SupervisorMaintenance || journal.Stage != windowsActivationCommitted && journal.Stage != windowsActivationRolledBack {
		return errInvalidWindowsActivation
	}
	version := journal.Version
	if journal.Stage == windowsActivationRolledBack {
		version = journal.PreviousVersion
	}
	if !validObservedRuntimeVersion(version) {
		return errInvalidWindowsActivation
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if err := verify(bounded, journal); err != nil {
		return err
	}
	// A previous start may have succeeded before its acknowledgement was lost.
	// Reuse its authenticated control readiness rather than restarting it.
	if response, err := status(bounded); err == nil && response.Version == version {
		return nil
	}
	if err := start(bounded); err != nil {
		return err
	}
	return waitForWindowsUpdaterVersion(bounded, version, 30*time.Second, 250*time.Millisecond, status)
}

// Runtime observations include locally-built versions; update candidates still
// require the exact signed release version contract.
func validObservedRuntimeVersion(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n\t ")
}

func verifyWindowsPreparedCandidate(ctx context.Context, j windowsActivationJournal) error {
	if j.Candidate.ID == "" || j.Candidate.Version != j.Version || j.Candidate.SHA256 != j.Runtime.SHA256 || j.Candidate.Length != j.Runtime.Length {
		return workerupdate.ErrPreparedCandidate
	}
	target := windowsActivationComponentTarget(j.Runtime, j.Architecture)
	if err := verifyWindowsActivationComponent(ctx, j.Runtime.Path, target); err != nil {
		return errors.Join(workerupdate.ErrPreparedCandidate, err)
	}
	return nil
}

func validWindowsRuntimeStatus(status hostdproto.Status, version string) bool {
	return status.State == hostdproto.StateActive && status.WorkerID == "runtime-"+strings.ReplaceAll(version, " ", "-") && status.Epoch > 0 && status.APIVersion > 0 && status.LastHeartbeatUnixMilli > 0 && time.Since(time.UnixMilli(status.LastHeartbeatUnixMilli)) <= 15*time.Second
}

func (b *windowsSCMActivationBackend) ownerControlClient() (*hostdproto.Client, error) {
	token, err := os.ReadFile(b.config.TokenFile)
	if err != nil {
		return nil, err
	}
	defer clear(token)
	return hostdproto.NewClient(b.config.HostdSocket, token, 31*time.Minute)
}
func (b *windowsSCMActivationBackend) featureGate(ctx context.Context, j windowsActivationJournal, operation string) (hostdproto.UpdateGateResponse, error) {
	client, err := b.ownerControlClient()
	if err != nil {
		return hostdproto.UpdateGateResponse{}, err
	}
	r := hostdproto.UpdateGateRequest{Operation: operation, TransactionID: j.TransactionID, Version: j.Version, ManifestSHA256: j.Release.ManifestSHA256}
	if operation != hostdproto.UpdateGateTarget {
		r.ExpectedTarget = j.GateTarget
	}
	switch operation {
	case hostdproto.UpdateGateCandidate:
		r.Path = j.Release.CanaryPath
		r.ExpectedStatus = j.Release.CanaryStatus
		r.Samples = j.Release.CanarySamples
		r.TimeoutMillis = j.Release.CanaryTimeout.Milliseconds()
	case hostdproto.UpdateGateDrain:
		r.PreviousVersion = j.PreviousVersion
		r.TimeoutMillis = j.Release.DrainTimeout.Milliseconds()
	case hostdproto.UpdateGateStability:
		r.Path = j.Release.CanaryPath
		r.ExpectedStatus = j.Release.CanaryStatus
		r.Samples = j.Release.CanarySamples
		r.WindowMillis = j.Release.StabilityWindow.Milliseconds()
		r.IntervalMillis = j.Release.StabilityInterval.Milliseconds()
	case hostdproto.UpdateGateRollback:
		r.PreviousVersion = j.PreviousVersion
		r.Path = j.Release.CanaryPath
		r.ExpectedStatus = j.Release.CanaryStatus
		r.Samples = j.Release.CanarySamples
		r.TimeoutMillis = j.Release.RollbackTimeout.Milliseconds()
	}
	return client.UpdateGate(ctx, r)
}
func (b *windowsSCMActivationBackend) PrepareFeature(ctx context.Context, j windowsActivationJournal) (hostdproto.UpdateGateTargetBinding, error) {
	target, err := b.featureGate(ctx, j, hostdproto.UpdateGateTarget)
	if err != nil {
		return target.Target, err
	}
	j.GateTarget = &target.Target
	if _, err = b.featureGate(ctx, j, hostdproto.UpdateGateCandidate); err != nil {
		return target.Target, err
	}
	drained, err := b.featureGate(ctx, j, hostdproto.UpdateGateDrain)
	if err != nil {
		return target.Target, err
	}
	if drained.BlockedReason != "" {
		return target.Target, workerupdate.ErrActivationGate
	}
	return target.Target, nil
}
func (b *windowsSCMActivationBackend) startFeature(ctx context.Context, path, version string, min, max uint16) error {
	client, err := b.ownerControlClient()
	if err != nil {
		return err
	}
	worker, err := (workerupdate.OwnerStarter{Client: client}).Start(ctx, workerupdate.StartRequest{Executable: path, WorkerID: "runtime-" + strings.ReplaceAll(version, " ", "-"), Release: workerupdate.Release{Version: version, HostdAPIMin: min, HostdAPIMax: max}, MutationsDisabled: true})
	if err != nil {
		return err
	}
	if _, err = worker.Ready(ctx); err != nil {
		_ = worker.Stop(context.WithoutCancel(ctx))
		return err
	}
	if _, err = worker.Activate(ctx); err != nil {
		_ = worker.Stop(context.WithoutCancel(ctx))
		return err
	}
	return nil
}
func (b *windowsSCMActivationBackend) ActivateFeature(ctx context.Context, j windowsActivationJournal) error {
	client, err := b.ownerControlClient()
	if err != nil {
		return err
	}
	if err = client.StopActive(ctx); err != nil {
		return err
	}
	if err = b.ActivateBinary(ctx, j); err != nil {
		return err
	}
	return b.startFeature(ctx, j.Runtime.Path, j.Version, j.Release.HostdAPIMin, j.Release.HostdAPIMax)
}
func (b *windowsSCMActivationBackend) RestoreFeature(ctx context.Context, j windowsActivationJournal) error {
	client, err := b.ownerControlClient()
	if err != nil {
		return err
	}
	if err = client.StopActive(ctx); err != nil {
		return err
	}
	if err = b.RestoreBinary(ctx, j); err != nil {
		return err
	}
	if err = verifyWindowsActivationComponent(ctx, j.PreviousRuntime.Path, windowsActivationComponentTarget(j.PreviousRuntime, j.Architecture)); err != nil {
		return err
	}
	if err = b.startFeature(ctx, j.PreviousRuntime.Path, j.PreviousVersion, 1, 1); err != nil {
		return err
	}
	if j.GateTarget != nil {
		_, err = b.featureGate(ctx, j, hostdproto.UpdateGateRollback)
	}
	return err
}
func (b *windowsSCMActivationBackend) VerifyFeature(ctx context.Context, j windowsActivationJournal) error {
	status, err := b.activeHostdStatus(ctx)
	if err != nil || status.WorkerID != "runtime-"+strings.ReplaceAll(j.Version, " ", "-") || status.Epoch == 0 {
		return errors.Join(errInvalidWindowsActivation, err)
	}
	_, err = b.featureGate(ctx, j, hostdproto.UpdateGateStability)
	return err
}
func (b *windowsSCMActivationBackend) CompleteFeature(ctx context.Context, j windowsActivationJournal) error {
	_, err := b.featureGate(ctx, j, hostdproto.UpdateGateCommit)
	if err != nil {
		return err
	}
	return b.clearCommittedOwnerMaintenance(j)
}

func windowsPinnedServiceExecutable(layout service.Layout, path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	relative, err := filepath.Rel(filepath.Join(layout.ReleasesRoot, "versions"), path)
	if err != nil {
		return false
	}
	parts := strings.Split(relative, string(filepath.Separator))
	return len(parts) == 2 && validObservedRuntimeVersion(parts[0]) && parts[0] != ".." && strings.EqualFold(parts[1], "pb.exe")
}
func windowsOwnedServiceExecutable(layout service.Layout, path string) bool {
	return strings.EqualFold(path, layout.Binary) || strings.EqualFold(path, layout.BinaryRollback) || windowsPinnedServiceExecutable(layout, path)
}

func (b *windowsSCMActivationBackend) AuthorizeOwnerMaintenance(ctx context.Context, release workerupdate.Release, manual bool) error {
	if !release.SupervisorMaintenance {
		return errInvalidWindowsActivation
	}
	if b.config.AuthorizeOwnerMaintenance != nil {
		return b.config.AuthorizeOwnerMaintenance(ctx, release, manual)
	}
	client, err := b.ownerControlClient()
	if err != nil {
		return err
	}
	return authorizeOwnerMaintenance(ctx, b.config.StateRoot, client, release, manual)
}

func (b *windowsSCMActivationBackend) AbortOwnerMaintenance(ctx context.Context) error {
	client, err := b.ownerControlClient()
	if err != nil {
		return err
	}
	return client.AbortMaintenance(ctx)
}
func (b *windowsSCMActivationBackend) AbortFeature(ctx context.Context, j windowsActivationJournal) error {
	if j.GateTarget == nil {
		return nil
	}
	_, err := b.featureGate(ctx, j, hostdproto.UpdateGateRollback)
	return err
}

func (b *windowsSCMActivationBackend) clearCommittedOwnerMaintenance(j windowsActivationJournal) error {
	path := filepath.Join(b.config.StateRoot, "owner-maintenance.json")
	notice, err := autoupdate.LoadOwnerMaintenance(path)
	if err != nil {
		return err
	}
	if notice != nil && notice.CandidateID == j.Candidate.ID {
		return autoupdate.ClearOwnerMaintenance(path)
	}
	return nil
}
