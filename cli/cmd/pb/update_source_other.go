//go:build !darwin

package main

import "context"

func manualUpdateExecutable(_ context.Context, input, _ string) (string, error) {
	return input, nil
}
