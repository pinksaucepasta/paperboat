package main

import "context"

func withAccessDependencies(ctx context.Context, configure func(*accessDependencies)) context.Context {
	return withDependencies(ctx, defaultAccessDependencies, configure)
}
