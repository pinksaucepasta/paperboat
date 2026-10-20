//go:build darwin

package hostinstall

import "context"

// Only systemd hosts end a user's services at logout; launchd and Windows services do not.
func ensureUserLinger(context.Context, Request, installPaths) error  { return nil }
func restoreUserLinger(context.Context, Request, installPaths) error { return nil }
