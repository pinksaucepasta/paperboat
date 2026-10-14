//go:build windows

package windowsopenssh

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	"reflect"
	"strings"
	"unsafe"
)

// ServiceSnapshot records only an exactly owned managed SSH registration.
// The caller persists it in the protected native installation transaction.
type ServiceSnapshot struct {
	Present         bool                 `json:"present"`
	Configuration   mgr.Config           `json:"configuration"`
	Running         bool                 `json:"running"`
	Recovery        []mgr.RecoveryAction `json:"recovery"`
	RecoveryReset   uint32               `json:"recovery_reset"`
	RecoveryOnCrash bool                 `json:"recovery_on_crash"`
}

func CaptureService(ctx context.Context, config Config) (*ServiceSnapshot, error) {
	if ctx == nil || validate(config) != nil {
		return nil, ErrInvalidConfig
	}
	if _, err := validatedServiceQueryOwner(config.ServiceName, config.OwnerSID); err != nil {
		return nil, err
	}
	m, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(config.ServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return &ServiceSnapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		return nil, err
	}
	if !sameOwnedServiceCommand(c.BinaryPathName, config.ServiceExecutable, config) || !strings.EqualFold(c.ServiceStartName, "LocalSystem") {
		return nil, ErrServiceOwnership
	}
	status, err := s.Query()
	if err != nil {
		return nil, err
	}
	if status.State != svc.Running && status.State != svc.Stopped {
		return nil, ErrServiceOwnership
	}
	actions, err := s.RecoveryActions()
	if err != nil {
		return nil, err
	}
	if len(actions) > len(paperboatServiceRecovery) {
		return nil, ErrServiceOwnership
	}
	command, err := s.RecoveryCommand()
	if err != nil {
		return nil, err
	}
	if command != "" {
		return nil, ErrServiceOwnership
	}
	reboot, err := s.RebootMessage()
	if err != nil {
		return nil, err
	}
	if reboot != "" {
		return nil, ErrServiceOwnership
	}
	reset, err := s.ResetPeriod()
	if err != nil {
		return nil, err
	}
	crash, err := s.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return nil, err
	}
	return &ServiceSnapshot{Present: true, Configuration: c, Running: status.State == svc.Running, Recovery: actions, RecoveryReset: reset, RecoveryOnCrash: crash}, nil
}

func RestoreService(ctx context.Context, config Config, state ServiceSnapshot) error {
	if ctx == nil || validate(config) != nil {
		return ErrInvalidConfig
	}
	if _, err := validatedServiceQueryOwner(config.ServiceName, config.OwnerSID); err != nil {
		return err
	}
	if len(state.Recovery) > len(paperboatServiceRecovery) {
		return ErrServiceOwnership
	}
	if !state.Present {
		return RemoveServiceOwned(ctx, config)
	}
	current, err := CaptureService(ctx, config)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(*current, state) {
		return nil
	}
	c := state.Configuration
	if !sameOwnedServiceCommand(c.BinaryPathName, config.ServiceExecutable, config) || !strings.EqualFold(c.ServiceStartName, "LocalSystem") {
		return ErrServiceOwnership
	}
	// Existing registrations must prove the same owned namespace before mutation.
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(config.ServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		args, e := windows.DecomposeCommandLine(c.BinaryPathName)
		if e != nil || len(args) < 2 {
			return ErrServiceOwnership
		}
		s, err = m.CreateService(config.ServiceName, args[0], c, args[1:]...)
	} else if err == nil {
		current, e := s.Config()
		if e != nil {
			s.Close()
			return e
		}
		if !sameOwnedServiceCommand(current.BinaryPathName, config.ServiceExecutable, config) {
			s.Close()
			return ErrServiceOwnership
		}
		err = s.UpdateConfig(c)
	}
	if err != nil {
		return err
	}
	defer s.Close()
	if err := grantServiceOwnerQuery(s.Handle, config.ServiceName, config.OwnerSID); err != nil {
		return err
	}
	if len(state.Recovery) == 0 {
		action := windows.SC_ACTION{}
		reset := windows.SERVICE_FAILURE_ACTIONS{ResetPeriod: state.RecoveryReset, Actions: &action}
		if err := windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_FAILURE_ACTIONS, (*byte)(unsafe.Pointer(&reset))); err != nil {
			return err
		}
	} else if err := s.SetRecoveryActions(state.Recovery, state.RecoveryReset); err != nil {
		return err
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(state.RecoveryOnCrash); err != nil {
		return err
	}
	if state.Running {
		if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return err
		}
		return waitForServiceState(ctx, s, svc.Running, 30e9)
	}
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return err
	}
	return waitForServiceState(ctx, s, svc.Stopped, 30e9)
}
