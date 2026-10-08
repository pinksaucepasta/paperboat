//go:build !darwin && !linux && !windows

package main

import (
	"context"
	"errors"
	"os"
)

func readExecInputFile(context.Context, *os.File, []byte) (int, error) {
	return 0, errors.New("remote execution streaming input is unavailable on this platform")
}
