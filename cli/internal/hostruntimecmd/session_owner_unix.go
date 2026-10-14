//go:build linux || darwin

package hostruntimecmd

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"io"
	"os"
)

func runSessionOwner(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) (resultErr error) {
	generation, _, err := sessionOwnerInvocation(args, stderr)
	if err != nil {
		return err
	}
	d, err := readSessionOwnerDescriptor(os.Getenv("PAPERBOAT_SESSION_OWNER_CATALOG"), generation)
	if err != nil {
		return err
	}
	if d.UID != os.Geteuid() || d.GID != os.Getegid() {
		return sessionowner.ErrInvalid
	}
	notifier, err := service.NewProcessNotifier()
	if err != nil {
		return err
	}
	if err := notifier.Starting(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, notifier.Stopping()) }()
	return runIndependentSessions(ctx, d, os.Getenv("PAPERBOAT_SESSION_OWNER_CATALOG"), notifier.Ready, notifier.Watchdog, notifier.WatchdogInterval())
}
