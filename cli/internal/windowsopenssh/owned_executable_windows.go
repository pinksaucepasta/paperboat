//go:build windows

package windowsopenssh

import (
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
	"strings"
)

// OwnedServiceExecutable resolves the actual SSH shim only after exact service
// arguments and its SYSTEM account have been checked.
func OwnedServiceExecutable(config Config) (string, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return "", err
	}
	defer manager.Disconnect()
	current, err := manager.OpenService(config.ServiceName)
	if err != nil {
		return "", err
	}
	defer current.Close()
	declaration, err := current.Config()
	if err != nil {
		return "", err
	}
	args, err := windows.DecomposeCommandLine(declaration.BinaryPathName)
	if err != nil || len(args) == 0 || !sameOwnedServiceCommand(declaration.BinaryPathName, args[0], config) || !(strings.EqualFold(declaration.ServiceStartName, "LocalSystem") || strings.EqualFold(declaration.ServiceStartName, "SYSTEM") || strings.EqualFold(declaration.ServiceStartName, `NT AUTHORITY\SYSTEM`)) {
		return "", ErrServiceOwnership
	}
	return args[0], nil
}
