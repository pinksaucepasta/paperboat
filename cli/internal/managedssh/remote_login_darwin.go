//go:build darwin

package managedssh

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// SystemRemoteLoginDenied asks the running macOS whether Remote Login refuses user.
func SystemRemoteLoginDenied(user string) bool {
	return RemoteLoginDenied(user, func(name string, args ...string) (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := exec.CommandContext(ctx, name, args...).Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0, nil
		case errors.As(err, &exit):
			return exit.ExitCode(), nil
		}
		return -1, err
	})
}
