//go:build windows

package wfp

import (
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This fixture owns the fixed historical key only when it is absent. It must
// never replace an installed provider to manufacture migration evidence.
func TestLegacyProviderCleanupPreservesForeignCustodyAndDependencies(t *testing.T) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if !current.User.Sid.Equals(system) {
		t.Skip("requires native SYSTEM custody fixture")
	}
	var engine uintptr
	if err := fwpmEngineOpen0(nil, cRPC_C_AUTHN_WINNT, nil, nil, unsafe.Pointer(&engine)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fwpmEngineClose0(engine); err != nil {
			t.Error(err)
		}
	})
	exists := func(key windows.GUID) bool {
		t.Helper()
		var p *wtFwpmProvider0
		r, _, _ := providerGetProc.Call(engine, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&p)))
		if p != nil {
			fwpmFreeMemory0(unsafe.Pointer(&p))
		}
		if r == 0 {
			return true
		}
		if !providerNotFound(r) {
			t.Fatal(windows.Errno(r))
		}
		return false
	}
	if exists(legacyProviderKey) {
		t.Skip("authentic historical provider retained for public uninstall")
	}
	display, err := createWtFwpmDisplayData0("Paperboat device guard", "Persistent protected device-name policy")
	if err != nil {
		t.Fatal(err)
	}
	add := func(key windows.GUID, description string, sd uintptr) {
		t.Helper()
		d, err := createWtFwpmDisplayData0("Paperboat device guard", description)
		if err != nil {
			t.Fatal(err)
		}
		p := wtFwpmProvider0{providerKey: key, displayData: *d, flags: 1}
		if err := fwpmProviderAdd0(engine, &p, sd); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			r, _, _ := providerDeleteProc.Call(engine, uintptr(unsafe.Pointer(&key)))
			if r != 0 && !providerNotFound(r) {
				t.Error(windows.Errno(r))
			}
		})
	}
	retire := func() error { return runTransaction(engine, func(e uintptr) error { return removeLegacyProvider(e) }) }
	add(legacyProviderKey, "Foreign policy", 0)
	if err := retire(); err == nil || !exists(legacyProviderKey) {
		t.Fatalf("wrong metadata was not preserved: %v", err)
	}
	r, _, _ := providerDeleteProc.Call(engine, uintptr(unsafe.Pointer(&legacyProviderKey)))
	if r != 0 {
		t.Fatal(windows.Errno(r))
	}
	unsafeSD, err := windows.SecurityDescriptorFromString("O:SYD:P(A;;GA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	add(legacyProviderKey, "Persistent protected device-name policy", uintptr(unsafe.Pointer(unsafeSD)))
	runtime.KeepAlive(unsafeSD)
	if err := retire(); err == nil || !exists(legacyProviderKey) {
		t.Fatalf("foreign mutation grant was not preserved: %v", err)
	}
	r, _, _ = providerDeleteProc.Call(engine, uintptr(unsafe.Pointer(&legacyProviderKey)))
	if r != 0 {
		t.Fatal(windows.Errno(r))
	}
	add(legacyProviderKey, "Persistent protected device-name policy", 0)
	foreignKey, err := windows.GenerateGUID()
	if err != nil {
		t.Fatal(err)
	}
	add(foreignKey, "Persistent protected device-name policy", 0)
	dependencyKey, err := windows.GenerateGUID()
	if err != nil {
		t.Fatal(err)
	}
	dependent := wtFwpmSublayer0{subLayerKey: dependencyKey, displayData: *display, providerKey: &legacyProviderKey, weight: 1}
	if err := fwpmSubLayerAdd0(engine, &dependent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r, _, _ := sublayerDeleteProc.Call(engine, uintptr(unsafe.Pointer(&dependencyKey)))
		if r != 0 && !notFound(r) {
			t.Error(windows.Errno(r))
		}
	})
	if err := ensureBase(engine); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r, _, _ := sublayerDeleteProc.Call(engine, uintptr(unsafe.Pointer(&sublayerKey)))
		if r != 0 && !notFound(r) {
			t.Error(windows.Errno(r))
		}
	})
	if err := Remove(t.Context()); err == nil {
		t.Fatal("provider dependency did not abort cleanup")
	}
	if !exists(legacyProviderKey) || !exists(foreignKey) {
		t.Fatal("failed cleanup lost provider state")
	}
	var sub *wtFwpmSublayer0
	getSub := windows.NewLazySystemDLL("fwpuclnt.dll").NewProc("FwpmSubLayerGetByKey0")
	r, _, _ = getSub.Call(engine, uintptr(unsafe.Pointer(&sublayerKey)), uintptr(unsafe.Pointer(&sub)))
	if sub != nil {
		fwpmFreeMemory0(unsafe.Pointer(&sub))
	}
	if r != 0 {
		t.Fatalf("failed provider retirement did not roll back owned sublayer: %v", windows.Errno(r))
	}
	r, _, _ = sublayerDeleteProc.Call(engine, uintptr(unsafe.Pointer(&dependencyKey)))
	if r != 0 {
		t.Fatal(windows.Errno(r))
	}
	if err := Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	if exists(legacyProviderKey) || !exists(foreignKey) {
		t.Fatal("owned provider retained or same-name foreign provider removed")
	}
	if err := Remove(t.Context()); err != nil {
		t.Fatalf("idempotent cleanup: %v", err)
	}
}
