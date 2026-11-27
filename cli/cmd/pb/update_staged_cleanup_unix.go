//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// L46: a pb update --from whose root step outlived pb (Ctrl-C, its bound, or a closed terminal killing pb) left
// its ~100 MB staged update in the temporary directory for good: pb keeps it for that step (a step still reading
// it must not lose it) and pb itself is gone when the step ends. A detached helper, started when the update is
// staged, removes it once pb has exited and no privileged install step runs any more.
const stagedCleanupCommand = "__remove-staged-update"

// startStagedCleanup starts that helper for directory; tests replace it (a test binary must not run itself).
var startStagedCleanup = func(directory string) {
	executable, err := os.Executable()
	if err != nil {
		return
	}
	helper := exec.Command(executable, stagedCleanupCommand, directory, strconv.Itoa(os.Getpid()))
	helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // a closed terminal's hangup ends pb, not the helper
	if helper.Start() == nil {
		_ = helper.Process.Release()
	}
}

func stagedCleanupCobraCommand() *cobra.Command {
	return &cobra.Command{Use: stagedCleanupCommand + " <directory> <pid>", Hidden: true, Args: cobra.ExactArgs(2),
		// None of pb's per-command checks (workspace, update notice and policy): the helper only removes a directory.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil }, RunE: func(_ *cobra.Command, args []string) error {
			pid, err := strconv.Atoi(args[1])
			if err != nil || pid <= 0 {
				return errors.New("invalid pid")
			}
			waitThenRemoveStaged(args[0], pid, stagedCleanupPoll, stagedCleanupBound)
			return nil
		}}
}

var (
	stagedCleanupPoll  = 2 * time.Second
	stagedCleanupBound = 6 * time.Hour // past it the step may still need it: keep it, the system's temp cleanup ends it
)

// waitThenRemoveStaged waits while pb (pid) runs or a privileged step may still read the staged update, then removes
// the directory if it is still pb's own staging directory.
func waitThenRemoveStaged(directory string, pid int, poll, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for processAlive(pid) || privilegedStepRunning() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(poll)
	}
	return removeOwnStagedUpdate(directory)
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// privilegedStepRunning reports any root install, commit or rollback step (pb __runtime-service *-supplied), on
// Linux in its transient service or under sudo, on macOS under sudo. It cannot tell whose it is: another account's
// step only delays the removal. Unknown counts as running. Tests replace it.
var privilegedStepRunning = func() bool {
	// A hidepid /proc hides root's processes from ps: the step's fixed transient service still shows (Linux).
	for _, systemctl := range []string{"/usr/bin/systemctl", "/bin/systemctl"} {
		if _, err := os.Stat(systemctl); err != nil {
			continue
		}
		for _, operation := range []string{"install-supplied", "commit-supplied", "rollback-supplied"} {
			unit := "paperboat-" + operation + "-u" + strconv.Itoa(os.Getuid()) + ".service"
			if exec.Command(systemctl, "is-active", "--quiet", unit).Run() == nil {
				return true
			}
		}
		break
	}
	// -ww: without it BSD ps cuts a non-terminal line at 79 columns, dropping the step's name from a long pb path.
	out, err := exec.Command("/bin/ps", "-A", "-ww", "-o", "args=").Output()
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "__runtime-service" {
				switch fields[i+1] {
				case "install-supplied", "commit-supplied", "rollback-supplied":
					return true
				}
			}
		}
	}
	return false
}

// removeOwnStagedUpdate removes directory only when it is a paperboat-update-<digits> directory directly in the
// temporary directory, not a link, owned by this account: exactly what os.MkdirTemp made for this update.
func removeOwnStagedUpdate(directory string) bool {
	if !filepath.IsAbs(directory) || filepath.Dir(directory) != filepath.Clean(os.TempDir()) {
		return false
	}
	digits, ok := strings.CutPrefix(filepath.Base(directory), "paperboat-update-")
	if !ok || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return false
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return false
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return false
	}
	return os.RemoveAll(directory) == nil // RemoveAll never follows a link inside it
}
