package containers

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pinksaucepasta/paperboat-relay/selfhost"
)

// Serve locks the container lifecycle before the shared claim supervisor can
// launch anything, preventing a second invocation from replacing live children.
func Serve(ctx context.Context, dir string, out io.Writer, observe func(context.Context, string, error)) error {
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
	images, err := LoadImages(dir)
	if err != nil {
		return err
	}
	return selfhost.RunWithLauncher(ctx, dir, &Launcher{StateDir: dir, Images: images}, out, observe)
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
	owner := installationOwner(dir)
	for _, role := range []string{"relay", "tunnel"} {
		if err := removeOwned(ctx, "paperboat-pbh-"+owner+"-"+role, owner); err != nil {
			return err
		}
	}
	return nil
}
