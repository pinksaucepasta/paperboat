//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// readCarrierGenerationFile reads one generation's own descriptor, active or not (an update's standby candidate).
func readCarrierGenerationFile(ctx context.Context, instance, generation string) (CarrierConfig, error) {
	if !sessionowner.ValidGeneration(generation) {
		return CarrierConfig{}, ErrInvalidRequest
	}
	c, err := readWindowsCarrierFile(ctx, instance, carrierGenerationPath(instance, generation))
	if err == nil && c.Generation != generation {
		return c, ErrInvalidRequest
	}
	return c, err
}

// carrierGenerationPath is one generation's own descriptor, which its carrier process reads. config.json is a copy
// of the active generation's descriptor, as on Linux and macOS (carrier_unix.go).
func carrierGenerationPath(instance, generation string) string {
	return filepath.Join(filepath.Dir(carrierConfigPath(instance)), "generation-"+generation+".json")
}

func carrierConfigPath(instance string) string {
	root, err := WindowsInstanceRoot(instance)
	if err != nil {
		return ""
	}
	return filepath.Join(root, "carrier", "config.json")
}
func carrierServiceRoot(c CarrierConfig) string {
	root, _ := WindowsInstanceRoot(c.Instance)
	return root
}
func validateCarrierPaths(c CarrierConfig) error {
	instance, err := WindowsInstanceForSID(c.OwnerSID)
	if err != nil || instance != c.Instance {
		return ErrInvalidRequest
	}
	layout, err := service.WindowsUserLayout(c.OwnerSID)
	if err != nil {
		return err
	}
	pin, err := WindowsPinnedSourcePath(layout, c.Source)
	if err != nil {
		return err
	}
	root := filepath.Dir(carrierConfigPath(instance))
	token, _ := WindowsInstanceTokenPath(instance)
	if !strings.EqualFold(c.Executable, pin) || c.HostdEndpoint != layout.HostdSocket || c.HostdTokenFile != token || c.DispatchCapabilityFile != filepath.Join(root, "dispatch.cap") || c.LockPath != filepath.Join(root, "owner-"+c.Generation+".lock") {
		return ErrInvalidRequest
	}
	return nil
}
func prepareCarrierConfig(ctx context.Context, r Request) (CarrierConfig, error) {
	admin, err := checkAdministrator()
	if err != nil {
		return CarrierConfig{}, err
	}
	if !admin {
		return CarrierConfig{}, ErrNotPrivileged
	}
	if r.EnrollmentPending || r.Source.Validate() != nil || r.Source.Platform != "windows" || r.Source.Architecture != runtime.GOARCH {
		return CarrierConfig{}, ErrInvalidRequest
	}
	layout, err := service.WindowsUserLayout(r.OwnerSID)
	if err != nil {
		return CarrierConfig{}, err
	}
	pin, err := ensureWindowsPinnedRuntimeFrom(ctx, layout, r.Source, r.OwnerSID, r.Executable)
	if err != nil {
		return CarrierConfig{}, err
	}
	return deriveWindowsCarrierConfig(r, layout, pin)
}

// Pure derivation permits the lifecycle to capture an absent native declaration
// before publishing either descriptor or executable.
func deriveWindowsCarrierConfig(r Request, layout service.Layout, pin string) (CarrierConfig, error) {
	root := filepath.Dir(carrierConfigPath(layout.Instance))
	token, _ := WindowsInstanceTokenPath(layout.Instance)
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return CarrierConfig{}, err
	}
	environment := map[string]string{"PAPERBOAT_WINDOWS_OWNER_SID": r.OwnerSID, "PAPERBOAT_RUNTIME_STATE_ROOT": r.StateRoot, "PAPERBOAT_WORKSPACE_ROOT": r.WorkspaceRoot, "PAPERBOAT_CONTROL_URL": r.ControlURL, "PAPERBOAT_MACHINE_ID": r.UserMachineID, "PAPERBOAT_SHELL": filepath.Join(system, "cmd.exe"), "PAPERBOAT_RUNTIME_LISTEN_ADDRESS": r.HelperListenAddress, "PAPERBOAT_RUNTIME_SERVICE_SCOPE": "user"}
	if r.Home != "" {
		environment["HOME"] = r.Home
	}
	if r.Path != "" {
		environment["PATH"] = r.Path
	}

	generation, err := sourceGeneration(r.Source)
	if err != nil {
		return CarrierConfig{}, err
	}
	c := CarrierConfig{Schema: carrierSchema, Instance: layout.Instance, Source: r.Source, Generation: generation, Executable: pin, UID: r.UID, GID: r.GID, OwnerSID: r.OwnerSID, User: "Paperboat", Group: "Paperboat", RuntimeRoot: r.StateRoot, Environment: environment, WorkerAddress: r.HelperListenAddress, HostdEndpoint: layout.HostdSocket, ControlEndpoint: carrierControlEndpoint(layout.HostdSocket, generation), HostdTokenFile: token, DispatchCapabilityFile: filepath.Join(root, "dispatch.cap"), LockPath: filepath.Join(root, "owner-"+generation+".lock")}
	return c, c.Validate()
}
// readCarrierConfigFile reads the active generation's descriptor.
func readCarrierConfigFile(ctx context.Context, instance string) (CarrierConfig, error) {
	return readWindowsCarrierFile(ctx, instance, carrierConfigPath(instance))
}

func readWindowsCarrierFile(ctx context.Context, instance, path string) (CarrierConfig, error) {
	if ctx == nil {
		return CarrierConfig{}, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return CarrierConfig{}, err
	}
	if path == "" || carrierConfigPath(instance) == "" {
		return CarrierConfig{}, ErrInvalidRequest
	}
	// The parent is protected before JSON is used as account custody input.
	root, _ := WindowsInstanceRoot(instance)
	trusted, err := windowsRuntimeTrustedOwner()
	if err != nil {
		return CarrierConfig{}, err
	}
	if _, err := os.Lstat(path); err != nil {
		return CarrierConfig{}, err
	}
	// Decode is bounded and grants no authority until handle owner and exact ACL checks below.
	file, err := os.Open(path)
	if err != nil {
		return CarrierConfig{}, err
	}
	body, err := io.ReadAll(io.LimitReader(file, (32<<10)+1))
	err = errors.Join(err, file.Close())
	if err != nil {
		return CarrierConfig{}, err
	}
	c, err := decodeCarrier(body)
	if err != nil {
		return c, err
	}
	if c.Instance != instance || c.Validate() != nil {
		return c, ErrInvalidRequest
	}
	if err := validateWindowsRuntimeSecurity(root, trusted, windowsRuntimeCurrentRootDACL(c.OwnerSID), "carrier account root"); err != nil {
		return c, err
	}
	f, err := openCarrierProtectedFile(c, path, false)
	if err != nil {
		return c, err
	}
	verified, err := io.ReadAll(io.LimitReader(f, (32<<10)+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return c, err
	}
	if string(verified) != string(body) {
		return c, ErrInvalidRequest
	}
	return c, nil
}
func carrierLockDACL(sid string) string {
	return fmt.Sprintf("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x%x;;;%s)", windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE, sid)
}
func openCarrierProtectedFile(c CarrierConfig, path string, writable bool) (*os.File, error) {
	trusted, err := windowsRuntimeTrustedOwner()
	if err != nil {
		return nil, err
	}
	if err := validateWindowsRuntimeSecurity(filepath.Dir(path), trusted, windowsRuntimeCurrentRootDACL(c.OwnerSID), "carrier root"); err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ)
	dacl := windowsRuntimeCurrentFileDACL(c.OwnerSID)
	if writable {
		access |= windows.GENERIC_WRITE
		dacl = carrierLockDACL(c.OwnerSID)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	var st windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &st); err != nil {
		f.Close()
		return nil, err
	}
	if st.NumberOfLinks != 1 || st.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		f.Close()
		return nil, ErrInvalidRequest
	}
	if err := checkWindowsRuntimeHandleSecurity(handle, trusted, dacl); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func publishCarrierFiles(c CarrierConfig, path string, capability []byte) error {
	if err := ensureWindowsProtectedMachineDirectory(filepath.Dir(carrierConfigPath(c.Instance)), c.OwnerSID, windowsRuntimeCurrentRootDACL(c.OwnerSID)); err != nil {
		return err
	}
	for _, item := range []struct {
		path string
		body []byte
		lock bool
	}{{c.DispatchCapabilityFile, capability, false}, {c.LockPath, []byte{}, true}} {
		if _, err := os.Lstat(item.path); err == nil {
			f, err := openCarrierProtectedFile(c, item.path, item.lock)
			if err != nil {
				return err
			}
			if err = f.Close(); err != nil {
				return err
			}
			continue
		} else if !installNotExistOnly(err) {
			return err
		}
		dacl := windowsRuntimeCurrentFileDACL(c.OwnerSID)
		if item.lock {
			dacl = carrierLockDACL(c.OwnerSID)
		}
		if err := atomicfile.Write(item.path, item.body, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SY" + dacl}); err != nil {
			return err
		}
	}
	if path == "" {
		return writeWindowsCarrierDescriptors(c, false)
	}
	if path != carrierConfigPath(c.Instance) {
		return ErrInvalidRequest
	}
	return writeWindowsCarrierDescriptors(c, true)
}

// publishCarrierDescriptor marks a generation active: config.json becomes its descriptor.
func publishCarrierDescriptor(c CarrierConfig) error { return writeWindowsCarrierDescriptors(c, true) }

func writeWindowsCarrierDescriptors(c CarrierConfig, active bool) error {
	body, err := jsonCarrier(c)
	if err != nil {
		return err
	}
	options := atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SY" + windowsRuntimeCurrentFileDACL(c.OwnerSID)}
	if err := atomicfile.Write(carrierGenerationPath(c.Instance, c.Generation), body, options); err != nil || !active {
		return err
	}
	return atomicfile.Write(carrierConfigPath(c.Instance), body, options)
}
func verifyCarrierNative(c CarrierConfig, i *service.Installer) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	account, err := token.GetTokenUser()
	err = errors.Join(err, token.Close())
	if err != nil {
		return err
	}
	if account.User.Sid.String() == c.OwnerSID {
		return nil
	}
	if account.User.Sid.String() != "S-1-5-18" {
		return ErrInvalidRequest
	}

	identity, err := service.VerifyOwnedWindowsRoleExecutable(service.CarrierKind, c.Instance+"-"+c.Generation, c.Executable)
	if err != nil {
		return err
	}
	if identity.SHA256 != c.Source.SHA256 || identity.Length != c.Source.Length {
		return ErrInvalidRequest
	}
	return i.VerifyPublishedDefinition()
}
func LockCarrier(c CarrierConfig) (io.Closer, error) {
	if c.Validate() != nil {
		return nil, ErrInvalidRequest
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	user, err := token.GetTokenUser()
	err = errors.Join(err, token.Close())
	if err != nil {
		return nil, err
	}
	if user.User.Sid.String() != c.OwnerSID {
		return nil, ErrInvalidRequest
	}
	f, err := openCarrierProtectedFile(c, c.LockPath, true)
	if err != nil {
		return nil, err
	}
	var region windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &region); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func carrierPublicationLock(ctx context.Context, r Request) (io.Closer, error) {
	allowed, err := checkAdministrator()
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrNotPrivileged
	}
	unlock, err := lockWindowsLocalDaemonMigration(ctx)
	if err != nil {
		return nil, err
	}
	return carrierUnlock(unlock), nil
}

// absentGeneration names a lifecycle component (carrier, hostd) when no generation exists and no source says which
// one would: no source has this SHA-256 prefix, so its service is never registered.
const absentGeneration = "00000000000000000000000000000000"

// windowsCarrierLifecycleInstaller never publishes while constructing a journal. It addresses the active carrier
// generation, or with none yet the one request would create; generationFile is that generation's own descriptor.
func windowsCarrierLifecycleInstaller(ctx context.Context, layout service.Layout, request *Request) (installer *service.Installer, probe func(context.Context) error, generationFile string, resultErr error) {
	c, err := readCarrierConfigFile(ctx, layout.Instance)
	if err == nil {
		i, err := carrierInstallerPending(c)
		return i, func(ctx context.Context) error { return waitCarrierReady(ctx, c) }, carrierGenerationPath(c.Instance, c.Generation), err
	}
	if !installNotExistOnly(err) {
		return nil, nil, "", err
	}
	// No carrier yet: the generation request would create, or for a caller without a source (recovery, repair,
	// uninstall) one that is never installed. Either stands for "no carrier registered" in the journal; restore
	// removes whatever generation the interrupted attempt created (restoreWindowsCarrierSnapshot).
	generation := absentGeneration
	if request != nil {
		if g, err := sourceGeneration(request.Source); err == nil {
			generation = g
		}
	}
	root, err := WindowsInstanceRoot(layout.Instance)
	if err != nil {
		return nil, nil, "", err
	}
	// The cold declaration is path-only pending input. Candidate identity is
	// bound when PrepareCarrier publishes the source-verified declaration.
	i, err := service.NewPending(service.Config{Platform: "windows", Kind: service.CarrierKind, Instance: layout.Instance, Generation: generation, ConfigRoot: root, Executable: layout.Binary, User: "Paperboat", Group: "Paperboat", Arguments: []string{"daemon", "__runtime-carrier", "--instance", layout.Instance, "--generation", generation}, Controller: service.WindowsController{}})
	return i, nil, carrierGenerationPath(layout.Instance, generation), err
}

// restoreWindowsCarrierSnapshot puts the baseline's carrier back: every other generation is the attempt's
// candidate and is removed, the baseline becomes the active generation again and serves (it may have released the
// network to the candidate and exited). Linux and macOS: unixCarrierInstallation.Restore. A generation whose
// service process runs this recovery (hostruntimecmd windowsDeadInstaller) is left for the lifecycle's last step
// (windowsCarrierComponent.RemoveSelf) or, a non-active one, for the updater's sweep.
func restoreWindowsCarrierSnapshot(ctx context.Context, layout service.Layout, baseline *CarrierConfig) error {
	if baseline != nil && (baseline.Instance != layout.Instance || baseline.Validate() != nil) {
		return ErrInvalidRequest
	}
	own, ownErr := windowsCarrierOfThisProcess(ctx, layout.Instance)
	if ownErr != nil {
		return ownErr
	}
	generations, err := windowsCarrierGenerations(ctx, layout.Instance)
	if err != nil {
		return err
	}
	for _, c := range generations {
		if baseline != nil && c.Generation == baseline.Generation || own != nil && c.Generation == own.Generation {
			continue
		}
		if err := removeWindowsCarrierGeneration(ctx, c); err != nil {
			return err
		}
	}
	if baseline == nil {
		if own != nil {
			return nil // windowsCarrierComponent.RemoveSelf removes it and the active descriptor last
		}
		return removeInstallFile(carrierConfigPath(layout.Instance))
	}
	if err := verifyWindowsSource(ctx, baseline.Executable, baseline.Source, baseline.OwnerSID); err != nil {
		return err
	}
	current, err := readCarrierConfigFile(ctx, layout.Instance)
	if err != nil && !installNotExistOnly(err) {
		return err
	}
	want, _ := jsonCarrier(*baseline)
	have, _ := jsonCarrier(current)
	if err != nil || string(want) != string(have) {
		if err := publishCarrierDescriptor(*baseline); err != nil {
			return err
		}
	}
	return EnsureExistingCarrier(ctx, *baseline)
}

// windowsCarrierOfThisProcess is the carrier generation whose running service process is the caller, or nil.
func windowsCarrierOfThisProcess(ctx context.Context, instance string) (own *CarrierConfig, resultErr error) {
	generations, err := windowsCarrierGenerations(ctx, instance)
	if err != nil {
		return nil, err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, manager.Disconnect()) }()
	for _, c := range generations {
		name, err := service.WindowsCarrierName(instance, c.Generation)
		if err != nil {
			return nil, err
		}
		carrier, err := manager.OpenService(name)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			continue
		}
		if err != nil {
			return nil, err
		}
		status, err := carrier.Query()
		closeErr := carrier.Close()
		if err = errors.Join(err, closeErr); err != nil {
			return nil, err
		}
		if status.State != svc.Stopped && status.ProcessId == uint32(os.Getpid()) {
			return &c, nil
		}
	}
	return nil, nil
}

// windowsCarrierIsThisProcess reports whether the caller is the service process of one of the account's carriers.
func windowsCarrierIsThisProcess(ctx context.Context, instance string) (bool, error) {
	own, err := windowsCarrierOfThisProcess(ctx, instance)
	return own != nil, err
}

// windowsCarrierComponent is the carrier in the lifecycle. Removing itself, the carrier's service process also
// removes the active descriptor and its generation's own one, as restoreWindowsCarrierSnapshot does for every other
// caller; they go before the service removal, whose stop ends the caller.
type windowsCarrierComponent struct {
	*service.NativeTransactionalComponent
	config, generation string
}

func (c windowsCarrierComponent) RemoveSelf(ctx context.Context) error {
	for _, path := range []string{c.config, c.generation} {
		if err := removeInstallFile(path); err != nil {
			return err
		}
	}
	return c.NativeTransactionalComponent.RemoveSelf(ctx)
}

func reconcileInstalledCarrier(ctx context.Context, instance string, pinReferences ...func(context.Context, string) ([]string, error)) (resultErr error) {
	config, err := LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return err
	}
	lock, err := carrierPublicationLock(ctx, windowsRepairRequest(config))
	if err != nil {
		return err
	}
	if lock != nil {
		defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	}
	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil {
		return err
	}
	recovery, err := newWindowsLifecycleManager(windowsRepairRequest(config), layout, true)
	if err != nil {
		return err
	}
	if err := recovery.RequireRecovered(ctx); err != nil {
		return fmt.Errorf("carrier reconciliation requires external native repair to finish the pending installation: %w", err)
	}
	config, err = LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return err
	}
	request, err := windowsCarrierInstallRequest(ctx, config)
	if err != nil {
		return err
	}
	retained, err := ReadCarrierConfig(ctx, instance)
	if err != nil {
		return err
	}
	if err := EnsureExistingCarrier(ctx, retained); err != nil {
		return err
	}
	if err := pruneWindowsSSHGenerations(ctx, retained, config.Source); err != nil {
		return err
	}
	// Generations an update retired (exited after releasing the network) or a failed one left standing.
	if err := sweepWindowsCarrierGenerations(ctx, instance, retained.Generation); err != nil {
		return err
	}
	if err := removeWindowsHostdGenerations(ctx, instance, sourceGenerationOrAbsent(config.Source), true); err != nil {
		return err
	}
	if len(pinReferences) == 1 {
		activation, err := pinReferences[0](ctx, instance)
		if err != nil {
			return err
		}
		if err := pruneWindowsRuntimePins(ctx, config, retained, activation); err != nil {
			return pinCleanupFailure{cause: err}
		}
	}
	if retained.Source.SHA256 == config.Source.SHA256 && retained.Source.Length == config.Source.Length {
		return nil
	}
	// The activation moves the carrier with the runtime; a carrier still on another source moves the same way,
	// standby first and the network last, under maintenance and the lifecycle journal.
	abort, err := PrepareSuppliedMaintenance(ctx, layout.HostdSocket, config.TokenFile, false)
	if err != nil {
		return err
	}
	defer abort()
	manager, err := newWindowsLifecycleManager(request, layout, true)
	if err != nil {
		return err
	}
	var upgrade *carrierUpgrade
	err = manager.Replace(ctx, func(ctx context.Context) error {
		if upgrade, err = stageWindowsCarrierUpgrade(ctx, request); err != nil {
			return err
		}
		if err := upgrade.activate(); err != nil {
			return err
		}
		if err := upgrade.switchNetwork(ctx); err != nil {
			return err
		}
		return upgrade.confirmServing(ctx, carrierServingWait)
	})
	return errors.Join(err, upgrade.finish(ctx, err != nil))
}

func windowsCarrierInstallRequest(ctx context.Context, c WindowsRuntimeConfig) (Request, error) {
	layout, err := service.WindowsUserLayout(c.OwnerSID)
	if err != nil {
		return Request{}, err
	}
	if err := verifyWindowsSource(ctx, layout.Binary, c.Source, c.OwnerSID); err != nil {
		return Request{}, err
	}
	pin, err := ensureWindowsPinnedRuntimeFrom(ctx, layout, c.Source, c.OwnerSID, layout.Binary)
	if err != nil {
		return Request{}, err
	}
	r := windowsRepairRequest(c)
	r.Schema, r.Platform, r.User, r.Executable, r.Artifact = SchemaV1, "windows", c.User, pin, c.Artifact
	r.WorkspaceRoot, r.ControlURL, r.UserMachineID = c.Workspace, c.ControlURL, c.MachineID
	r.Home = filepath.Dir(c.StateRoot)
	return r, nil
}
