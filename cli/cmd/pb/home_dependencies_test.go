package main

import "context"

func withHomeDependencies(ctx context.Context, configure func(*homeDependencies)) context.Context {
	return withDependencies(ctx, defaultHomeDependencies, configure)
}
