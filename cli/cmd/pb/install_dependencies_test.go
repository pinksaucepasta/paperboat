package main

import "context"

func withInstallDependencies(ctx context.Context, configure func(*installDependencies)) context.Context {
	return withDependencies(ctx, defaultInstallDependencies, configure)
}
