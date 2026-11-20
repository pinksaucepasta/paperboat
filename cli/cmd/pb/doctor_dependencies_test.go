package main

import "context"

func withDoctorDependencies(ctx context.Context, configure func(*doctorDependencies)) context.Context {
	return withDependencies(ctx, defaultDoctorDependencies, configure)
}
