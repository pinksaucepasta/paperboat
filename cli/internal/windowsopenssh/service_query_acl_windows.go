//go:build windows

package windowsopenssh

import (
	"fmt"
	"strings"

	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

func validatedServiceQueryOwner(serviceName, ownerSID string) (*windows.SID, error) {
	owner, err := windows.StringToSid(ownerSID)
	if err != nil || owner == nil || !owner.IsValid() {
		return nil, ErrInvalidConfig
	}
	instance, err := hostservice.WindowsUserInstance(owner.String())
	if err != nil || !strings.EqualFold(serviceName, ServiceName+"-"+instance) {
		return nil, ErrServiceOwnership
	}
	return owner, nil
}

func serviceOwnerQueryACL(existing *windows.ACL, owner *windows.SID) (*windows.ACL, error) {
	if existing == nil || owner == nil || !owner.IsValid() {
		return nil, ErrServiceOwnership
	}
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.SERVICE_QUERY_CONFIG | windows.SERVICE_QUERY_STATUS,
		AccessMode:        windows.GRANT_ACCESS, Inheritance: windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(owner)},
	}}, existing)
}

func serviceDACLString(acl *windows.ACL) (string, error) {
	if acl == nil {
		return "", ErrServiceOwnership
	}
	descriptor, err := windows.NewSecurityDescriptor()
	if err != nil {
		return "", err
	}
	if err := descriptor.SetDACL(acl, true, false); err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

// Grant only observation rights on this owner's already-verified service.
// Merge preserves SYSTEM/admin and all other existing ACEs; preflight never
// receives start/stop, configuration mutation or service-DACL authority.
func grantServiceOwnerQuery(handle windows.Handle, serviceName, ownerSID string) error {
	owner, err := validatedServiceQueryOwner(serviceName, ownerSID)
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read managed SSH service permissions: %w", err)
	}
	existing, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	merged, err := serviceOwnerQueryACL(existing, owner)
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
	if err := windows.SetSecurityInfo(handle, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION, nil, nil, merged, nil); err != nil {
		return fmt.Errorf("grant owner managed SSH service query: %w", err)
	}
	verified, err := windows.GetSecurityInfo(handle, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION)
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
