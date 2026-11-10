package privateproxyconfig

import (
	"context"
	"os/exec"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/processlaunch"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	processlaunch.ConfigureBackground(command)
	command.WaitDelay = time.Second
	return command.CombinedOutput()
}
