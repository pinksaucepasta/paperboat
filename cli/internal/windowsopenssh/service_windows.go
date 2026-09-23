//go:build windows

package windowsopenssh

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

var paperboatServiceRecovery = []mgr.RecoveryAction{
	{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
	{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	{Type: mgr.NoAction, Delay: 0},
}

// ServiceRecoveryActions returns the exact SCM recovery policy owned by the
// Paperboat OpenSSH service. Update activation must preserve this policy when
// it changes only the service executable target.
func ServiceRecoveryActions() []mgr.RecoveryAction {
	return append([]mgr.RecoveryAction(nil), paperboatServiceRecovery...)
}

func InstallService(ctx context.Context, serviceName, serviceExecutable, sshdPath, configPath, ownerSID string) error {
	if ctx == nil || serviceName == "" || !filepath.IsAbs(serviceExecutable) || !filepath.IsAbs(sshdPath) || !filepath.IsAbs(configPath) {
		return ErrInvalidConfig
	}
	if _, err := validatedServiceQueryOwner(serviceName, ownerSID); err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(serviceName)
	instance := strings.TrimPrefix(serviceName, ServiceName+"-")
	if len(instance) != 25 || instance[0] != 'u' {
		return ErrInvalidConfig
	}
	arguments := []string{"daemon", "__windows-sshd-service", "--instance", instance}
	restartRequired := false
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		service, err = manager.CreateService(serviceName, serviceExecutable, mgr.Config{
			DisplayName: "Paperboat OpenSSH Server", Description: "Loopback-only OpenSSH endpoint managed by Paperboat",
			StartType: mgr.StartAutomatic, ErrorControl: mgr.ErrorNormal, ServiceStartName: "LocalSystem",
			SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED,
		}, arguments...)
	} else if err == nil {
		current, configErr := service.Config()
		if configErr != nil {
			service.Close()
			return configErr
		}
		expectedCommand := windows.ComposeCommandLine(append([]string{serviceExecutable}, arguments...))
		if !samePaperboatServiceCommand(current.BinaryPathName, serviceName, sshdPath, configPath) && !sameLegacyServiceCommand(current.BinaryPathName, sshdPath, configPath) {
			service.Close()
			return ErrServiceOwnership
		}
		restartRequired = !sameServiceCommand(current.BinaryPathName, serviceName, serviceExecutable, sshdPath, configPath)
		current.BinaryPathName = expectedCommand
		current.StartType = mgr.StartAutomatic
		current.ErrorControl = mgr.ErrorNormal
		current.ServiceStartName = "LocalSystem"
		current.SidType = windows.SERVICE_SID_TYPE_UNRESTRICTED
		err = service.UpdateConfig(current)
	}
	if err != nil {
		return err
	}
	defer service.Close()
	if err := service.SetRecoveryActions(paperboatServiceRecovery, 24*60*60); err != nil {
		return err
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return err
	}
	installed, err := service.Config()
	if err != nil || !sameServiceCommand(installed.BinaryPathName, serviceName, serviceExecutable, sshdPath, configPath) || !strings.EqualFold(installed.ServiceStartName, "LocalSystem") || installed.StartType != mgr.StartAutomatic || installed.ErrorControl != mgr.ErrorNormal || installed.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED {
		return errors.Join(ErrServiceOwnership, err)
	}
	if err := grantServiceOwnerQuery(service.Handle, serviceName, ownerSID); err != nil {
		return err
	}
	if restartRequired {
		if status, queryErr := service.Query(); queryErr == nil && status.State != svc.Stopped {
			if _, stopErr := service.Control(svc.Stop); stopErr != nil && !errors.Is(stopErr, windows.ERROR_SERVICE_NOT_ACTIVE) {
				return stopErr
			}
			if err := waitForServiceState(ctx, service, svc.Stopped, 30*time.Second); err != nil {
				return err
			}
		}
	}
	if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		status, queryErr := service.Query()
		if queryErr != nil {
			return queryErr
		}
		if status.State == svc.Running {
			return nil
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

func waitForServiceState(ctx context.Context, service *mgr.Service, wanted svc.State, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		status, err := service.Query()
		if err != nil {
			return err
		}
		if status.State == wanted {
			return nil
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

func sameServiceCommand(command, serviceName, serviceExecutable, sshdPath, configPath string) bool {
	arguments, err := windows.DecomposeCommandLine(command)
	instance := strings.TrimPrefix(serviceName, ServiceName+"-")
	return err == nil && len(arguments) == 5 && sameWindowsPath(arguments[0], serviceExecutable) && arguments[1] == "daemon" && arguments[2] == "__windows-sshd-service" && arguments[3] == "--instance" && arguments[4] == instance
}

func samePaperboatServiceCommand(command, serviceName, sshdPath, configPath string) bool {
	arguments, err := windows.DecomposeCommandLine(command)
	instance := strings.TrimPrefix(serviceName, ServiceName+"-")
	return err == nil && len(arguments) == 5 && arguments[1] == "daemon" && arguments[2] == "__windows-sshd-service" && arguments[3] == "--instance" && arguments[4] == instance
}

func sameLegacyServiceCommand(command, sshdPath, configPath string) bool {
	arguments, err := windows.DecomposeCommandLine(command)
	return err == nil && len(arguments) == 4 && sameWindowsPath(arguments[0], sshdPath) && strings.EqualFold(arguments[1], "-D") &&
		strings.EqualFold(arguments[2], "-f") && sameWindowsPath(arguments[3], configPath)
}

func sameWindowsPath(first, second string) bool {
	return strings.EqualFold(filepath.Clean(first), filepath.Clean(second))
}

// RemoveServiceOwned stops and deletes only a PaperboatSshd registration whose
// command still points at Paperboat's dedicated binaries and state. A service
// merely named PaperboatSshd is never sufficient ownership evidence.
func RemoveServiceOwned(ctx context.Context, config Config) error {
	if err := validate(config); err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(config.ServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer service.Close()
	current, err := service.Config()
	if err != nil {
		return err
	}
	serviceExecutable, err := paperboatServiceExecutable(config.ServiceExecutable)
	if err != nil {
		return err
	}
	if !sameOwnedServiceCommand(current.BinaryPathName, serviceExecutable, config) {
		return ErrServiceOwnership
	}
	status, queryErr := service.Query()
	if queryErr != nil {
		return queryErr
	}
	if status.State != svc.Stopped {
		if _, err := service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
		if ctx == nil {
			ctx = context.Background()
		}
		if err := waitForServiceState(ctx, service, svc.Stopped, 30*time.Second); err != nil {
			return err
		}
	}
	return service.Delete()
}

func sameOwnedServiceCommand(command, serviceExecutable string, config Config) bool {
	sshdPath := filepath.Join(config.InstallRoot, "sshd.exe")
	configPath := filepath.Join(config.StateRoot, "sshd_config")
	return sameServiceCommand(command, config.ServiceName, serviceExecutable, sshdPath, configPath) || sameLegacyServiceCommand(command, sshdPath, configPath)
}
