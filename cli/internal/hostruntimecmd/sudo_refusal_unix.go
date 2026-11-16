//go:build darwin || linux

package hostruntimecmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
)

// AdministratorRequiredError is a privileged step that could not start because sudo refused it or is missing.
// Callers branch on it with errors.As; its text names the step and how to grant the rights.
type AdministratorRequiredError struct {
	Step   string
	Reason string
}

func (e *AdministratorRequiredError) Error() string {
	return fmt.Sprintf("%s needs administrator rights (sudo), and %s. %s", e.Step, e.Reason, grantAdministratorAdvice())
}

func grantAdministratorAdvice() string {
	name := "<user>"
	if current, err := user.Current(); err == nil && current.Username != "" {
		name = current.Username
	}
	grant := "ask an administrator to run `sudo usermod -aG sudo " + name + "` (`wheel` instead of `sudo` on Fedora and RHEL) and log in again"
	if runtime.GOOS == "darwin" {
		grant = "ask an administrator to turn on \"Allow this user to administer this computer\" for " + name + " in System Settings > Users & Groups"
	}
	return "Run it as a user with sudo rights, or as root; to grant them, " + grant + ". Then rerun the command."
}

// sudoRefusalReasons are the sudo diagnostics that mean the privileged step never ran, in the order they are tried.
var sudoRefusalReasons = []struct{ marker, reason string }{
	{"is not in the sudoers file", "this user is not allowed to use sudo"},
	{"may not run sudo", "this user is not allowed to use sudo"},
	{"not allowed to execute", "this user is not allowed to run it with sudo"},
	{"a password is required", "sudo needs a password and none could be asked here"},
	{"a terminal is required", "sudo needs a password and there is no terminal to ask it in"},
	{"no tty present", "sudo needs a password and there is no terminal to ask it in"},
	{"incorrect password", "the sudo password was not accepted"},
}

// sudoRefusal returns an *AdministratorRequiredError (as an authored failure) when runErr, a failed privileged
// child, was sudo refusing or missing; it returns nil for every other failure so those keep their own message.
func sudoRefusal(step string, runErr error, stderr string) error {
	if runErr == nil {
		return nil
	}
	var started *exec.Error
	if errors.As(runErr, &started) && errors.Is(started.Err, fs.ErrNotExist) {
		return refusal(step, "sudo is not installed on this machine", runErr)
	}
	lower := strings.ToLower(stderr)
	for _, known := range sudoRefusalReasons {
		if strings.Contains(lower, known.marker) {
			return refusal(step, known.reason, runErr)
		}
	}
	return nil
}

func refusal(step, reason string, runErr error) error {
	typed := &AdministratorRequiredError{Step: step, Reason: reason}
	return runtimeFailure(typed.Error(), typed, runErr)
}
