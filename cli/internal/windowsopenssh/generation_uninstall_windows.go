//go:build windows

package windowsopenssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// RemoveGenerations is explicit uninstall, not empty-flow retirement. The caller
// holds the installation publication lock and has authorized ending its work.
// A protected native registration can survive deletion of its JSON descriptor;
// it is sufficient only to remove that exact canonical account-owned role.
func RemoveGenerations(ctx context.Context, ownerSID string) (resultErr error) {
	instance, err := service.WindowsUserInstance(ownerSID)
	if err != nil {
		return err
	}
	_, sample, err := generationPaths(ownerSID, strings.Repeat("0", 32))
	if err != nil {
		return err
	}
	root := filepath.Dir(sample)
	generations := map[string]bool{}
	if err := checkGenerationPath(root, generationDirectoryDACL(ownerSID), true); err == nil {
		directory, err := os.Open(root)
		if err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(runtimeconfig.DefaultResources.MaxSessions + 3)
		closeErr := directory.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(entries) > runtimeconfig.DefaultResources.MaxSessions+2 {
			return ErrServiceOwnership
		}
		for _, entry := range entries {
			if !entry.IsDir() || !validSSHGeneration(entry.Name()) {
				return ErrServiceOwnership
			}
			generations[entry.Name()] = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, manager.Disconnect()) }()
	buffer := make([]byte, 256<<10)
	var needed, count uint32
	if err := windows.EnumServicesStatusEx(manager.Handle, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_STATE_ALL, &buffer[0], uint32(len(buffer)), &needed, &count, nil, nil); err != nil {
		return err
	}
	if uint64(count)*uint64(unsafe.Sizeof(windows.ENUM_SERVICE_STATUS_PROCESS{})) > uint64(len(buffer)) {
		return ErrServiceOwnership
	}
	prefix := ServiceName + "-" + instance + "-"
	for _, entry := range unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buffer[0])), int(count)) {
		name := windows.UTF16PtrToString(entry.ServiceName)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		generation := strings.TrimPrefix(name, prefix)
		if !validSSHGeneration(generation) {
			return ErrServiceOwnership
		}
		generations[generation] = true
	}
	runtime.KeepAlive(buffer)
	if len(generations) > runtimeconfig.DefaultResources.MaxSessions+2 {
		return ErrServiceOwnership
	}
	type owned struct {
		item       *mgr.Service
		generation string
		root       string
	}
	var roles []owned
	defer func() {
		for _, role := range roles {
			if role.item != nil && role.item.Handle != 0 {
				resultErr = errors.Join(resultErr, role.item.Close())
			}
		}
	}()
	// Validate the entire union before mutating any role.
	for generation := range generations {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, path, err := generationPaths(ownerSID, generation)
		if err != nil {
			return err
		}
		if err := generationStorage(path, ownerSID, false); err != nil {
			return err
		}
		name := prefix + generation
		item, err := manager.OpenService(name)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			roles = append(roles, owned{generation: generation, root: path})
			continue
		}
		if err != nil {
			return err
		}
		roles = append(roles, owned{item: item, generation: generation, root: path})
		if err := generationRemovalRegistration(ctx, item, ownerSID, instance, generation, path); err != nil {
			return err
		}

	}
	for _, role := range roles {
		if role.item != nil {
			status, err := role.item.Query()
			if err != nil {
				return err
			}
			if status.State != svc.Stopped {
				if _, err := role.item.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
					return err
				}
				if err := waitForServiceState(ctx, role.item, svc.Stopped, 30*time.Second); err != nil {
					return err
				}
			}
			if err := role.item.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
				return err
			}
			if err := role.item.Close(); err != nil {
				return err
			}
			role.item.Handle = 0
			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			for {
				item, err := manager.OpenService(prefix + role.generation)
				if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
					break
				}
				if item != nil {
					item.Close()
				}
				if err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-deadline.C:
					return context.DeadlineExceeded
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		if err := removeGenerationStorage(role.root, ownerSID); err != nil {
			return err
		}
	}
	return nil
}

func generationRemovalPin(ownerSID, instance, generation, path string) error {
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		return err
	}
	account := filepath.Join(layout.InstallRoot, "users", instance)
	relative, err := filepath.Rel(filepath.Join(account, "releases", "versions"), filepath.Clean(path))
	if err != nil {
		return err
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || parts[1] != "pb.exe" || !strings.HasPrefix(parts[0], "custom-") {
		return ErrServiceOwnership
	}
	digest := strings.TrimPrefix(parts[0], "custom-")
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || digest != strings.ToLower(digest) || !strings.HasPrefix(digest, generation) {
		return ErrServiceOwnership
	}
	// Only existing protected ancestors are needed for removal of an absent image.
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if info, err := os.Lstat(current); err == nil {
			attr, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(current))
			if err != nil {
				return err
			}
			if attr&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || current != filepath.Clean(path) && !info.IsDir() {
				return ErrServiceOwnership
			}
			sd, err := windows.GetNamedSecurityInfo(current, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				return err
			}
			if err := generationRemovalCustody(sd, false); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if strings.EqualFold(current, account) {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ErrServiceOwnership
		}
	}
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(ptr, windows.GENERIC_READ|windows.READ_CONTROL, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.NumberOfLinks != 1 || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return ErrServiceOwnership
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if err := generationRemovalCustody(sd, false); err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, (256<<20)+1))
	if err != nil {
		return err
	}
	if n > 256<<20 || hex.EncodeToString(hash.Sum(nil)) != digest {
		return ErrServiceOwnership
	}

	return nil
}

func generationRemovalServiceCustody(handle windows.Handle) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_SERVICE, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return generationRemovalCustody(sd, true)
}
func generationRemovalCustody(sd *windows.SECURITY_DESCRIPTOR, native bool) error {
	if sd == nil {
		return ErrServiceOwnership
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return errors.Join(ErrServiceOwnership, err)
	}
	if owner == nil || owner.String() != "S-1-5-18" && owner.String() != "S-1-5-32-544" {
		return ErrServiceOwnership
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return errors.Join(ErrServiceOwnership, err)
	}
	if acl == nil {
		return ErrServiceOwnership
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return errors.Join(ErrServiceOwnership, err)
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return ErrServiceOwnership
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return ErrServiceOwnership
		}
		if sid.String() == "S-1-5-18" || sid.String() == "S-1-5-32-544" || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if native {
			if ace.Mask&(windows.SERVICE_CHANGE_CONFIG|windows.DELETE|windows.WRITE_DAC|windows.WRITE_OWNER|windows.GENERIC_WRITE|windows.GENERIC_ALL) != 0 {
				return ErrServiceOwnership
			}
		} else if uint32(ace.Mask)&^uint32(windows.FILE_GENERIC_READ|windows.FILE_GENERIC_EXECUTE) != 0 {
			return ErrServiceOwnership
		}
	}
	return nil
}

func generationRemovalRegistration(ctx context.Context, item *mgr.Service, ownerSID, instance, generation, path string) error {
	cfg, err := item.Config()
	if err != nil {
		return err
	}
	descriptor, descriptorErr := loadGenerationDescriptor(ctx, ownerSID, generation)
	if descriptorErr == nil {
		if !generationServiceMatches(cfg, descriptor) {
			return ErrServiceOwnership
		}
	} else {
		// Only absent metadata permits native-only removal. Malformed present JSON
		// and custody failures are never treated as absence.
		if _, err := os.Lstat(filepath.Join(path, "descriptor.json")); !errors.Is(err, os.ErrNotExist) {
			return descriptorErr
		}
		args, err := windows.DecomposeCommandLine(cfg.BinaryPathName)
		if err != nil || len(args) != 7 || !sameGenerationServiceCommand(cfg.BinaryPathName, ServiceName+"-"+instance+"-"+generation, args[0]) || !strings.EqualFold(cfg.ServiceStartName, "LocalSystem") || cfg.StartType != mgr.StartAutomatic || cfg.ErrorControl != mgr.ErrorNormal || cfg.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED {
			return ErrServiceOwnership
		}
		if err := generationRemovalPin(ownerSID, instance, generation, args[0]); err != nil {
			return err
		}
	}
	if err := generationRemovalServiceCustody(item.Handle); err != nil {
		return err
	}
	return nil
}
