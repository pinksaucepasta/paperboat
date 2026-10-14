package envinject

import "context"

type LaunchContext struct {
	WorkspaceID    string
	ActorAccountID string
}
type launchContextKey struct{}

// WithLaunchContext is called only after an execution credential is verified.
// Client request bodies do not supply either field.
func WithLaunchContext(ctx context.Context, workspace, actor string) context.Context {
	return context.WithValue(ctx, launchContextKey{}, LaunchContext{workspace, actor})
}
func LaunchContextFrom(ctx context.Context) (LaunchContext, bool) {
	if ctx == nil {
		return LaunchContext{}, false
	}
	binding, ok := ctx.Value(launchContextKey{}).(LaunchContext)
	return binding, ok && validIdentifier(binding.WorkspaceID) && validIdentifier(binding.ActorAccountID)
}
