package bugreport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
)

var errRecordingInput = errors.New("recording requires standard input, a regular file, or a finite in-memory reader")

type recordingInputError struct{ Err error }

func (e recordingInputError) Error() string { return "could not read reproduction confirmation" }
func (e recordingInputError) Unwrap() error { return e.Err }

// waitForLine borrows input exclusively for the recording prompt. It never
// closes it, changes its flags/mode, retains line contents, or starts a worker.
// Concurrent reads by another owner are outside this command's input contract.
func waitForLine(ctx context.Context, input io.Reader) error {
	if ctx == nil || input == nil {
		return errRecordingInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch input := input.(type) {
	case *bytes.Buffer:
		if input == nil {
			return errRecordingInput
		}
		return discardRecordingLine(ctx, input)
	case *bytes.Reader:
		if input == nil {
			return errRecordingInput
		}
		return discardRecordingLine(ctx, input)
	case *strings.Reader:
		if input == nil {
			return errRecordingInput
		}
		return discardRecordingLine(ctx, input)
	case *os.File:
		if input == nil {
			return errRecordingInput
		}
		info, err := input.Stat()
		if err != nil {
			return recordingInputError{Err: err}
		}
		if info.Mode().IsRegular() {
			return discardRecordingLine(ctx, input)
		}
		return waitRecordingFile(ctx, input)
	default:
		return errRecordingInput
	}
}

func discardRecordingLine(ctx context.Context, input io.Reader) error {
	var value [1]byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := input.Read(value[:])
		if count > 0 && value[0] == '\n' {
			return nil
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return recordingInputError{Err: err}
		}
		if count == 0 {
			return recordingInputError{Err: io.ErrNoProgress}
		}
	}
}
