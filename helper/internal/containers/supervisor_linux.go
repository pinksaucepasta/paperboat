package containers

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
)

// Serve locks the container lifecycle before the shared claim supervisor can
// launch anything, preventing a second invocation from replacing live children.
func Serve(ctx context.Context, dir string, out io.Writer, observe func(context.Context, string, error)) (result error) {
	if !filepath.IsAbs(dir) {
		return errors.New("installation directory must be absolute")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "container-owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("this installation already has a runtime owner")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := ClearHandoff(dir); err != nil {
		return err
	}
	images, err := LoadImages(dir)
	if err != nil {
		return err
	}
	config, err := selfhost.ReadConfig(dir)
	if err != nil {
		return err
	}
	defer func() {
		if ctx.Err() == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		result = errors.Join(result, removeInstallationRuntimes(cleanup, dir, true))
	}()
	launcher := &Launcher{StateDir: dir, Images: images, ReadOnlyFiles: []string{config.ControlCA, config.PublicHTTPSCertFile, config.PublicHTTPSKeyFile}}
	return selfhost.RunWithLauncher(ctx, dir, launcher, out, observe)
}

// Teardown requires the same exclusive lifecycle ownership as Serve, and
// removes only containers labelled for this exact installation.
func Teardown(ctx context.Context, dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("installation directory must be absolute")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "container-owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("stop the self-host runtime owner before teardown")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return removeInstallationRuntimes(ctx, dir, false)
}

func removeInstallationRuntimes(ctx context.Context, dir string, handoff bool) error {
	owner := installationOwner(dir)
	for _, role := range []string{"relay", "tunnel"} {
		name := "paperboat-pbh-" + owner + "-" + role
		if handoff {
			info, exists, err := inspectContainer(ctx, name)
			if err != nil {
				return err
			}
			if exists && info.Owner == owner && preservesHandoff(dir, role, info.ImageID) {
				continue
			}
		}
		if err := removeOwned(ctx, name, owner); err != nil {
			return err
		}
	}
	return nil
}
