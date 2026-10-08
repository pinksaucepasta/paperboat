package session

import "context"

type launchEnvironmentContextKey struct{}

// WithLaunchEnvironment carries managed values only until process launch. These
// values never become part of a durable command, input or operation identity.
func WithLaunchEnvironment(ctx context.Context, values []string) context.Context {
	return context.WithValue(ctx, launchEnvironmentContextKey{}, append([]string(nil), values...))
}
func LaunchEnvironment(ctx context.Context) []string {
	values, _ := ctx.Value(launchEnvironmentContextKey{}).([]string)
	return append([]string(nil), values...)
}
