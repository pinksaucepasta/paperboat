//go:build !darwin && !linux && !windows

package bugreport

import (
	"context"
	"os"
)

func waitRecordingFile(context.Context, *os.File) error { return errRecordingInput }
