//go:build !windows

package main

import "context"

func windowsUninstallInFlight(context.Context) error { return nil }
