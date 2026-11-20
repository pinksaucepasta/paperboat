package main

import "context"

func (d tunnelDependencies) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, tunnelDependenciesKey{}, d)
}

func withTunnelDependencies(ctx context.Context, configure func(*tunnelDependencies)) context.Context {
	deps := tunnelDependenciesOf(ctx)
	configure(&deps)
	return deps.context(ctx)
}
