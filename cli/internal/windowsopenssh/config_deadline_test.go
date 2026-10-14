package windowsopenssh

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

type deadlineRunner struct {
	deadline bool
	output   []byte
	failure  error
}

func (r *deadlineRunner) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	r.deadline = ok && time.Until(deadline) > 0 && time.Until(deadline) <= 15*time.Second
	return append([]byte(nil), r.output...), r.failure
}

func TestValidateServiceConfigUsesBoundedContext(t *testing.T) {
	sshd, config := "/opt/openssh/sshd", "/var/lib/paperboat/sshd_config"
	if runtime.GOOS == "windows" {
		sshd, config = `C:\Program Files\OpenSSH\sshd.exe`, `C:\ProgramData\Paperboat\ssh\sshd_config`
	}
	const privateOutput = "C:\\Users\\Alice\\private\\sshd_config"
	cause := errors.New("sshd rejected configuration")
	runner := &deadlineRunner{output: []byte(privateOutput), failure: cause}
	err := ValidateServiceConfig(runner, sshd, config)
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("validation error %v lost runner cause %v", err, cause)
	}
	if strings.Contains(err.Error(), privateOutput) {
		t.Fatalf("validation error exposed command output: %v", err)
	}
	if !runner.deadline {
		t.Fatal("sshd configuration validation did not receive a bounded context")
	}
}
