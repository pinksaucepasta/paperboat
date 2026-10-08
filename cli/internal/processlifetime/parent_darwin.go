//go:build darwin

package processlifetime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"golang.org/x/sys/unix"
)

// ArmParentDeath registers an exact process-exit event for the current parent.
func ArmParentDeath(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	parent := unix.Getppid()
	if parent <= 1 {
		return ErrParentUnavailable
	}
	queue, err := unix.Kqueue()
	if err != nil {
		return err
	}
	change := unix.Kevent_t{Ident: uint64(parent), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(queue, []unix.Kevent_t{change}, nil, nil); err != nil {
		return closeQueueAfterSetupFailure(queue, fmt.Errorf("register parent exit watch: %w", err))
	}
	if unix.Getppid() != parent {
		return closeQueueAfterSetupFailure(queue, ErrParentUnavailable)
	}
	startParentWatch(ctx, queue, parent)
	return nil
}

func closeQueueAfterSetupFailure(queue int, cause error) error {
	if err := unix.Close(queue); err != nil {
		return errors.Join(cause, fmt.Errorf("close parent exit watch: %w", err))
	}
	return cause
}

func startParentWatch(ctx context.Context, queue, parent int) {
	go watchParentExit(context.WithoutCancel(ctx), queue, parent)
}

func watchParentExit(ctx context.Context, queue, parent int) {
	events := make([]unix.Kevent_t, 1)
	var failure error
	for {
		count, err := unix.Kevent(queue, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			failure = fmt.Errorf("wait for parent exit: %w", err)
			break
		}
		if count != 1 {
			failure = errUnexpectedParentWatchEvent
			break
		}
		event := events[0]
		if event.Flags&unix.EV_ERROR != 0 {
			if event.Data == int64(unix.EINTR) {
				continue
			}
			if event.Data == 0 {
				failure = errUnexpectedParentWatchEvent
			} else {
				failure = fmt.Errorf("parent exit watch failed: %w", syscall.Errno(event.Data))
			}
			break
		}
		if event.Filter != unix.EVFILT_PROC || event.Ident != uint64(parent) || event.Fflags&unix.NOTE_EXIT == 0 {
			failure = errUnexpectedParentWatchEvent
		}
		break
	}
	if err := unix.Close(queue); err != nil {
		closeErr := fmt.Errorf("close parent exit watch: %w", err)
		if failure == nil {
			failure = closeErr
		} else {
			failure = errors.Join(failure, closeErr)
		}
	}
	if failure != nil {
		errorreport.Current().ObserveFailure(ctx, "paperboat-cli", "ssh", "lifecycle", "managed_ssh_failed", failure)
	}
	if err := unix.Kill(os.Getpid(), unix.SIGTERM); err != nil {
		// A process without parent supervision must not continue running.
		os.Exit(1)
	}
}
