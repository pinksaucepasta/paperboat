//go:build !darwin && !linux && !windows

package machineservices

import "context"

func snapshot(context.Context) ([]Service, error) { return nil, ErrDiscoveryUnavailable }
