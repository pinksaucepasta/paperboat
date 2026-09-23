//go:build !linux && !darwin && !windows

package deviceguard

import "context"

func Uninstall(context.Context) (UninstallResult, error) { return UninstallResult{}, ErrUnsupported }
