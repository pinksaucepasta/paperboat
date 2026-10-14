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
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
	"time"
)

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
	if !strings.EqualFold(c.Executable, pin) || c.HostdEndpoint != layout.HostdSocket || c.HostdTokenFile != token || c.DispatchCapabilityFile != filepath.Join(root, "dispatch.cap") || c.LockPath != filepath.Join(root, "owner.lock") {
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

	c := CarrierConfig{Schema: carrierSchema, Instance: layout.Instance, Source: r.Source, Executable: pin, UID: r.UID, GID: r.GID, OwnerSID: r.OwnerSID, User: "Paperboat", Group: "Paperboat", RuntimeRoot: r.StateRoot, Environment: environment, WorkerAddress: r.HelperListenAddress, HostdEndpoint: layout.HostdSocket, ControlEndpoint: layout.HostdSocket + ".carrier", HostdTokenFile: token, DispatchCapabilityFile: filepath.Join(root, "dispatch.cap"), LockPath: filepath.Join(root, "owner.lock")}
	return c, c.Validate()
}
func readCarrierConfigFile(ctx context.Context, instance string) (CarrierConfig, error) {
	if ctx == nil {
		return CarrierConfig{}, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return CarrierConfig{}, err
	}
	path := carrierConfigPath(instance)
	if path == "" {
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
	if err := ensureWindowsProtectedMachineDirectory(filepath.Dir(path), c.OwnerSID, windowsRuntimeCurrentRootDACL(c.OwnerSID)); err != nil {
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
	body, err := jsonCarrier(c)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SY" + windowsRuntimeCurrentFileDACL(c.OwnerSID)})
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

	identity, err := service.VerifyOwnedWindowsRoleExecutable(service.CarrierKind, c.Instance, c.Executable)
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

// windowsCarrierLifecycleInstaller never publishes while constructing a journal.
func windowsCarrierLifecycleInstaller(ctx context.Context, layout service.Layout, request *Request) (*service.Installer, func(context.Context) error, error) {
	c, err := readCarrierConfigFile(ctx, layout.Instance)
	if err == nil {
		i, err := carrierInstallerPending(c)
		return i, func(ctx context.Context) error { return waitCarrierReady(ctx, c) }, err
	}
	if !installNotExistOnly(err) {
		return nil, nil, err
	}
	root, err := WindowsInstanceRoot(layout.Instance)
	if err != nil {
		return nil, nil, err
	}
	// The cold declaration is path-only pending input. Candidate identity is
	// bound when PrepareCarrier publishes the source-verified declaration.
	i, err := service.NewPending(service.Config{Platform: "windows", Kind: service.CarrierKind, Instance: layout.Instance, ConfigRoot: root, Executable: layout.Binary, User: "Paperboat", Group: "Paperboat", Arguments: []string{"daemon", "__runtime-carrier", "--instance", layout.Instance}, Controller: service.WindowsController{}})
	return i, nil, err
}

func restoreWindowsCarrierSnapshot(ctx context.Context, layout service.Layout, baseline *CarrierConfig) error {
	current, err := readCarrierConfigFile(ctx, layout.Instance)
	if err != nil && !installNotExistOnly(err) {
		return err
	}
	if baseline != nil {
		if baseline.Instance != layout.Instance || baseline.Validate() != nil {
			return ErrInvalidRequest
		}
		if err != nil {
			return ErrInvalidRequest
		}
		want, _ := jsonCarrier(*baseline)
		have, _ := jsonCarrier(current)
		if string(want) != string(have) {
			if !sameWindowsCarrierAccount(current, *baseline) {
				return ErrInvalidRequest
			}
			if err := stopAndRemoveWindowsCarrier(ctx, current); err != nil {
				return err
			}
			if err := verifyWindowsSource(ctx, baseline.Executable, baseline.Source, baseline.OwnerSID); err != nil {
				return err
			}
			return writeWindowsCarrierConfig(*baseline)
		}
		// Ordinary worker rollback leaves the carrier and accepted SSH alone.
		return EnsureExistingCarrier(ctx, current)
	}
	if installNotExistOnly(err) {
		i, _, err := windowsCarrierLifecycleInstaller(ctx, layout, nil)
		if err != nil {
			return err
		}
		status, err := (service.WindowsController{}).Inspect(ctx, i.DefinitionPath())
		if err != nil {
			return err
		}
		if status.Registered || status.Running {
			return ErrInvalidRequest
		}
		return nil
	}
	if err := stopAndRemoveWindowsCarrier(ctx, current); err != nil {
		return err
	}
	// Capability and lifetime-lock inodes remain unchanged for a bounded retry.
	return os.Remove(carrierConfigPath(layout.Instance))
}

func sameWindowsCarrierAccount(a, b CarrierConfig) bool {
	a.Source, a.Executable = b.Source, b.Executable
	x, _ := jsonCarrier(a)
	y, _ := jsonCarrier(b)
	return string(x) == string(y)
}
func writeWindowsCarrierConfig(c CarrierConfig) error {
	body, err := jsonCarrier(c)
	if err != nil {
		return err
	}
	return atomicfile.Write(carrierConfigPath(c.Instance), body, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SY" + windowsRuntimeCurrentFileDACL(c.OwnerSID)})
}
func stopAndRemoveWindowsCarrier(ctx context.Context, c CarrierConfig) error {
	if err := verifyWindowsSource(ctx, c.Executable, c.Source, c.OwnerSID); err != nil {
		return err
	}
	i, err := carrierInstaller(c)
	if err != nil {
		return err
	}
	native := service.WindowsController{}
	status, err := native.Inspect(ctx, i.DefinitionPath())
	if err != nil {
		return err
	}
	if status.Registered || status.Running {
		if err := verifyCarrierNative(c, i); err != nil {
			return err
		}
		if err := i.Uninstall(ctx); err != nil {
			return err
		}
	}
	status, err = native.Inspect(ctx, i.DefinitionPath())
	if err != nil {
		return err
	}
	if status.Registered || status.Running {
		return ErrInvalidRequest
	}
	return nil
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
	if len(pinReferences) == 1 {
		activation, err := pinReferences[0](ctx, instance)
		if err != nil {
			return err
		}
		if err := pruneWindowsRuntimePins(ctx, config, retained, activation); err != nil {
			return err
		}
	}
	if retained.Source.SHA256 == config.Source.SHA256 && retained.Source.Length == config.Source.Length {
		return nil
	}
	desired := retained
	desired.Source = config.Source
	desired.Executable, err = ensureWindowsPinnedRuntimeFrom(ctx, layout, config.Source, config.OwnerSID, layout.Binary)
	if err != nil {
		return err
	}
	if err := desired.Validate(); err != nil {
		return err
	}
	capability, err := ReadCarrierCapability(ctx, retained)
	if err != nil {
		return err
	}
	defer clear(capability)
	client, err := carriercontrol.NewClient(retained.ControlEndpoint, capability, retained.Source.SHA256)
	if err != nil {
		return err
	}
	if err := client.RetireSSH(ctx); errors.Is(err, carriercontrol.ErrBusy) {
		return nil
	} else if err != nil {
		return err
	}
	mutated := false
	defer func() {
		if !mutated {
			resume, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, client.ResumeSSH(resume))
		}
	}()
	abort, err := PrepareSuppliedMaintenance(ctx, layout.HostdSocket, config.TokenFile, false)
	if err != nil {
		return err
	}
	defer abort()
	manager, err := newWindowsLifecycleManager(request, layout, true)
	if err != nil {
		return err
	}
	return manager.Replace(ctx, func(ctx context.Context) error {
		if err := stopAndRemoveWindowsCarrier(ctx, retained); err != nil {
			return err
		}
		mutated = true
		if err := writeWindowsCarrierConfig(desired); err != nil {
			return err
		}
		i, err := carrierInstaller(desired)
		if err != nil {
			return err
		}
		if err := i.Install(ctx); err != nil {
			return err
		}
		if err := waitCarrierReady(ctx, desired); err != nil {
			return err
		}
		if _, err := PrepareWindowsSSHGeneration(ctx, request); err != nil {
			return err
		}
		return nil
	})
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
