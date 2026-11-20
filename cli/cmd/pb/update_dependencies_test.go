package main

import "context"

func withUpdateDependencies(ctx context.Context, configure func(*updateDependencies)) context.Context {
	deps := updateDependenciesOf(ctx)
	configure(&deps)
	return context.WithValue(ctx, updateDependenciesKey{}, deps)
}
