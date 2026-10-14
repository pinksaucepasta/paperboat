//go:build !linux

package containers

import (
	"context"
	"errors"
	"io"
)

func Serve(context.Context, string, io.Writer, func(context.Context, string, error)) error {
	return errors.New("self-hosted containers require supported Linux")
}

func Teardown(context.Context, string) error {
	return errors.New("self-hosted containers require supported Linux")
}
