package main

import "context"

// quietAuthDependencies are the production ones except that this computer's own daemon is never asked whether it
// awaits enrollment: on a development host with a paired-but-unenrolled daemon login tests failed while CI, with no
// daemon, passed.
func quietAuthDependencies() authDependencies {
	deps := defaultAuthDependencies()
	deps.daemonAwaitingEnrollment = func(context.Context) bool { return false }
	return deps
}

func withAuthDependencies(ctx context.Context, configure func(*authDependencies)) context.Context {
	return withDependencies(ctx, quietAuthDependencies, configure)
}
