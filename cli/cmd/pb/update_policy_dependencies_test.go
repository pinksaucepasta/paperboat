package main

import "context"

func withUpdatePolicyDependencies(ctx context.Context, configure func(*updatePolicyDependencies)) context.Context {
	return withDependencies(ctx, defaultUpdatePolicyDependencies, configure)
}
