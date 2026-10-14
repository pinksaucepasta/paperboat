//go:build windows

package windowsopenssh

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func sshdProcessOwnerQueryACL(existing *windows.ACL, owner *windows.SID) (*windows.ACL, error) {
	if existing == nil || owner == nil || !owner.IsValid() {
		return nil, ErrServiceOwnership
	}
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.PROCESS_QUERY_LIMITED_INFORMATION,
		AccessMode:        windows.GRANT_ACCESS, Inheritance: windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(owner)},
	}}, existing)
}

// The SYSTEM wrapper calls this only with the handle of its own newly-created
// sshd child. Grant image/status observation, never process modification,
// memory/token access, duplication or termination, to the exact enrolled SID.
func grantSSHDProcessOwnerQuery(handle windows.Handle, serviceName, ownerSID string) error {
	owner, err := validatedServiceQueryOwner(serviceName, ownerSID)
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read sshd process permissions: %w", err)
	}
	existing, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	merged, err := sshdProcessOwnerQueryACL(existing, owner)
	if err != nil {
		return err
	}
	expected, err := serviceDACLString(merged)
	if err != nil {
		return err
	}
	current, err := serviceDACLString(existing)
	if err != nil {
		return err
	}
	if current == expected {
		return nil
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, merged, nil); err != nil {
		return fmt.Errorf("grant owner sshd process query: %w", err)
	}
	verified, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	actual, _, err := verified.DACL()
	if err != nil {
		return err
	}
	actualString, err := serviceDACLString(actual)
	if err != nil {
		return err
	}
	if actualString != expected {
		return ErrServiceOwnership
	}
	return nil
}
