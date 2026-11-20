//go:build windows

package main

import "context"

func withWindowsUninstallDependencies(ctx context.Context, configure func(*windowsUninstallDependencies)) context.Context {
	return withDependencies(ctx, defaultWindowsUninstallDependencies, configure)
}
