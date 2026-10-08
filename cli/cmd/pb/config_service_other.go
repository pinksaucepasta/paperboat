//go:build !windows

package main

import "context"

func manageWindowsConfigService(context.Context, string, bool) (bool, error) { return false, nil }
