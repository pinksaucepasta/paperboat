//go:build !darwin && !linux && !windows

package hostruntimecmd

import (
	"context"
	"os"
)

func readWorkerActivationByte(context.Context, *os.File) (byte, error) {
	return 0, runtimeFailure("worker activation pipe is unsupported on this platform")
}
