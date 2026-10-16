package main

import (
	"context"
	"errors"
	"io"
	"os"
)

var errExecInputLimit = errors.New("remote execution input queue exceeds its bounded capacity")

// The command owns reads exclusively, but borrows the file itself. No worker
// closes caller stdin or changes its descriptor/console flags to interrupt it.
func readExecInput(ctx context.Context, reader io.Reader, value []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if file, ok := reader.(*os.File); ok {
		if file == nil {
			return 0, os.ErrInvalid
		}
		info, err := file.Stat()
		if err != nil {
			return 0, err
		}
		// Darwin poll rejects the valid null device with POLLNVAL. Its read
		// is already nonblocking EOF; identify the borrowed object, not its name.
		if info.Mode()&os.ModeCharDevice != 0 {
			null, err := os.Stat(os.DevNull)
			if err == nil && os.SameFile(info, null) {
				return file.Read(value)
			}
		}
		if !info.Mode().IsRegular() {
			return readExecInputFile(ctx, file, value)
		}
	}
	return reader.Read(value)
}
