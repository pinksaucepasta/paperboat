//go:build !linux && !darwin && !windows

package machineguard

import "context"

func Uninstall(context.Context) (UninstallResult, error) { return UninstallResult{}, ErrUnsupported }
