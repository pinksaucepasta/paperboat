package main

import "context"

func withSSHDependencies(ctx context.Context, configure func(*sshDependencies)) context.Context {
	return withDependencies(ctx, defaultSSHDependencies, configure)
}
