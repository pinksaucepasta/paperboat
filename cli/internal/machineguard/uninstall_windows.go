//go:build windows

package machineguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/machineloopback"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"golang.org/x/sys/windows"
)

func Uninstall(ctx context.Context) (result UninstallResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if !elevation.IsCurrentProcessElevated() {
		return UninstallResult{}, errors.New("removing machine access requires an elevated administrator")
	}
	if !elevation.IsCurrentProcessElevated() {
		return result, errors.New("removing machine access requires an elevated administrator")
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("machine access removal incomplete; safety state retained; retry elevated pb daemon machine-guard uninstall: %w", err)
		}
	}()
	installed := filepath.Join(os.Getenv("ProgramFiles"), "Paperboat", "MachineGuard", "pb.exe")
	owned, err := ownedScheduledTask(ctx, installed)
	if err != nil {
		return result, err
	}
	if _, e := os.Stat(DefaultStateDir); os.IsNotExist(e) && !owned {
		return result, nil
	}
	lifecycle, lockErr := lockGuardLifecycle(DefaultStateDir)
	if lockErr != nil {
		return UninstallResult{}, lockErr
	}
	defer lifecycle.Close()
	owned, err = ownedScheduledTask(ctx, installed)
	if err != nil {
		return result, err
	}
	if err = protectedDirectory(DefaultStateDir, 0700); err != nil {
		return result, err
	}
	if owned {
		// Disabling first prevents a reboot/restart trigger from restoring access
		// between stop and durable empty WFP projection.
		for _, args := range [][]string{{"/Change", "/TN", windowsGuardTask, "/DISABLE"}, {"/End", "/TN", windowsGuardTask}} {
			output, e := exec.CommandContext(ctx, "schtasks.exe", args...).CombinedOutput()
			if e != nil {
				return result, fmt.Errorf("stop owned machine guard task: %w: %s", e, output)
			}
		}
	}
	var lock interface{ Close() error }
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		lock, err = lockState(filepath.Join(DefaultStateDir, "lock"))
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-deadline.C:
			return result, err
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer lock.Close()
	state, stateErr := loadLoopbackState(DefaultStateDir)
	if stateErr != nil {
		return result, stateErr
	}
	state, stateErr = mergedLoopbackState(state, machineloopback.DefaultCIDR)
	if stateErr != nil {
		return result, stateErr
	}
	if stateErr = writeLoopbackState(DefaultStateDir, state); stateErr != nil {
		return result, stateErr
	}
	cfg := Config{StateDir: DefaultStateDir, LoopbackCIDR: machineloopback.DefaultCIDR, ProtectedLoopbackCIDRs: state.Protected}
	if err = applyProtection(ctx, cfg, nil); err != nil {
		return result, err
	}
	result.Retained = []string{"persistent empty-admission WFP address protection", "address/name reservations", "protected recovery executable"}
	roots, err := uninstallRoots(DefaultStateDir)
	if err != nil {
		return result, err
	}
	if err = cleanupHistoricalLocalNames(ctx, cfg); err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, "machine listeners and owned local name configuration")
	if err = cleanupWindowsOwnedTrust(ctx, roots); err != nil {
		return result, err
	}
	if err = removeBrowserDomainSelection(DefaultStateDir); err != nil {
		return result, err
	}
	result.Removed = append(result.Removed, "owned private HTTPS LocalMachine trust and CA state")
	if owned {
		if output, e := exec.CommandContext(ctx, "schtasks.exe", "/Delete", "/TN", windowsGuardTask, "/F").CombinedOutput(); e != nil {
			return result, fmt.Errorf("remove owned machine guard task: %w: %s", e, output)
		}
	}
	result.Removed = append(result.Removed, "machine access scheduled task")
	return result, nil
}

func cleanupHistoricalTrust(ctx context.Context, cfg Config) error {
	roots, err := uninstallRoots(cfg.StateDir)
	if err != nil {
		return err
	}
	if err := cleanupWindowsOwnedTrust(ctx, roots); err != nil {
		return err
	}
	return removeBrowserDomainSelection(cfg.StateDir)
}

func cleanupWindowsOwnedTrust(ctx context.Context, roots []ownedRoot) error {
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := removeWindowsRoot(root); err != nil {
			return err
		}
	}
	return removeOwnedRootState(roots)
}

func removeWindowsRoot(root ownedRoot) error {
	name, _ := windows.UTF16PtrFromString("ROOT")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM_W, 0, 0, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE|windows.CERT_STORE_OPEN_EXISTING_FLAG, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return err
	}
	defer windows.CertCloseStore(store, 0)
	var previous *windows.CertContext
	for {
		cert, e := windows.CertEnumCertificatesInStore(store, previous)
		if e != nil {
			if errors.Is(e, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return nil
			}
			return e
		}
		if cert == nil {
			return nil
		}
		previous = cert
		if bytes.Equal(unsafe.Slice(cert.EncodedCert, cert.Length), root.certificate.Raw) {
			// Deletion consumes only this exact matching certificate context.
			if e = windows.CertDeleteCertificateFromStore(cert); e != nil {
				return e
			}
			previous = nil
		}
	}
}
