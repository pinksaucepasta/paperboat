//go:build !linux && !darwin

package processlifetime

import "context"

func ArmParentDeath(context.Context) error { return nil }
