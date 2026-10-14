//go:build windows

package wfp

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var legacyProviderKey = windows.GUID{Data1: 0x70617065, Data2: 0x7262, Data3: 0x6f61, Data4: [8]byte{0x74, 0x2d, 0x77, 0x66, 0x70, 0x2d, 0x30, 0x31}}
var providerGetProc = windows.NewLazySystemDLL("fwpuclnt.dll").NewProc("FwpmProviderGetByKey0")
var providerDeleteProc = windows.NewLazySystemDLL("fwpuclnt.dll").NewProc("FwpmProviderDeleteByKey0")
var providerSecurityProc = windows.NewLazySystemDLL("fwpuclnt.dll").NewProc("FwpmProviderGetSecurityInfoByKey0")

// A dependency or ownership mismatch aborts the caller's transaction.
func removeLegacyProvider(engine uintptr) error {
	var provider *wtFwpmProvider0
	result, _, _ := providerGetProc.Call(engine, uintptr(unsafe.Pointer(&legacyProviderKey)), uintptr(unsafe.Pointer(&provider)))
	if provider != nil {
		defer fwpmFreeMemory0(unsafe.Pointer(&provider))
	}
	if providerNotFound(result) {
		return nil
	}
	if result != 0 {
		return windows.Errno(result)
	}
	if provider == nil || provider.providerKey != legacyProviderKey || provider.displayData.name == nil || provider.displayData.description == nil || windows.UTF16PtrToString(provider.displayData.name) != "Paperboat device guard" || windows.UTF16PtrToString(provider.displayData.description) != "Persistent protected device-name policy" || provider.flags != 1 || provider.providerData.size != 0 || provider.providerData.data != nil || provider.serviceName != nil {
		return errors.New("historical Paperboat WFP provider identity mismatch; provider retained")
	}
	if err := validateLegacyProviderCustody(engine); err != nil {
		return fmt.Errorf("historical Paperboat WFP provider custody: %w", err)
	}
	result, _, _ = providerDeleteProc.Call(engine, uintptr(unsafe.Pointer(&legacyProviderKey)))
	if result != 0 && !providerNotFound(result) {
		return fmt.Errorf("remove historical Paperboat WFP provider: %w", windows.Errno(result))
	}
	return nil
}

func providerSecurity(engine uintptr, key *windows.GUID) (*windows.SECURITY_DESCRIPTOR, error) {
	var descriptor *windows.SECURITY_DESCRIPTOR
	result, _, _ := providerSecurityProc.Call(engine, uintptr(unsafe.Pointer(key)), uintptr(windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION), 0, 0, 0, 0, uintptr(unsafe.Pointer(&descriptor)))
	if result != 0 {
		if descriptor != nil {
			fwpmFreeMemory0(unsafe.Pointer(&descriptor))
		}
		return nil, windows.Errno(result)
	}
	if descriptor == nil || !descriptor.IsValid() {
		if descriptor != nil {
			fwpmFreeMemory0(unsafe.Pointer(&descriptor))
		}
		return nil, errors.New("invalid WFP security descriptor")
	}
	return descriptor, nil
}

// The native default has fewer than 32 ACEs. This permits substantial OS
// expansion while bounding the two ACL inventories and their comparison.
const maximumProviderCustodyACEs = 128

func validateLegacyProviderCustody(engine uintptr) error {
	descriptor, err := providerSecurity(engine, &legacyProviderKey)
	if err != nil {
		return err
	}
	defer fwpmFreeMemory0(unsafe.Pointer(&descriptor))
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(system) {
		return errors.New("provider is not owned by SYSTEM; provider retained")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 || dacl.AceCount > maximumProviderCustodyACEs {
		return errors.New("provider has no valid custody ACL; provider retained")
	}
	// WFP default ACLs inherit from their object-type container (null GUID).
	// Compare effective ACEs rather than pinning installation-specific service
	// SIDs or requiring filesystem-style DACL protection.
	container, err := providerSecurity(engine, nil)
	if err != nil {
		return err
	}
	defer fwpmFreeMemory0(unsafe.Pointer(&container))
	localService, err := windows.CreateWellKnownSid(windows.WinLocalServiceSid)
	if err != nil {
		return err
	}
	containerOwner, _, err := container.Owner()
	if err != nil || containerOwner == nil || !containerOwner.Equals(localService) {
		return errors.New("provider container is not owned by the BFE custodian")
	}
	inherited, _, err := container.DACL()
	if err != nil || inherited == nil || inherited.AceCount == 0 || inherited.AceCount > maximumProviderCustodyACEs {
		return errors.New("provider container has no valid custody ACL")
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	networkOperators, err := windows.CreateWellKnownSid(windows.WinBuiltinNetworkConfigurationOperatorsSid)
	if err != nil {
		return err
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(inherited.AceCount); i++ {
		ace, sid, err := providerAllowedACE(inherited, i)
		if err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		const openAndClassify windows.ACCESS_MASK = 0x40 | 0x10
		if sid.Equals(everyone) && ace.Mask & ^openAndClassify == 0 {
			continue
		}
		if sid.Equals(system) || sid.Equals(admins) || sid.Equals(networkOperators) || (sid.SubAuthorityCount() == 6 && strings.HasPrefix(sid.String(), "S-1-5-80-")) {
			continue
		}
		return errors.New("provider container grants rights outside Windows custody; provider retained")
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		ace, sid, err := providerAllowedACE(dacl, i)
		if err != nil {
			return err
		}
		if ace.Header.AceFlags != windows.INHERITED_ACE {
			return errors.New("provider ACL has explicit or nondefault grants; provider retained")
		}
		matched := false
		for j := uint32(0); j < uint32(inherited.AceCount); j++ {
			parent, parentSID, err := providerAllowedACE(inherited, j)
			if err != nil {
				return err
			}
			if parent.Header.AceFlags&windows.INHERIT_ONLY_ACE == 0 && parent.Mask == ace.Mask && parentSID.Equals(sid) {
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("provider ACL differs from inherited Windows custody; provider retained")
		}
	}
	return nil
}

func providerAllowedACE(acl *windows.ACL, index uint32) (*windows.ACCESS_ALLOWED_ACE, *windows.SID, error) {
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, index, &ace); err != nil {
		return nil, nil, err
	}
	if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return nil, nil, errors.New("unsupported WFP custody ACE; provider retained")
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !sid.IsValid() {
		return nil, nil, errors.New("invalid WFP custody SID; provider retained")
	}
	return ace, sid, nil
}

// Provider absence has its own WFP error; filter/sublayer absence is not proof
// that this exact provider lookup or deletion succeeded.
func providerNotFound(result uintptr) bool {
	value := uint32(result)
	return value == 0x80320005 || value == 0x20000005
}
