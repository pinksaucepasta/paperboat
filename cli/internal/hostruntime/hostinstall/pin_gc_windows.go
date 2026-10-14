//go:build windows

package hostinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/binarytarget"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

var windowsOfficialPinName = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)

// The machine SCM namespace is read once, without growing allocation or paging.
// This matches the existing 256KiB native lifecycle resource bound.
const windowsPinSCMBytes = 256 << 10

// pruneWindowsRuntimePins requires the existing native publication lock and a
// completed lifecycle recovery. Every reference is collected before deletion.
func pruneWindowsRuntimePins(ctx context.Context, installed WindowsRuntimeConfig, carrier CarrierConfig, activation []string) error {
	if ctx == nil || len(activation) > 14 || !validWindowsConfig(installed) || carrier.Validate() != nil {
		return ErrInvalidRequest
	}
	layout, err := service.WindowsUserLayout(installed.OwnerSID)
	if err != nil {
		return err
	}
	if carrier.Instance != layout.Instance || carrier.OwnerSID != installed.OwnerSID {
		return ErrInvalidRequest
	}
	root, _ := WindowsInstanceRoot(layout.Instance)
	// Recover removed this journal under the same lock. A remaining journal is
	// uncertain source ownership, so no pin is eligible for deletion.
	if _, err := os.Lstat(filepath.Join(root, "service-lifecycle", "service-lifecycle.json")); err == nil {
		return service.ErrLifecycleUncertain
	} else if !installNotExistOnly(err) {
		return err
	}
	pins := filepath.Join(layout.ReleasesRoot, "versions")
	keep := make(map[string]bool)
	add := func(path string) error {
		if path == "" {
			return ErrInvalidRequest
		}
		if strings.EqualFold(path, layout.Binary) || strings.EqualFold(path, layout.BinaryRollback) || strings.EqualFold(path, layout.BinaryStaged) {
			return nil
		}
		relative, err := filepath.Rel(pins, path)
		parts := strings.Split(relative, string(filepath.Separator))
		if err != nil || len(parts) != 2 || !validWindowsPinName(parts[0]) || !strings.EqualFold(parts[1], "pb.exe") {
			return ErrInvalidRequest
		}
		keep[strings.ToLower(filepath.Clean(path))] = true
		return nil
	}
	for _, source := range []*installsource.Source{&installed.Source, installed.RollbackSource} {
		if source == nil {
			continue
		}
		path, err := WindowsPinnedSourcePath(layout, *source)
		if err != nil {
			return err
		}
		if err := add(path); err != nil {
			return err
		}
	}
	if err := add(carrier.Executable); err != nil {
		return err
	}
	for _, path := range activation {
		if err := add(path); err != nil {
			return err
		}
	}
	catalogue, _, err := sessionOwnerCatalog(Request{OwnerSID: installed.OwnerSID})
	if err != nil {
		return err
	}
	descriptors, err := catalogue.ReadAll(ctx, layout.Instance)
	if err != nil {
		return err
	}
	names := make(map[string]string)
	for _, kind := range []string{service.HostKind, service.ConfigKind, service.HostdKind, service.DaemonKind, service.UpdaterKind, service.CarrierKind} {
		base := map[string]string{service.HostKind: "PaperboatHost", service.ConfigKind: "PaperboatRuntimeConfig", service.HostdKind: "PaperboatHostd", service.DaemonKind: "PaperboatLocalDaemon", service.UpdaterKind: "PaperboatUpdated", service.CarrierKind: "PaperboatCarrier"}[kind]
		name := base + "-" + layout.Instance
		names[strings.ToLower(name)] = filepath.Join(`C:\ProgramData\Paperboat\services`, name+".json")
	}
	for _, d := range descriptors {
		if d.OwnerSID != installed.OwnerSID || d.RuntimeRoot != installed.StateRoot {
			return ErrInvalidRequest
		}
		expected, err := WindowsPinnedSourcePath(layout, d.Source)
		if err != nil || !strings.EqualFold(expected, d.Executable) {
			return ErrInvalidRequest
		}
		if err := add(d.Executable); err != nil {
			return err
		}
		name, err := service.WindowsSessionOwnerName(layout.Instance, d.Generation)
		if err != nil {
			return err
		}
		names[strings.ToLower(name)] = filepath.Join(`C:\ProgramData\Paperboat\services`, name+".json")
	}
	generations, err := windowsopenssh.ListGenerations(ctx, installed.OwnerSID)
	if err != nil {
		return err
	}
	sshNames := make(map[string]string)
	sshDescriptors := make(map[string]windowsopenssh.GenerationDescriptor)
	for _, g := range generations {
		expected, err := WindowsPinnedSourcePath(layout, g.Source)
		if err != nil || !strings.EqualFold(expected, g.Executable) {
			return ErrInvalidRequest
		}
		if err := add(g.Executable); err != nil {
			return err
		}
		sshNames[strings.ToLower("PaperboatSshd-"+layout.Instance+"-"+g.Generation)] = g.Executable
		sshDescriptors[strings.ToLower("PaperboatSshd-"+layout.Instance+"-"+g.Generation)] = g
	}
	original, err := windowsOpenSSHConfig(layout, installed.OwnerSID)
	if err != nil {
		return err
	}
	originalPath, err := windowsopenssh.OwnedServiceExecutable(original)
	if err == nil {
		if err := add(originalPath); err != nil {
			return err
		}
		sshNames[strings.ToLower(original.ServiceName)] = originalPath
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return err
	}
	if err := collectWindowsDeclaredPinReferences(ctx, layout.Instance, names, add); err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	buffer := make([]byte, windowsPinSCMBytes)
	var needed, count uint32
	if err := windows.EnumServicesStatusEx(manager.Handle, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_STATE_ALL, &buffer[0], uint32(len(buffer)), &needed, &count, nil, nil); err != nil {
		if errors.Is(err, windows.ERROR_MORE_DATA) {
			return fmt.Errorf("pin cleanup skipped: SCM inventory exceeds the bounded 256KiB read: %w", err)
		}
		return err
	}
	if uint64(count)*uint64(unsafe.Sizeof(windows.ENUM_SERVICE_STATUS_PROCESS{})) > uint64(len(buffer)) {
		return ErrInvalidRequest
	}
	entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buffer[0])), int(count))
	owned := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.ToLower(windows.UTF16PtrToString(entry.ServiceName))
		if !strings.HasPrefix(name, "paperboat") || !strings.Contains(name, "-"+layout.Instance) {
			continue
		}
		owned++
		if owned > 2*config.DefaultResources.MaxSessions+12 {
			return ErrInvalidRequest
		}
		if _, ok := sshNames[name]; ok {
			if d, generation := sshDescriptors[name]; generation {
				if err := windowsopenssh.InspectGeneration(ctx, d); err != nil {
					return err
				}
			}
			continue
		}
		definition, ok := names[name]
		if !ok {
			return fmt.Errorf("pin cleanup skipped: an owned native service lacks a protected descriptor: %w", ErrInvalidRequest)
		}
		status, err := (service.WindowsController{}).Inspect(ctx, definition)
		if err != nil || !status.Registered {
			return errors.Join(ErrInvalidRequest, err)
		}
		executable, err := service.OwnedWindowsExecutable(definition)
		if err != nil {
			return err
		}
		if err := add(executable); err != nil {
			return err
		}
	}
	runtime.KeepAlive(buffer)
	return pruneProtectedWindowsPins(ctx, pins, installed.OwnerSID, keep, 2*config.DefaultResources.MaxSessions+32)
}

func validWindowsPinName(name string) bool {
	if windowsOfficialPinName.MatchString(name) {
		return len(name) <= 64
	}
	if !strings.HasPrefix(name, "custom-") {
		return false
	}
	digest := strings.TrimPrefix(name, "custom-")
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == 32 && strings.ToLower(digest) == digest
}

func collectWindowsDeclaredPinReferences(ctx context.Context, instance string, known map[string]string, add func(string) error) error {
	root := `C:\ProgramData\Paperboat\services`
	if err := rejectWindowsReparseAncestors(root); err != nil {
		return err
	}
	owner, err := windowsRuntimeTrustedOwner()
	if err != nil {
		return err
	}
	if err := validateWindowsRuntimeSecurity(root, owner, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", "pin declaration directory"); err != nil {
		return err
	}
	file, err := os.Open(root)
	if err != nil {
		return err
	}
	entries, readErr := file.ReadDir(2*config.DefaultResources.MaxSessions + 33)
	err = errors.Join(file.Close(), readErr)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return err
	}
	if len(entries) > 2*config.DefaultResources.MaxSessions+32 {
		return fmt.Errorf("pin cleanup skipped: native declarations exceed the configured cleanup bound: %w", ErrInvalidRequest)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.ToLower(strings.TrimSuffix(entry.Name(), ".json"))
		if !strings.HasPrefix(name, "paperboat") || !strings.Contains(name, "-"+instance) {
			continue
		}
		path, ok := known[name]
		if !ok || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return ErrInvalidRequest
		}
		if err := validateWindowsRuntimeSecurity(path, owner, "D:P(A;;FA;;;SY)(A;;FA;;;BA)", "pin declaration"); err != nil {
			return err
		}
		executable, err := service.OwnedWindowsExecutable(path)
		if err != nil {
			return err
		}
		if err := add(executable); err != nil {
			return err
		}
	}
	return nil
}

func openWindowsPinObject(path, sid string, directory, remove bool) (windows.Handle, error) {
	access := uint32(windows.GENERIC_READ | windows.READ_CONTROL)
	if remove {
		access |= windows.DELETE
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(path), access, 0, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory || !directory && info.NumberOfLinks != 1 {
		windows.CloseHandle(h)
		return 0, ErrInvalidRequest
	}
	owner, err := windowsRuntimeTrustedOwner()
	if err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	dacl := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;" + sid + ")"
	if directory {
		dacl = windowsRuntimeCurrentRootDACL(sid)
	}
	if err := checkWindowsRuntimeHandleSecurity(h, owner, dacl); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	return h, nil
}

func pruneProtectedWindowsPins(ctx context.Context, root, sid string, keep map[string]bool, maximum int) error {
	if ctx == nil || maximum < 1 || maximum > 544 {
		return ErrInvalidRequest
	}
	if err := rejectWindowsReparseAncestors(root); err != nil {
		return err
	}
	parent, err := openWindowsPinObject(filepath.Dir(root), sid, true, false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	handle, err := openWindowsPinObject(root, sid, true, false)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(handle), root)
	defer directory.Close()
	// The handle remains owned by this function; ReadDir never closes it.
	entries, err := directory.ReadDir(maximum)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		path := filepath.Join(root, name, "pb.exe")
		if !validWindowsPinName(name) || keep[strings.ToLower(path)] {
			continue
		}
		child, err := openWindowsPinObject(filepath.Dir(path), sid, true, true)
		if err != nil {
			continue
		}
		childFile := os.NewFile(uintptr(child), filepath.Dir(path))
		removed, err := pruneProtectedWindowsPin(ctx, childFile, name, sid)
		if err == nil && removed {
			one := byte(1)
			err = windows.SetFileInformationByHandle(child, windows.FileDispositionInfo, &one, 1)
		}
		closeErr := childFile.Close()
		if err != nil {
			return errors.Join(err, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	runtime.KeepAlive(directory)
	return nil
}

func pruneProtectedWindowsPin(ctx context.Context, directory *os.File, name, sid string) (bool, error) {
	path := directory.Name()
	entries, err := directory.ReadDir(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if len(entries) != 1 || !strings.EqualFold(entries[0].Name(), "pb.exe") || entries[0].IsDir() {
		return false, nil
	}
	if err := binarytarget.Validate(filepath.Join(path, "pb.exe"), "windows", runtime.GOARCH); err != nil {
		return false, nil
	}
	if !strings.HasPrefix(name, "custom-") {
		if err := verifyWindowsInstalledBinary(ctx, filepath.Join(path, "pb.exe"), runtime.GOARCH); err != nil {
			return false, nil
		}
	}
	handle, err := openWindowsPinObject(filepath.Join(path, "pb.exe"), sid, false, true)
	if err != nil {
		return false, nil
	}
	file := os.NewFile(uintptr(handle), filepath.Join(path, "pb.exe"))
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() < 1 || info.Size() > 256<<20 {
		return false, nil
	}
	if strings.HasPrefix(name, "custom-") {
		hash := sha256.New()
		length, err := io.Copy(hash, io.LimitReader(file, info.Size()+1))
		if err != nil {
			return false, err
		}
		if length != info.Size() || hex.EncodeToString(hash.Sum(nil)) != strings.TrimPrefix(name, "custom-") {
			return false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	one := byte(1)
	err = windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &one, 1)
	runtime.KeepAlive(file)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return false, nil
	}
	return err == nil, err
}
