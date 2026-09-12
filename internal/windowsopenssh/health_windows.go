//go:build windows

package windowsopenssh

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func collectLoopbackHealth(ctx context.Context, config Config, result Result) (ServiceHealth, error) {
	if err := ctx.Err(); err != nil {
		return ServiceHealth{}, err
	}
	record, err := queryLoopbackService(config.ServiceName)
	if err != nil {
		return ServiceHealth{}, fmt.Errorf("%w: query managed SSH service: %w", ErrServiceUnhealthy, err)
	}
	health := ServiceHealth{Service: record}
	if !record.Exists {
		return health, nil
	}
	health.Listeners, err = collectNativeLoopbackListeners(result.Port)
	if err != nil {
		return ServiceHealth{}, fmt.Errorf("%w: query managed SSH listeners: %w", ErrServiceUnhealthy, err)
	}
	if err := ctx.Err(); err != nil {
		return ServiceHealth{}, err
	}
	return health, nil
}

func queryLoopbackService(name string) (ServiceRecord, error) {
	record := ServiceRecord{Name: name}
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return record, fmt.Errorf("open service manager: %w", err)
	}
	defer windows.CloseServiceHandle(manager)
	encoded, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return record, err
	}
	handle, err := windows.OpenService(manager, encoded, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return record, nil
	}
	if err != nil {
		return record, fmt.Errorf("open exact service: %w", err)
	}
	service := mgr.Service{Name: name, Handle: handle}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return record, fmt.Errorf("query service status: %w", err)
	}
	configuration, err := service.Config()
	if err != nil {
		return record, fmt.Errorf("query service config: %w", err)
	}
	record.Exists = true
	record.ProcessID = status.ProcessId
	record.PathName = configuration.BinaryPathName
	if status.State == svc.Running {
		record.State = "running"
	}
	return record, nil
}

func validLoopbackServiceCommand(config Config, result Result, command string) bool {
	return sameServiceCommand(command, config.ServiceName, config.ServiceExecutable, result.SSHDPath, result.ConfigPath)
}
