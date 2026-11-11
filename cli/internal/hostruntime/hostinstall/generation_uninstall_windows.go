//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// Explicit uninstall owns every generation, including stopped declarations whose
// old executable has disappeared. Removal authenticates declarations and SCM
// targets; it never launches the old image or requires an owner RPC.
// removeWindowsRuntimeGenerations removes the account's per-generation services: session owners, carriers and
// OpenSSH. Without purge the active carrier stays for the lifecycle's own uninstall.
func removeWindowsRuntimeGenerations(ctx context.Context, layout service.Layout, ownerSID string, purge bool) error {
	instance, err := WindowsInstanceForSID(ownerSID)
	if err != nil || instance != layout.Instance {
		return errors.Join(ErrInvalidRequest, err)
	}
	owners, err := windowsGenerationRemovalInventory(ctx, instance)
	if err != nil {
		return err
	}
	keep := ""
	if !purge {
		if active, err := readCarrierConfigFile(ctx, instance); err == nil {
			keep, _ = service.WindowsCarrierName(instance, active.Generation)
		} else if !installNotExistOnly(err) {
			return err
		}
	}
	var result error
	for _, name := range owners {
		if keep != "" && strings.EqualFold(name, keep) {
			continue
		}
		path := filepath.Join(`C:\ProgramData\Paperboat\services`, name+".json")
		if err := (service.WindowsController{}).Remove(ctx, path); err != nil {
			result = errors.Join(result, err)
		} else {
			result = errors.Join(result, removeInstallFile(path))
		}
	}
	if purge {
		// Every carrier generation's descriptor and lock, and the active descriptor; the shared capability goes
		// with the account root.
		for _, pattern := range []string{carrierGenerationPath(instance, "*"), filepath.Join(filepath.Dir(carrierConfigPath(instance)), "owner-*.lock"), carrierConfigPath(instance)} {
			paths, err := filepath.Glob(pattern)
			result = errors.Join(result, err)
			for _, path := range paths {
				result = errors.Join(result, removeInstallFile(path))
			}
		}
	}
	result = errors.Join(result, windowsopenssh.RemoveGenerations(ctx, ownerSID))
	return result
}

// windowsGenerationName is the canonical SCM name of one account-owned generation service (session owner or
// carrier) from its lower-case name, or "" for any other service.
func windowsGenerationName(instance, lower string) (string, error) {
	for _, generational := range []struct {
		prefix string
		name   func(string, string) (string, error)
	}{{"paperboatsessionowner-" + instance + "-", service.WindowsSessionOwnerName}, {"paperboatcarrier-" + instance + "-", service.WindowsCarrierName}} {
		if strings.HasPrefix(lower, generational.prefix) {
			return generational.name(instance, strings.TrimPrefix(lower, generational.prefix))
		}
	}
	return "", nil
}

// Unknown account-owned generation registrations prevent deletion of their
// potentially referenced files. Discovery has the same fixed SCM bound as GC.
func windowsGenerationRemovalInventory(ctx context.Context, instance string) ([]string, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	defer manager.Disconnect()
	buffer := make([]byte, windowsPinSCMBytes)
	var needed, count uint32
	if err := windows.EnumServicesStatusEx(manager.Handle, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_STATE_ALL, &buffer[0], uint32(len(buffer)), &needed, &count, nil, nil); err != nil {
		return nil, err
	}
	if uint64(count)*uint64(unsafe.Sizeof(windows.ENUM_SERVICE_STATUS_PROCESS{})) > uint64(len(buffer)) {
		return nil, ErrInvalidRequest
	}
	entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buffer[0])), int(count))
	owned := 0
	var owners []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		canonical, err := windowsGenerationName(instance, strings.ToLower(windows.UTF16PtrToString(entry.ServiceName)))
		if err != nil {
			return nil, err
		}
		if canonical == "" {
			continue
		}
		owned++
		if owned > 2*config.DefaultResources.MaxSessions+4 {
			return nil, ErrInvalidRequest
		}
		owners = append(owners, canonical)
	}
	runtime.KeepAlive(buffer)
	declarations, err := windowsAccountDeclarationInventory(ctx, instance)
	if err != nil {
		return nil, err
	}
	for _, path := range declarations {
		canonical, err := windowsGenerationName(instance, strings.ToLower(strings.TrimSuffix(filepath.Base(path), ".json")))
		if err != nil {
			return nil, err
		}
		if canonical == "" {
			continue
		}
		found := false
		for _, existing := range owners {
			if strings.EqualFold(existing, canonical) {
				found = true
				break
			}
		}
		if !found {
			owners = append(owners, canonical)
		}
	}
	if len(owners) > 2*config.DefaultResources.MaxSessions+4 {
		return nil, ErrInvalidRequest
	}
	return owners, nil
}
