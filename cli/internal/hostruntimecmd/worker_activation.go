package hostruntimecmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
)

// Activation input is a finite in-memory protocol fixture or the exclusively
// borrowed supervisor pipe. Cancellation neither closes nor changes that pipe.
func awaitWorkerActivation(ctx context.Context, input io.Reader) error {
	var readByte func(context.Context) (byte, error)
	switch reader := input.(type) {
	case *bytes.Buffer, *bytes.Reader, *strings.Reader:
		readByte = func(ctx context.Context) (byte, error) {
			if err := runtimeContextFailure(ctx); err != nil {
				return 0, err
			}
			var b [1]byte
			_, err := io.ReadFull(reader, b[:])
			return b[0], err
		}
	case *os.File:
		info, err := reader.Stat()
		if err != nil {
			return runtimeFailure("worker activation input is unavailable", err)
		}
		if info.Mode().IsRegular() {
			readByte = func(ctx context.Context) (byte, error) {
				if err := runtimeContextFailure(ctx); err != nil {
					return 0, err
				}
				var b [1]byte
				_, err := io.ReadFull(reader, b[:])
				return b[0], err
			}
		} else {
			readByte = func(ctx context.Context) (byte, error) { return readWorkerActivationByte(ctx, reader) }
		}
	default:
		return runtimeFailure("worker activation requires a finite reader or the supervisor pipe")
	}
	var line [64]byte
	for n := 0; n < len(line); n++ {
		b, err := readByte(ctx)
		if err != nil {
			return runtimeFailure("worker activation input failed", err)
		}
		line[n] = b
		if b == '\n' {
			if string(line[:n+1]) != "activate\n" {
				return runtimeFailure("invalid worker activation")
			}
			return nil
		}
	}
	return runtimeFailure("worker activation exceeds its size limit")
}
