package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/coder/websocket"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: preview-e2e-client <websocket-url> <payload>")
		os.Exit(2)
	}
	if err := exchange(context.Background(), os.Args[1], os.Args[2], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type exchangeFailure struct {
	message string
	cause   error
}

func (e *exchangeFailure) Error() string { return e.message }
func (e *exchangeFailure) Unwrap() error { return e.cause }

func exchange(parent context.Context, address, payload string, output io.Writer) (resultErr error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	message := "Preview WebSocket connection failed; check the URL and target, then retry."
	defer func() {
		if resultErr != nil {
			resultErr = &exchangeFailure{message: message, cause: resultErr}
		}
	}()
	connection, _, err := websocket.Dial(ctx, address, nil)
	if err == nil {
		defer func() {
			// CloseNow returns net.ErrClosed when its own reader already closed
			// the connection and all WebSocket goroutines have stopped.
			if closeErr := connection.CloseNow(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				if resultErr == nil {
					message = "Preview WebSocket cleanup failed; the response may already have been delivered."
				} else {
					message += " WebSocket cleanup also failed."
				}
				resultErr = errors.Join(resultErr, closeErr)
			}
		}()
		message = "Preview WebSocket request could not be sent; check the target, then retry."
		err = connection.Write(ctx, websocket.MessageText, []byte(payload))
	}
	var response []byte
	if err == nil {
		message = "Preview WebSocket response could not be read; check the target, then retry."
		_, response, err = connection.Read(ctx)
	}
	if err != nil {
		return err
	}
	message = "Preview response could not be written; check the output destination before retrying."
	n, err := output.Write(response)
	if err == nil && n != len(response) {
		err = io.ErrShortWrite
	}
	return err
}
