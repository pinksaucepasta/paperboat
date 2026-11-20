package main

import "context"

func withWorkspaceDependencies(ctx context.Context, configure func(*workspaceDependencies)) context.Context {
	return withDependencies(ctx, defaultWorkspaceDependencies, configure)
}
