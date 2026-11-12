package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// Activation input is a finite in-memory protocol fixture or the exclusively
// borrowed supervisor pipe. Cancellation neither closes nor changes that pipe.
func awaitWorkerActivation(ctx context.Context, input io.Reader) error {
	line, err := readWorkerControlLine(ctx, input)
	if err != nil {
		return err
	}
	if line != "activate\n" {
		return runtimeFailure("invalid worker activation")
	}
	return nil
}

// deferControlPlaneBindKey marks a context whose feature starts without binding its loopback address: the Windows
// hostd binds it with a "bind" line once the previous generation gave it up (see serveWorkerControl).
type deferControlPlaneBindKey struct{}

// readWorkerControlLine reads one line of at most 64 bytes from the supervisor's control input.
func readWorkerControlLine(ctx context.Context, input io.Reader) (string, error) {
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
			return "", runtimeFailure("worker activation input is unavailable", err)
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
		return "", runtimeFailure("worker activation requires a finite reader or the supervisor pipe")
	}
	var line [64]byte
	for n := 0; n < len(line); n++ {
		b, err := readByte(ctx)
		if err != nil {
			return "", runtimeFailure("worker activation input failed", err)
		}
		line[n] = b
		if b == '\n' {
			return string(line[:n+1]), nil
		}
	}
	return "", runtimeFailure("worker activation exceeds its size limit")
}

// errWorkerControlClosed means the hostd closed the worker's control input; the worker keeps serving.
var errWorkerControlClosed = errors.New("the hostd closed the worker control input")

// serveWorkerControl answers the Windows hostd's handover lines after activation (it cannot pass the loopback
// listener as Unix does): "stop" closes the listener and keeps accepted connections, "resume" binds it again
// after a handover that did not complete, "drain" finishes accepted work, bounded by runtimewire.DrainLimit, and
// returns, which ends the worker.
func serveWorkerControl(ctx context.Context, input io.Reader, output io.Writer, feature workerFeature) error {
	accepting, handsOver := feature.(interface {
		StopAccepting()
		ResumeAccepting(context.Context) error
		DrainLocal(context.Context) error
		BindControlPlane(context.Context) error
	})
	for {
		line, err := readWorkerControlLine(ctx, input)
		if err != nil {
			return errWorkerControlClosed
		}
		if !handsOver {
			return runtimeFailure("the runtime feature cannot hand its listener over")
		}
		switch line {
		case "bind\n":
			if err = accepting.BindControlPlane(ctx); err == nil {
				_, err = io.WriteString(output, "bound\n")
			}
		case "stop\n":
			accepting.StopAccepting()
			_, err = io.WriteString(output, "stopped\n")
		case "resume\n":
			if err = accepting.ResumeAccepting(ctx); err == nil {
				_, err = io.WriteString(output, "resumed\n")
			}
		case "drain\n":
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimewire.DrainLimit)
			defer cancel()
			return accepting.DrainLocal(drainCtx)
		default:
			err = runtimeFailure("invalid worker control line")
		}
		if err != nil {
			return err
		}
	}
}
