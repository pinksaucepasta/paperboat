package main

import "context"

// withDependencies returns ctx carrying defaults() adjusted by configure.
func withDependencies[T any](ctx context.Context, defaults func() T, configure func(*T)) context.Context {
	deps := dependenciesIn(ctx, defaults)
	configure(&deps)
	return context.WithValue(ctx, dependenciesKey[T]{}, deps)
}

func withHostDependencies(ctx context.Context, configure func(*hostDependencies)) context.Context {
	return withDependencies(ctx, defaultHostDependencies, configure)
}
