package environmentmanager

import "errors"

var (
	ErrVariableNotConfigured = errors.New("environment variable is not configured")
	ErrAuthorityFork         = errors.New("ENV authority history conflicts with local state")
	ErrIntegrity             = errors.New("ENV encrypted state failed verification")
)
