//go:build darwin

package managedssh

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// parentWatchdog ends the native tool when pb dies. macOS has no kernel parent-death signal for a child that
// does not cooperate (the native tool is not ours to arm), so a detached shell outlives a SIGKILLed pb and
// sends the tool SIGTERM. A zombie pb counts as dead. shortcut: polls once a second and a recycled tool pid
// inside that second would be signalled; upgrade to a kqueue NOTE_EXIT watcher (processlifetime) if it matters.
const parentWatchdog = `alive() { s=$(/bin/ps -o state= -p "$1" 2>/dev/null); [ -n "$s" ] && [ "${s#Z}" = "$s" ]; }
while alive "$1" && alive "$2"; do /bin/sleep 1; done
alive "$1" && exit 0
alive "$2" && kill -TERM "$2"`

// bindChildToParent returns started, which Execute calls once the tool runs, and release, which it defers.
func bindChildToParent(command *exec.Cmd) (started, release func()) {
	return func() {
		watchdog := exec.Command("/bin/sh", "-c", parentWatchdog, "sh", strconv.Itoa(os.Getpid()), strconv.Itoa(command.Process.Pid))
		watchdog.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if watchdog.Start() == nil {
			go watchdog.Wait()
		}
	}, func() {}
}
