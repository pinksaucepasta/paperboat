//go:build !darwin && !linux && !windows

package deviceservices

import "context"

func snapshot(context.Context) ([]Service, error) { return nil, ErrDiscoveryUnavailable }
