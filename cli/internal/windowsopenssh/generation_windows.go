//go:build windows

package windowsopenssh

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesignature"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const sshGenerationSchema = "paperboat.windows-ssh-generation/v1"

type GenerationRequest struct {
	OwnerSID   string
	Source     installsource.Source
	Executable string
}
type GenerationDescriptor struct {
	Schema     string               `json:"schema"`
	Instance   string               `json:"instance"`
	Generation string               `json:"generation"`
	OwnerSID   string               `json:"owner_sid"`
	Source     installsource.Source `json:"source"`
	Executable string               `json:"executable"`
	Port       uint16               `json:"port"`
}

func validSSHGeneration(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func (d GenerationDescriptor) ServiceName() string {
	return ServiceName + "-" + d.Instance + "-" + d.Generation
}
func (d GenerationDescriptor) Validate() error {
	instance, err := service.WindowsUserInstance(d.OwnerSID)
	if err != nil || d.Schema != sshGenerationSchema || instance != d.Instance || !validSSHGeneration(d.Generation) || d.Source.Validate() != nil || d.Source.Platform != "windows" || d.Source.Architecture != runtime.GOARCH || d.Generation != d.Source.SHA256[:32] || d.Port < 40000 || d.Port >= 60000 {
		return ErrInvalidConfig
	}
	layout, err := service.WindowsUserLayout(d.OwnerSID)
	if err != nil {
		return err
	}
	key := d.Source.Version
	if d.Source.Distribution == installsource.Custom {
		key = "custom-" + d.Source.SHA256
	}
	if !sameCleanPath(d.Executable, filepath.Join(layout.ReleasesRoot, "versions", key, "pb.exe")) {
		return ErrServiceOwnership
	}
	return nil
}
func generationPaths(ownerSID, generation string) (base, root string, err error) {
	instance, err := service.WindowsUserInstance(ownerSID)
	if err != nil || !validSSHGeneration(generation) {
		return "", "", ErrInvalidConfig
	}
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		return "", "", err
	}
	base = filepath.Join(filepath.Dir(layout.UpdateStateRoot), "users", instance, "ssh")
	return base, filepath.Join(base, "generations", generation), nil
}
func generationConfig(d GenerationDescriptor) (Config, Result, error) {
	if err := d.Validate(); err != nil {
		return Config{}, Result{}, err
	}
	_, root, err := generationPaths(d.OwnerSID, d.Generation)
	if err != nil {
		return Config{}, Result{}, err
	}
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return Config{}, Result{}, err
	}
	config := DefaultConfig(nil)
	config.InstallRoot = filepath.Join(programFiles, "OpenSSH")
	config.StateRoot = root
	config.OwnerSID = d.OwnerSID
	config.ServiceName = d.ServiceName()
	config.ServiceSID = ServiceSID(config.ServiceName)
	config.ServiceExecutable = d.Executable
	config.Port = d.Port
	result := resultForConfig(config)
	result.ConfigPath = filepath.Join(root, "sshd_config")
	result.Port = d.Port
	return config, result, nil
}
func generationFileDACL(owner string) string {
	if owner == "S-1-5-18" {
		return "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	}
	return "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + owner + ")"
}
func generationDirectoryDACL(owner string) string {
	if owner == "S-1-5-18" {
		return "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	}
	return "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;" + owner + ")"
}
func checkGenerationPath(path, dacl string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.IsDir() != directory || !directory && !info.Mode().IsRegular() || !windowssecurity.OwnerMatchesSID(path, system) || !windowssecurity.ProtectedDACLMatches(path, dacl) {
		return ErrServiceOwnership
	}
	return nil
}
func prepareGenerationDirectory(path, owner string) error {
	if _, err := os.Lstat(path); err == nil {
		return checkGenerationPath(path, generationDirectoryDACL(owner), true)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:SY" + generationDirectoryDACL(owner))
	if err != nil {
		return err
	}
	system, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	err = windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, system, nil, dacl, nil)
	})
	if err != nil {
		return err
	}
	return checkGenerationPath(path, generationDirectoryDACL(owner), true)
}
func verifyGenerationExecutable(ctx context.Context, d GenerationDescriptor, signature bool) error {
	if err := d.Validate(); err != nil {
		return err
	}
	info, err := os.Lstat(d.Executable)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrUntrustedBinary
	}
	allowed := []string{"D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;" + d.OwnerSID + ")", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)"}
	protected := false
	for _, acl := range allowed {
		if checkGenerationPath(d.Executable, acl, false) == nil {
			protected = true
			break
		}
	}
	if !protected {
		return ErrUntrustedBinary
	}
	if err := d.Source.Verify(d.Executable); err != nil {
		return err
	}
	if signature && d.Source.Distribution == installsource.Official {
		verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return nativesignature.New(nil).Verify(verifyCtx, d.Executable, "windows", d.Source.Architecture)
	}
	return nil
}
func LoadGeneration(ctx context.Context, ownerSID, generation string) (GenerationDescriptor, error) {
	d, err := loadGenerationDescriptor(ctx, ownerSID, generation)
	if err != nil {
		return d, err
	}
	if err := verifyGenerationExecutable(ctx, d, false); err != nil {
		return GenerationDescriptor{}, err
	}
	return d, nil
}
func loadGenerationDescriptor(ctx context.Context, ownerSID, generation string) (GenerationDescriptor, error) {
	if ctx == nil {
		return GenerationDescriptor{}, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return GenerationDescriptor{}, err
	}
	_, root, err := generationPaths(ownerSID, generation)
	if err != nil {
		return GenerationDescriptor{}, err
	}
	if err := checkGenerationPath(filepath.Dir(root), generationDirectoryDACL(ownerSID), true); err != nil {
		return GenerationDescriptor{}, err
	}
	if err := checkGenerationPath(root, generationDirectoryDACL(ownerSID), true); err != nil {
		return GenerationDescriptor{}, err
	}
	path := filepath.Join(root, "descriptor.json")
	if err := checkGenerationPath(path, generationFileDACL(ownerSID), false); err != nil {
		return GenerationDescriptor{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return GenerationDescriptor{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	var d GenerationDescriptor
	var extra any
	if decoder.Decode(&d) != nil || decoder.Decode(&extra) != io.EOF || d.OwnerSID != ownerSID || d.Generation != generation || d.Validate() != nil {
		return GenerationDescriptor{}, ErrInvalidConfig
	}
	if err := checkGenerationPath(filepath.Join(root, "sshd_config"), generationFileDACL(ownerSID), false); err != nil {
		return GenerationDescriptor{}, err
	}
	return d, nil
}
func ListGenerations(ctx context.Context, ownerSID string) ([]GenerationDescriptor, error) {
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	_, root, err := generationPaths(ownerSID, strings.Repeat("0", 32))
	if err != nil {
		return nil, err
	}
	root = filepath.Dir(root)
	if err := checkGenerationPath(root, generationDirectoryDACL(ownerSID), true); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	directory, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	maximum := runtimeconfig.DefaultResources.MaxSessions + 2
	entries, err := directory.ReadDir(maximum + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maximum {
		return nil, ErrServiceOwnership
	}
	result := make([]GenerationDescriptor, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !validSSHGeneration(entry.Name()) {
			return nil, ErrServiceOwnership
		}
		d, err := loadGenerationDescriptor(ctx, ownerSID, entry.Name())
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, nil
}
func generationArguments(d GenerationDescriptor) []string {
	return []string{"daemon", "__runtime-ssh", "--instance", d.Instance, "--generation", d.Generation}
}
func generationServiceMatches(configuration mgr.Config, d GenerationDescriptor) bool {
	args, err := windows.DecomposeCommandLine(configuration.BinaryPathName)
	expected := append([]string{d.Executable}, generationArguments(d)...)
	if err != nil || len(args) != len(expected) || !sameCleanPath(args[0], expected[0]) || !strings.EqualFold(configuration.ServiceStartName, "LocalSystem") || configuration.StartType != mgr.StartAutomatic || configuration.ErrorControl != mgr.ErrorNormal || configuration.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED {
		return false
	}
	for i := 1; i < len(args); i++ {
		if args[i] != expected[i] {
			return false
		}
	}
	return true
}
func installGenerationService(ctx context.Context, d GenerationDescriptor) error {
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(d.ServiceName())
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		item, err = manager.CreateService(d.ServiceName(), d.Executable, mgr.Config{DisplayName: "Paperboat OpenSSH generation", Description: "Loopback-only managed SSH generation", StartType: mgr.StartAutomatic, ErrorControl: mgr.ErrorNormal, ServiceStartName: "LocalSystem", SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED}, generationArguments(d)...)
	}
	if err != nil {
		return err
	}
	defer item.Close()
	configuration, err := item.Config()
	if err != nil {
		return err
	}
	if !generationServiceMatches(configuration, d) {
		return ErrServiceOwnership
	}
	if err := grantServiceOwnerQuery(item.Handle, d.ServiceName(), d.OwnerSID); err != nil {
		return err
	}
	if err := item.SetRecoveryActions(paperboatServiceRecovery, 24*60*60); err != nil {
		return err
	}
	if err := item.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return err
	}
	if err := item.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	return waitForServiceState(ctx, item, svc.Running, 30*time.Second)
}
func allocateGenerationPort(ctx context.Context, ownerSID, generation string) (uint16, error) {
	digest := sha256.Sum256([]byte(ownerSID + generation))
	first := uint32(40000) + uint32(binary.BigEndian.Uint16(digest[:2]))%20000
	for offset := 0; offset < runtimeconfig.DefaultResources.MaxSessions+2; offset++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		port := uint16(40000 + (first-40000+uint32(offset))%20000)
		v4, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
		if err != nil {
			continue
		}
		v6, err := net.ListenTCP("tcp6", &net.TCPAddr{IP: net.IPv6loopback, Port: int(port)})
		v4.Close()
		if err != nil {
			continue
		}
		v6.Close()
		return port, nil
	}
	return 0, ErrServiceUnhealthy
}

// PrepareGeneration returns only after protected SCM/listener custody proves
// readiness. Callers keep the previous target until this function succeeds.
// It never restarts another generation or the original single-wrapper service.
func PrepareGeneration(ctx context.Context, request GenerationRequest) (result GenerationDescriptor, resultErr error) {
	if ctx == nil || request.Source.Validate() != nil {
		return GenerationDescriptor{}, ErrInvalidConfig
	}
	// Existing verification/configuration/start/readiness deadlines compose to
	// at most two minutes; the caller can impose a shorter activation budget.
	bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ctx = bounded
	instance, err := service.WindowsUserInstance(request.OwnerSID)
	if err != nil {
		return GenerationDescriptor{}, err
	}
	d := GenerationDescriptor{Schema: sshGenerationSchema, Instance: instance, Generation: request.Source.SHA256[:32], OwnerSID: request.OwnerSID, Source: request.Source, Executable: request.Executable, Port: 40000}
	if err := verifyGenerationExecutable(ctx, d, true); err != nil {
		return GenerationDescriptor{}, err
	}
	fresh := false
	defer func() {
		if resultErr != nil && fresh {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			cleanupErr := RetireGeneration(cleanup, d)
			if errors.Is(cleanupErr, os.ErrNotExist) {
				manager, managerErr := mgr.Connect()
				if managerErr != nil {
					cleanupErr = managerErr
				} else {
					item, openErr := manager.OpenService(d.ServiceName())
					if openErr == nil {
						item.Close()
					}
					manager.Disconnect()
					if errors.Is(openErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
						_, root, pathErr := generationPaths(d.OwnerSID, d.Generation)
						if pathErr != nil {
							cleanupErr = pathErr
						} else {
							cleanupErr = removeGenerationStorage(root, d.OwnerSID)
						}
					} else {
						cleanupErr = ErrServiceOwnership
					}
				}
			}
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()
	existing, err := loadGenerationDescriptor(ctx, d.OwnerSID, d.Generation)
	if err == nil {
		if !existing.Source.SameArtifact(d.Source) {
			return GenerationDescriptor{}, ErrServiceOwnership
		}
		d = existing
	} else if !errors.Is(err, os.ErrNotExist) {
		return GenerationDescriptor{}, err
	} else {
		_, pendingRoot, pathErr := generationPaths(d.OwnerSID, d.Generation)
		if pathErr != nil {
			return GenerationDescriptor{}, pathErr
		}
		if _, statErr := os.Lstat(pendingRoot); statErr == nil {
			manager, managerErr := mgr.Connect()
			if managerErr != nil {
				return GenerationDescriptor{}, managerErr
			}
			item, openErr := manager.OpenService(d.ServiceName())
			if openErr == nil {
				item.Close()
			}
			manager.Disconnect()
			if !errors.Is(openErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
				return GenerationDescriptor{}, ErrServiceOwnership
			}
			if err := removeGenerationStorage(pendingRoot, d.OwnerSID); err != nil {
				return GenerationDescriptor{}, err
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return GenerationDescriptor{}, statErr
		}
		all, err := ListGenerations(ctx, d.OwnerSID)
		if err != nil {
			return GenerationDescriptor{}, err
		}
		if len(all) >= runtimeconfig.DefaultResources.MaxSessions+2 {
			return GenerationDescriptor{}, ErrServiceOwnership
		}
		base, root, err := generationPaths(d.OwnerSID, d.Generation)
		if err != nil {
			return GenerationDescriptor{}, err
		}
		// The installed instance is the trusted parent; reject linked SSH state.
		parent := filepath.Dir(base)
		if err := checkGenerationPath(parent, generationDirectoryDACL(d.OwnerSID), true); err != nil {
			return GenerationDescriptor{}, err
		}
		info, err := os.Lstat(base)
		if err != nil || !info.IsDir() {
			return GenerationDescriptor{}, ErrServiceOwnership
		}
		attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(base))
		if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return GenerationDescriptor{}, ErrServiceOwnership
		}
		if err := prepareGenerationDirectory(filepath.Dir(root), d.OwnerSID); err != nil {
			return GenerationDescriptor{}, err
		}
		fresh = true
		if err := prepareGenerationDirectory(root, d.OwnerSID); err != nil {
			return GenerationDescriptor{}, err
		}
		if err := prepareGenerationDirectory(filepath.Join(root, "logs"), d.OwnerSID); err != nil {
			return GenerationDescriptor{}, err
		}
		d.Port, err = allocateGenerationPort(ctx, d.OwnerSID, d.Generation)
		if err != nil {
			return GenerationDescriptor{}, err
		}
		config, result, err := generationConfig(d)
		if err != nil {
			return GenerationDescriptor{}, err
		}
		if err := verifyBinary(ctx, config, result.SSHDPath); err != nil {
			return GenerationDescriptor{}, err
		}
		if err := verifyBinary(ctx, config, result.SFTPPath); err != nil {
			return GenerationDescriptor{}, err
		}
		hostkey := filepath.Join(base, "hostkeys", "ssh_host_ed25519_key")
		if err := verifyHostKeyFiles(hostkey); err != nil {
			return GenerationDescriptor{}, err
		}
		path, err := WriteServiceConfig(ServiceConfig{StateRoot: root, HostKey: hostkey, SSHDPath: result.SSHDPath, SFTPPath: result.SFTPPath, AuthorizedKeys: filepath.Join(base, "authorized_keys", "paperboat"), Port: d.Port})
		if err != nil {
			return GenerationDescriptor{}, err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return GenerationDescriptor{}, err
		}
		if err := atomicfile.Write(path, body, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SY" + generationFileDACL(d.OwnerSID)}); err != nil {
			return GenerationDescriptor{}, err
		}
		if err := ValidateServiceConfig(config.Runner, result.SSHDPath, path); err != nil {
			return GenerationDescriptor{}, err
		}
		body, err = json.Marshal(d)
		if err != nil {
			return GenerationDescriptor{}, err
		}
		if err := atomicfile.Write(filepath.Join(root, "descriptor.json"), body, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SY" + generationFileDACL(d.OwnerSID)}); err != nil {
			return GenerationDescriptor{}, err
		}
	}
	if err := installGenerationService(ctx, d); err != nil {
		return GenerationDescriptor{}, err
	}
	ready, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := InspectGeneration(ready, d); err == nil {
			return d, nil
		}
		select {
		case <-ready.Done():
			return GenerationDescriptor{}, ErrServiceUnhealthy
		case <-tick.C:
		}
	}
}

func InspectGeneration(ctx context.Context, d GenerationDescriptor) error {
	loaded, err := loadGenerationDescriptor(ctx, d.OwnerSID, d.Generation)
	if err != nil {
		return err
	}
	if loaded != d {
		return ErrServiceOwnership
	}
	config, result, err := generationConfig(d)
	if err != nil {
		return err
	}
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(manager)
	name, err := windows.UTF16PtrFromString(d.ServiceName())
	if err != nil {
		return err
	}
	handle, err := windows.OpenService(manager, name, windows.SERVICE_QUERY_CONFIG|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	item := mgr.Service{Name: d.ServiceName(), Handle: handle}
	defer item.Close()
	configuration, err := item.Config()
	if err != nil || !generationServiceMatches(configuration, d) {
		return ErrServiceOwnership
	}
	health, err := collectLoopbackHealth(ctx, config, result)
	if err != nil {
		return err
	}
	return ValidateLoopbackHealth(health, config, result)
}

// RunGenerationServiceForInstance accepts protected generation identifiers only. SCM
// dispatch and executable/source identity are verified before job creation.
func RunGenerationServiceForInstance(ctx context.Context, instance, generation string) error {
	d, err := LoadGenerationForInstance(ctx, instance, generation)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil || !sameCleanPath(executable, d.Executable) {
		return ErrServiceOwnership
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	item, err := manager.OpenService(d.ServiceName())
	if err != nil {
		manager.Disconnect()
		return err
	}
	configuration, queryErr := item.Config()
	closeErr := item.Close()
	disconnectErr := manager.Disconnect()
	if queryErr != nil || !generationServiceMatches(configuration, d) {
		return errors.Join(ErrServiceOwnership, queryErr, closeErr, disconnectErr)
	}
	if err := errors.Join(closeErr, disconnectErr); err != nil {
		return err
	}
	_, result, err := generationConfig(d)
	if err != nil {
		return err
	}
	return RunServiceHost(d.ServiceName(), result.SSHDPath, result.ConfigPath, d.OwnerSID)
}

// RetireGeneration requires the carrier to fence admission and prove zero
// accepted bridges for this exact generation before calling. Stop/delete only
// this owned SCM registration; never touch the original or another generation.
func RetireGeneration(ctx context.Context, d GenerationDescriptor) error {
	loaded, err := loadGenerationDescriptor(ctx, d.OwnerSID, d.Generation)
	if err != nil {
		return err
	}
	if loaded != d {
		return ErrServiceOwnership
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(d.ServiceName())
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		_, root, pathErr := generationPaths(d.OwnerSID, d.Generation)
		if pathErr != nil {
			return pathErr
		}
		return removeGenerationStorage(root, d.OwnerSID)
	}
	if err != nil {
		return err
	}
	defer func() {
		if item.Handle != 0 {
			item.Close()
		}
	}()
	configuration, err := item.Config()
	if err != nil || !generationServiceMatches(configuration, d) {
		return ErrServiceOwnership
	}
	status, err := item.Query()
	if err != nil {
		return err
	}
	if status.State != svc.Stopped {
		if _, err := item.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
		if err := waitForServiceState(ctx, item, svc.Stopped, 30*time.Second); err != nil {
			return err
		}
	}
	if err := item.Delete(); err != nil {
		return err
	}
	if err := item.Close(); err != nil {
		return err
	}
	item.Handle = 0
	_, root, pathErr := generationPaths(d.OwnerSID, d.Generation)
	if pathErr != nil {
		return pathErr
	}
	return removeGenerationStorage(root, d.OwnerSID)
}

func sameGenerationServiceCommand(command, name, executable string) bool {
	suffix := strings.TrimPrefix(name, ServiceName+"-")
	parts := strings.Split(suffix, "-")
	if len(parts) != 2 || len(parts[0]) != 25 || parts[0][0] != 'u' || !validSSHGeneration(parts[1]) {
		return false
	}
	args, err := windows.DecomposeCommandLine(command)
	expected := []string{executable, "daemon", "__runtime-ssh", "--instance", parts[0], "--generation", parts[1]}
	if err != nil || len(args) != len(expected) || !sameCleanPath(args[0], expected[0]) {
		return false
	}
	for i := 1; i < len(args); i++ {
		if args[i] != expected[i] {
			return false
		}
	}
	return true
}

// Remove only the protected files created for this native generation. An
// unexpected neighbour is an error and remains untouched.
func removeGenerationStorage(root, ownerSID string) error {
	return generationStorage(root, ownerSID, true)
}
func generationStorage(root, ownerSID string, remove bool) error {
	if err := checkGenerationPath(root, generationDirectoryDACL(ownerSID), true); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	directory, err := os.Open(root)
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(4)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) > 3 {
		return ErrServiceOwnership
	}
	files := []string{}
	logs := ""
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		switch entry.Name() {
		case "descriptor.json", "sshd_config":
			if err := checkGenerationPath(path, generationFileDACL(ownerSID), false); err != nil {
				return err
			}
			files = append(files, path)
		case "logs":
			if err := checkGenerationPath(path, generationDirectoryDACL(ownerSID), true); err != nil {
				return err
			}
			logs = path
		default:
			return ErrServiceOwnership
		}
	}
	if logs != "" {
		directory, err := os.Open(logs)
		if err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(2)
		closeErr := directory.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(entries) > 1 {
			return ErrServiceOwnership
		}
		for _, entry := range entries {
			if entry.Name() != "service.log" {
				return ErrServiceOwnership
			}
			path := filepath.Join(logs, entry.Name())
			if err := checkGenerationPath(path, generationFileDACL(ownerSID), false); err != nil {
				return err
			}
			files = append(files, path)
		}
	}
	if !remove {
		return nil
	}
	for _, path := range files {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if logs != "" {
		if err := os.Remove(logs); err != nil {
			return err
		}
	}
	return os.Remove(root)
}
