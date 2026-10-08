//go:build windows

package managedssh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

var ErrOpenSSHExecution = errors.New("OpenSSH execution request is invalid")

type OpenSSHExecutor struct{}

// Execute runs the native OpenSSH client with inherited standard streams and
// returns its exact process error, including the native exit code.
func (OpenSSHExecutor) Execute(ctx context.Context, executable string, arguments, environment []string) error {
	if ctx == nil || !validProcessValues(arguments) || !validEnvironment(environment) {
		return ErrOpenSSHExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := resolveOpenSSHExecutable(executable)
	if err != nil {
		return NativeLaunchError{Err: errors.Join(ErrOpenSSHExecution, err)}
	}
	command := exec.Command(path, arguments...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if environment != nil {
		command.Env = append([]string(nil), environment...)
	}
	// Reuse the existing native OpenSSH job owner; cancellation and normal exit
	// retire its descendants without altering the inherited console.
	process, err := startOpenSSHLoopbackProcess(command)
	if err != nil {
		return NativeLaunchError{Err: err}
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = process.Kill()
		err = <-done
	}
	if err == nil {
		return nil
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return NativeExitError{Code: exited.ExitCode(), Err: err}
	}
	return NativeLaunchError{Err: err}
}

func validProcessValues(values []string) bool {
	if len(values) == 0 || len(values) > 4096 {
		return false
	}
	for _, value := range values {
		if strings.ContainsRune(value, 0) || len(value) > 1<<20 {
			return false
		}
	}
	return true
}

func validEnvironment(values []string) bool {
	if values == nil {
		return true
	}
	if len(values) > 16384 {
		return false
	}
	for _, value := range values {
		if strings.ContainsRune(value, 0) || len(value) > 1<<20 {
			return false
		}
		// cmd.exe maintains one hidden current-directory entry per active
		// drive. GetEnvironmentStringsW, and therefore os.Environ, preserves
		// entries such as "=C:=C:\\Users\\Pujan". They are required input to
		// CreateProcess and are the only leading-equals form accepted here.
		if strings.HasPrefix(value, "=") {
			if !validDriveCurrentDirectoryEnvironment(value) {
				return false
			}
			continue
		}
		name, _, ok := strings.Cut(value, "=")
		if !ok || name == "" || strings.ContainsRune(name, '=') {
			return false
		}
	}
	return true
}

func validDriveCurrentDirectoryEnvironment(value string) bool {
	// The exact Windows pseudo-variable is =<drive>:=<absolute same-drive path>.
	if len(value) < 7 || value[0] != '=' || !asciiLetter(value[1]) || value[2] != ':' || value[3] != '=' {
		return false
	}
	directory := value[4:]
	return len(directory) >= 3 && strings.EqualFold(directory[:2], value[1:3]) && (directory[2] == '\\' || directory[2] == '/')
}

func asciiLetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}
