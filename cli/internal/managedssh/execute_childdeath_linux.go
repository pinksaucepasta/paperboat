//go:build linux

package managedssh

import (
	"os/exec"
	"runtime"
	"syscall"
)

// bindChildToParent makes the kernel kill the native tool when pb dies, so a retry never races a transfer
// that outlived its pb (prior art: teleport/lib/srv/exec_linux_test.go:109 asserts Pdeathsig SIGKILL on the
// child it starts). Pdeathsig fires when the thread that started the child exits, so the thread stays
// locked until release (prior art: tailscale/ssh/tailssh/incubator.go:398 LockOSThread).
func bindChildToParent(command *exec.Cmd) (release func()) {
	runtime.LockOSThread()
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Pdeathsig = syscall.SIGKILL
	return runtime.UnlockOSThread
}
