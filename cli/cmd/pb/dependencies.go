package main

import "context"

// Each command family keeps the edges tests must replace (a daemon socket, a browser, a prompt, a remote call) in one
// dependencies struct. Production builds the default one; a test puts its own in the command's context with
// withDependencies. No package variable is swapped, so tests share no state and may run in parallel.
type dependenciesKey[T any] struct{}

func dependenciesIn[T any](ctx context.Context, defaults func() T) T {
	if ctx != nil {
		if deps, ok := ctx.Value(dependenciesKey[T]{}).(T); ok {
			return deps
		}
	}
	return defaults()
}
