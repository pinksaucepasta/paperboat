//go:build windows

package main

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"golang.org/x/sys/windows"
)

// platformUpdateActivating asks SCM, with query-only rights a standard user holds, whether this owner's update
// activator service is running.
func platformUpdateActivating() (bool, error) {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	instance, err := hostinstall.WindowsInstanceForSID(user.User.Sid.String())
	if err != nil {
		return false, err
	}
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false, err
	}
	defer windows.CloseServiceHandle(manager)
	handle, err := windows.OpenService(manager, windows.StringToUTF16Ptr("PaperboatUpdateActivator-"+instance), windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer windows.CloseServiceHandle(handle)
	var status windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(handle, &status); err != nil {
		return false, err
	}
	return status.CurrentState != windows.SERVICE_STOPPED, nil
}
