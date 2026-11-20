package main

import "context"

func withEnvDependencies(ctx context.Context, configure func(*envDependencies)) context.Context {
	return withDependencies(ctx, defaultEnvDependencies, configure)
}
