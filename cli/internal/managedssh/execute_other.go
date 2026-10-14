//go:build !darwin && !linux && !windows

package managedssh

import (
	"context"
	"errors"
)

var ErrOpenSSHExecution = errors.New("OpenSSH execution is unsupported on this platform")

type OpenSSHExecutor struct{}

func (OpenSSHExecutor) Execute(context.Context, string, []string, []string) error {
	return ErrOpenSSHExecution
}
