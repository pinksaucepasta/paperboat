package tunnelmanager

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

var (
	ErrInvalidConfig               = errors.New("invalid tunnel manager configuration")
	ErrOriginUnavailable           = errors.New("tunnel origin unavailable")
	ErrProductionAssemblyInvalid   = errors.New("invalid production tunnel assembly")
	ErrProductionCredentialMissing = errors.New("production tunnel credential renewal source is unavailable")
)

// OriginProber performs a safe, non-mutating readiness probe for one route.
// Implementations must honor the route connect timeout and caller context.
type OriginProber interface {
	ProbeOrigin(context.Context, hoststate.TunnelConfigRoute) error
}
